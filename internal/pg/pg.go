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
	"syscall"
	"time"

	"github.com/cplieger/pg-autodump/internal/dump"
	scheduler "github.com/cplieger/scheduler/v3"
)

// ErrNoDeadline is returned by every boundary method when handed a context
// without a deadline, enforcing Property 3 (every external call is bounded) at
// the boundary instead of trusting callers.
var ErrNoDeadline = errors.New("pg: context has no deadline")

// stderrCap bounds captured pg_dump/psql stderr so a chatty tool cannot flood
// logs or the HTTP body.
const stderrCap = 2048

// newCommand builds every child process this package spawns (pg_dump,
// pg_restore, psql). It is the fleet-standard child shape shared with
// docker-renovate-scheduler: the scheduler library supplies graceful
// cancellation (SIGTERM on context cancellation, then a DefaultGrace 5s
// window before os/exec escalates to SIGKILL), Setpgid puts the child in its
// OWN process group, and Cancel targets that whole group.
//
// Setpgid is the load-bearing half. PID 1 here is tini, which in its default
// mode signals only its immediate child — but a group-forwarding init
// (dumb-init, `tini -g`) would forward a docker-stop SIGTERM to the daemon's
// entire process group, TERMing an in-flight pg_dump out-of-band in the same
// instant as the daemon and silently defeating the shutdown drain
// (Guard.WaitIdle would just observe the corpse). That exact failure shipped
// to prod in docker-renovate-scheduler. With its own group the child only
// ever receives signals the daemon sends it (ctx cancellation on timeout or
// drain-budget expiry), so the drain semantics hold under ANY init above the
// daemon instead of depending on tini's forwarding default. The caller wires
// Stdout/Stderr/Env on the returned command.
//
// The group-targeted Cancel matters only if a child ever has group members:
// pg_dump --format=custom is single-process today, so group-SIGTERM equals
// child-SIGTERM — but a future parallel dump (--format=directory --jobs=N)
// forks workers, and a direct-child Cancel would silently under-kill them.
// Aligning with renovate's group Cancel now closes that latent divergence.
//
// Fleet alignment note (l-f3 audit, 2026-07): this own-process-group wrapper
// stays deliberately app-side — the scheduler library gains a
// WithProcessGroup() option only when a THIRD consumer of the shape appears
// (scheduler.md, "Setpgid pairing rule"). Copies to keep line-aligned when
// editing either: docker-renovate-scheduler runner.go defaultCommandRunner
// (superset — adds stdio streaming and post-run group sweep/probe/drain,
// because its child provably spawns package-manager descendants) and this
// one. vibekit internal/auth login_proc_unix.go carries the group half only
// (hard SIGKILL on timeout, no scheduler dep — a deliberate non-copy).
var newCommand scheduler.CommandRunner = func() scheduler.CommandRunner {
	base := scheduler.NewCommandRunner(scheduler.DefaultGrace)
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := base(ctx, name, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			// Signal the child's whole process group (Setpgid makes it the
			// leader), so any forked worker stops with it.
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return cmd
	}
}()

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

// requiredBins are the PostgreSQL client binaries the image must ship and the
// health preflight verifies; New() and BinariesPresent share this single list so
// the two can never drift.
var requiredBins = []string{"pg_dump", "pg_restore", "psql"}

// New builds a Tool. pgPassFile is exported into each child as PGPASSFILE;
// stmtTimeout becomes the server-side statement_timeout (via PGOPTIONS).
func New(pgPassFile string, stmtTimeout time.Duration) *Tool {
	return &Tool{
		dumpBin:     requiredBins[0],
		restoreBin:  requiredBins[1],
		psqlBin:     requiredBins[2],
		pgPassFile:  pgPassFile,
		stmtTimeout: stmtTimeout,
		dialTimeout: 5 * time.Second,
	}
}

var _ dump.PGTool = (*Tool)(nil)

// BinariesPresent reports whether pg_dump, pg_restore, and psql resolve on
// PATH. The health probe calls it; a missing binary is an image/build error.
func BinariesPresent() error {
	for _, bin := range requiredBins {
		if _, err := exec.LookPath(bin); err != nil {
			return errors.New("required binary not found on PATH: " + bin)
		}
	}
	return nil
}

