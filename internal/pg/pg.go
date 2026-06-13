// Package pg is the external boundary over the pg_dump / pg_restore / psql
// command-line tools. It implements dump.PGTool so the orchestrator depends on
// the narrow interface it defines, not on os/exec directly. Every invocation
// builds an explicit []string argv (never a shell string) and runs under a
// context whose deadline is enforced here, so "every external call is bounded"
// holds at the boundary rather than by convention.
//
// The argv construction and exit-code handling follow patterns proven in
// orgrim/pg_back (BSD-2-Clause); see CREDITS. The connect-vs-auth probe
// (dial-then-psql) is original to this project.
package pg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/pg-autodump/internal/dump"
)

// ErrNoDeadline is returned by every boundary method when handed a context
// without a deadline, enforcing Property 3 (every external call is bounded) at
// the boundary instead of trusting callers.
var ErrNoDeadline = errors.New("pg: context has no deadline")

// stderrCap bounds captured pg_dump/psql stderr so a chatty tool cannot flood
// logs or the HTTP body.
const stderrCap = 2048

// Tool runs the PostgreSQL client binaries as a network client. Credentials
// flow only through the child environment (PGPASSFILE), never argv.
type Tool struct {
	dumpBin     string
	restoreBin  string
	psqlBin     string
	pgPassFile  string
	stmtTimeout time.Duration
	dialTimeout time.Duration

	clientOnce  sync.Once
	clientMajor int // resolved lazily from `pg_dump --version`; 0 if unknown
}

// New builds a Tool. pgPassFile is exported into each child as PGPASSFILE;
// stmtTimeout becomes the server-side statement_timeout (via PGOPTIONS).
func New(pgPassFile string, stmtTimeout time.Duration) *Tool {
	return &Tool{
		dumpBin:     "pg_dump",
		restoreBin:  "pg_restore",
		psqlBin:     "psql",
		pgPassFile:  pgPassFile,
		stmtTimeout: stmtTimeout,
		dialTimeout: 5 * time.Second,
	}
}

var _ dump.PGTool = (*Tool)(nil)

// BinariesPresent reports whether pg_dump, pg_restore, and psql resolve on
// PATH. The health probe calls it; a missing binary is an image/build error.
func BinariesPresent() error {
	for _, bin := range []string{"pg_dump", "pg_restore", "psql"} {
		if _, err := exec.LookPath(bin); err != nil {
			return errors.New("required binary not found on PATH: " + bin)
		}
	}
	return nil
}

// Dump streams a network custom-format pg_dump for c into w. pg_dump is a local
// child process, so ctx cancellation (cmd.Cancel via CommandContext) kills it
// directly and Postgres tears down the server backend when the client TCP
// connection drops. Returns the process exit code and a bounded stderr tail.
func (t *Tool) Dump(ctx context.Context, c dump.Conn, w io.Writer) (exitCode int, stderrTail string, err error) {
	if _, ok := ctx.Deadline(); !ok {
		return 0, "", ErrNoDeadline
	}
	args := []string{
		"--format=custom",
		"--host=" + c.Host,
		"--port=" + strconv.Itoa(c.Port),
		"--username=" + c.User,
		"--no-password",
		"--dbname=" + c.DBName,
	}
	//nolint:gosec // G204: explicit argv (no shell); host/port/user/db are validated in internal/spec.
	cmd := exec.CommandContext(ctx, t.dumpBin, args...)
	cmd.Stdout = w
	var errBuf boundedBuffer
	errBuf.max = stderrCap
	cmd.Stderr = &errBuf
	cmd.Env = t.childEnv()
	cmd.WaitDelay = 5 * time.Second

	code, runErr := run(cmd)
	return code, strings.TrimSpace(errBuf.String()), runErr
}

// VerifyTOC runs a local `pg_restore --list` against the custom-format file at
// path: no network, no daemon. A readable table-of-contents proves the file is
// structurally intact (not truncated mid-stream). Returns nil iff readable.
func (t *Tool) VerifyTOC(ctx context.Context, path string) error {
	if _, ok := ctx.Deadline(); !ok {
		return ErrNoDeadline
	}
	//nolint:gosec // G204: explicit argv (no shell); path is an internally-created temp file.
	cmd := exec.CommandContext(ctx, t.restoreBin, "--list", path)
	cmd.Stdout = io.Discard
	var errBuf boundedBuffer
	errBuf.max = 512
	cmd.Stderr = &errBuf
	if _, err := run(cmd); err != nil {
		return err
	}
	return nil
}

