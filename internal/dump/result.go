package dump

import (
	"context"
	"errors"
	"io"
	"time"
)

// Conn is the network coordinates of one database. It carries no password:
// libpq resolves credentials from .pgpass keyed on host:port:dbname:user.
type Conn struct {
	Host   string
	DBName string
	User   string
	Port   int
}

// FailKind is a typed classification the pg boundary returns from Probe (and
// from a connection-phase Dump failure) so the orchestrator can map a failure
// to a Reason structurally, never by substring-matching stderr.
type FailKind int

const (
	// FailNone means no boundary-classified failure occurred.
	FailNone FailKind = iota
	// FailConnect means the host was unreachable or refused the connection.
	FailConnect
	// FailAuth means authentication failed (bad/missing .pgpass entry or role).
	FailAuth
	// FailVersion means the client pg_dump major is older than the server major.
	FailVersion
)

// PGTool is the external boundary the orchestrator depends on. The concrete
// implementation lives in internal/pg; tests supply a fake. Every method is
// context-bounded and the implementation enforces that a deadline is present.
type PGTool interface {
	// Probe performs a TCP dial followed by an authenticated round-trip to
	// classify connect/auth/version failures and report the server major version.
	Probe(ctx context.Context, c Conn) (serverMajor int, kind FailKind, err error)

	// Dump streams a network custom-format pg_dump for c to w and returns the
	// process exit code and a bounded tail of stderr. pg_dump runs as a local
	// child process, so ctx cancellation terminates it directly.
	Dump(ctx context.Context, c Conn, w io.Writer) (exitCode int, stderrTail string, err error)

	// VerifyTOC runs a local `pg_restore --list` against the custom-format
	// file at path. It needs no network and no daemon. Returns nil iff the
	// table-of-contents header is readable.
	VerifyTOC(ctx context.Context, path string) error
}

// Reason is the closed dump-result taxonomy.
type Reason string

// ReasonOK and the following constants are the valid Reason values for a dump result.
const (
	ReasonOK              Reason = "ok"
	ReasonEmpty           Reason = "empty"
	ReasonTruncated       Reason = "truncated"
	ReasonTimeout         Reason = "timeout"
	ReasonKilled          Reason = "killed"
	ReasonPGError         Reason = "pg_error"
	ReasonConnectError    Reason = "connect_error"
	ReasonAuthError       Reason = "auth_error"
	ReasonVersionMismatch Reason = "version_mismatch"
	ReasonOther           Reason = "other"
	ReasonDuplicate       Reason = "duplicate"
	ReasonInvalid         Reason = "invalid"
	ReasonMkdirFailed     Reason = "mkdir_failed"
	ReasonRenameFailed    Reason = "rename_failed"
	ReasonSkipped         Reason = "skipped"
)

// Result is one database's outcome from a dump run.
type Result struct {
	Host          string
	DBName        string
	Reason        Reason
	Detail        string // human line for the HTTP body, e.g. "ok (4823104 bytes)"
	Bytes         int64
	ServerVersion int // server major version from the probe (server_version_num/10000), 0 if unknown
	Duration      time.Duration
}

// OK reports whether the dump succeeded.
func (r *Result) OK() bool { return r.Reason == ReasonOK }

// BodyDetail is the operator-facing line for the POST /dump (and `trigger`)
// response body. For the execution-tool failure reasons whose Detail carries a
// raw pg_dump/pg_restore stderr tail (pg_error, truncated, other), it returns
// only the reason word: that stderr can echo schema/object/role names and error
// text, and the endpoint may run open (no AUTH_TOKEN), so the full detail is
// kept to the logs (the orchestrator's per-result log line records r.Detail) and
// out of a body any reachable client could read. Every other reason returns its
// Detail (e.g. "ok (4823104 bytes)", a validation reason, "connect_error"),
// falling back to the reason word when Detail is empty.
func (r *Result) BodyDetail() string {
	switch r.Reason {
	case ReasonPGError, ReasonTruncated, ReasonOther:
		return string(r.Reason)
	}
	if r.Detail == "" {
		return string(r.Reason)
	}
	return r.Detail
}

// classify maps a pg_dump exit code, a context error, and a typed FailKind
// from the boundary to a Reason. Order matters: a cancelled or timed-out
// context wins over any exit code, and a boundary-classified failure
// (connect/auth/version) wins over a generic non-zero exit. No stderr
// substring matching is ever used.
func classify(exitCode int, ctxErr error, kind FailKind) Reason {
	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return ReasonTimeout
	case errors.Is(ctxErr, context.Canceled):
		return ReasonKilled
	}
	switch kind {
	case FailConnect:
		return ReasonConnectError
	case FailAuth:
		return ReasonAuthError
	case FailVersion:
		return ReasonVersionMismatch
	case FailNone:
		// fall through to exit-code handling
	}
	if exitCode != 0 {
		return ReasonPGError
	}
	return ReasonOther
}