// Dump streams a network custom-format pg_dump for c into w. pg_dump is a
// local child process, so ctx cancellation reaches it directly (SIGTERM, then
// the runner's grace window — see newCommand) and Postgres tears down the
// server backend when the client TCP connection drops. Returns the process
// exit code and a bounded stderr tail.
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
	cmd := newCommand(ctx, t.dumpBin, args...)
	cmd.Stdout = w
	var errBuf boundedBuffer
	errBuf.max = stderrCap
	cmd.Stderr = &errBuf
	cmd.Env = t.childEnv()

	code, runErr := run(cmd)
	return code, strings.TrimSpace(errBuf.String()), runErr
}

// VerifyTOC runs a local `pg_restore --list` against the custom-format file at
// path: no network, no daemon. It reads the archive header and table of
// contents at the front of the archive, confirming the file is a well-formed
// custom-format archive; it does NOT re-read the trailing data section, so it
// is a structural check, not a full-restore validation. Data-stream
// completeness is guaranteed by pg_dump's exit code (gated before this check
// in stageAndReplace); VerifyTOC is the secondary guard that rejects a
// non-archive or header-truncated file. Returns nil iff the TOC is readable.
func (t *Tool) VerifyTOC(ctx context.Context, path string) error {
	if _, ok := ctx.Deadline(); !ok {
		return ErrNoDeadline
	}
	cmd := newCommand(ctx, t.restoreBin, "--list", path)
	cmd.Stdout = io.Discard
	var errBuf boundedBuffer
	errBuf.max = 512
	cmd.Stderr = &errBuf
	code, err := run(cmd)
	if err != nil {
		return err
	}
	if code != 0 {
		detail := strings.TrimSpace(errBuf.String())
		if detail == "" {
			detail = "pg_restore --list exited " + strconv.Itoa(code)
		}
		return errors.New(detail)
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

	cmd := newCommand(ctx, t.psqlBin,
		"--no-password", "-tAX", "-q",
		"-h", c.Host, "-p", strconv.Itoa(c.Port), "-U", c.User, "-d", c.DBName,
		"-c", "SELECT current_setting('server_version_num')")
	cmd.Env = t.childEnv()
	var outBuf, errBuf boundedBuffer
	outBuf.max = 64
	errBuf.max = stderrCap
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	code, rerr := run(cmd)
	if rerr != nil || code != 0 {
		// A ctx timeout/cancel killed psql: report it as a context error so
		// classify() maps it to timeout/killed, never a spurious auth_error.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, dump.FailNone, ctxErr
		}
		// psql could not be started (fork failure, vanished binary): an
		// environment fault, not auth. Surface the exec error so classify()
		// maps it to ReasonOther, mirroring the Dump path's exit-0-with-error branch.
		if rerr != nil {
			return 0, dump.FailNone, rerr
		}
		// Dial succeeded => host is up; a non-zero psql exit is auth / missing
		// database, not reachability. Surface the bounded psql stderr.
		detail := strings.TrimSpace(errBuf.String())
		if detail == "" {
			detail = "psql probe exited " + strconv.Itoa(code)
		}
		return 0, dump.FailAuth, errors.New(detail)
	}

	serverNum, _ := strconv.Atoi(strings.TrimSpace(outBuf.String()))
	serverMajor := serverNum / 10000 // correct for PostgreSQL 10+ (all modern servers)
	if cm := t.clientMajorCached(ctx); cm > 0 && serverMajor > cm {
		return serverMajor, dump.FailVersion,
			errors.New("client major " + strconv.Itoa(cm) + " older than server major " +
				strconv.Itoa(serverMajor) + " (bump the pg-autodump image)")
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
// (never a false version_mismatch). Its sole caller (Probe) has already
// enforced that ctx carries a deadline, so the version exec is bounded by it.
func (t *Tool) clientMajorCached(ctx context.Context) int {
	t.clientOnce.Do(func() {
		out, err := newCommand(ctx, t.dumpBin, "--version").Output()
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