// Probe classifies one database before a dump is attempted. It separates
// connect from auth without matching stderr: a TCP dial that fails is a
// definitive connect_error; if the dial succeeds but the authenticated psql
// round-trip fails, the server is reachable so the fault is auth/database
// (auth_error). On success it reads server_version_num and reports
// version_mismatch when the shipped client major is older than the server.
func (t *Tool) Probe(ctx context.Context, c dump.Conn) (int, dump.FailKind, error) {
	if _, ok := ctx.Deadline(); !ok {
		return 0, dump.FailNone, ErrNoDeadline
	}

	dialCtx := ctx
	if t.dialTimeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, t.dialTimeout)
		defer cancel()
	}
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort(c.Host, strconv.Itoa(c.Port)))
	if err != nil {
		return 0, dump.FailConnect, err
	}
	_ = conn.Close()

	//nolint:gosec // G204: explicit argv (no shell); host/port/user/db are validated in internal/spec.
	cmd := exec.CommandContext(ctx, t.psqlBin,
		"--no-password", "-tAX", "-q",
		"-h", c.Host, "-p", strconv.Itoa(c.Port), "-U", c.User, "-d", c.DBName,
		"-c", "SELECT current_setting('server_version_num')")
	cmd.Env = t.childEnv()
	var outBuf, errBuf boundedBuffer
	outBuf.max = 64
	errBuf.max = stderrCap
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if _, err := run(cmd); err != nil {
		// Dial succeeded, so the host is up: the failure is authentication or
		// a missing/invalid database, not reachability.
		return 0, dump.FailAuth, err
	}

	serverNum, _ := strconv.Atoi(strings.TrimSpace(outBuf.String()))
	serverMajor := serverNum / 10000 // correct for PostgreSQL 10+ (all modern servers)
	if cm := t.clientMajorCached(ctx); cm > 0 && serverMajor > cm {
		return serverMajor, dump.FailVersion, nil
	}
	return serverMajor, dump.FailNone, nil
}

// childEnv builds the child environment: the parent env plus PGPASSFILE (so
// pg_dump/psql resolve the password from the mounted .pgpass) and PGOPTIONS
// carrying the server-side statement_timeout. No secret is ever placed here or
// on the command line.
func (t *Tool) childEnv() []string {
	env := append(os.Environ(), "PGPASSFILE="+t.pgPassFile)
	if t.stmtTimeout > 0 {
		ms := strconv.FormatInt(t.stmtTimeout.Milliseconds(), 10)
		env = append(env, "PGOPTIONS=-c statement_timeout="+ms)
	}
	return env
}

// clientMajorCached resolves the shipped pg_dump major version once, caching
// the result. On any failure it caches 0 so the version comparison is skipped
// (never a false version_mismatch).
func (t *Tool) clientMajorCached(ctx context.Context) int {
	t.clientOnce.Do(func() {
		vctx := ctx
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			vctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
		}
		//nolint:gosec // G204: static command `pg_dump --version`, no user input.
		out, err := exec.CommandContext(vctx, t.dumpBin, "--version").Output()
		if err != nil {
			return
		}
		t.clientMajor = parseMajor(string(out))
	})
	return t.clientMajor
}

// parseMajor extracts the major version from a `pg_dump (PostgreSQL) 18.1`
// version line: the first whitespace-separated token whose leading digits form
// an integer. Returns 0 when no version token is found.
func parseMajor(versionLine string) int {
	for tok := range strings.FieldsSeq(versionLine) {
		end := 0
		for end < len(tok) && tok[end] >= '0' && tok[end] <= '9' {
			end++
		}
		if end > 0 {
			n, err := strconv.Atoi(tok[:end])
			if err == nil {
				return n
			}
		}
	}
	return 0
}

// run executes cmd and returns the process exit code with a nil error, or a
// non-nil error for failures that are not a clean non-zero exit (binary not
// found, context cancellation before start). A signal-killed process (e.g. ctx
// timeout) yields an ExitError with code -1 and a nil run error; the caller
// distinguishes that case via ctx.Err().
func run(cmd *exec.Cmd) (int, error) {
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// boundedBuffer captures at most max bytes while always reporting full writes,
// so a child process is never blocked by a full stderr pipe.
type boundedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if rem := b.max - b.buf.Len(); rem > 0 {
		if len(p) <= rem {
			b.buf.Write(p)
		} else {
			b.buf.Write(p[:rem])
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }
