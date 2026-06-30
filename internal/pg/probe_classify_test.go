package pg

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/dump"
)

func probeTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// fakePsqlBin writes an executable stand-in for psql that ignores its args and
// runs body, so Probe's post-dial classification can be exercised without a
// real PostgreSQL server.
func fakePsqlBin(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakepsql")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Probe separates a connect failure from an auth failure by dialing the host
// first: a refused dial is FailConnect, while a successful dial followed by a
// non-zero psql exit is FailAuth. Both arms are asserted against a real
// loopback listener and a fake psql binary.
func TestProbeClassifiesConnectVsAuth(t *testing.T) {
	t.Run("dial ok and non-zero psql exit is auth", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		_, portStr, _ := net.SplitHostPort(l.Addr().String())
		port, _ := strconv.Atoi(portStr)

		tool := New("/secrets/.pgpass", 5*time.Second)
		tool.psqlBin = fakePsqlBin(t, "#!/bin/sh\necho 'FATAL: password authentication failed' >&2\nexit 1\n")

		major, kind, perr := tool.Probe(probeTestCtx(t), dump.Conn{Host: "127.0.0.1", Port: port, DBName: "db", User: "u"})
		if kind != dump.FailAuth {
			t.Fatalf("Probe kind = %v, want FailAuth (dial succeeded, psql exited non-zero)", kind)
		}
		if perr == nil {
			t.Fatal("Probe err = nil, want a non-nil auth error")
		}
		if major != 0 {
			t.Errorf("Probe serverMajor = %d, want 0 on an auth failure", major)
		}
		if !strings.Contains(perr.Error(), "password authentication failed") {
			t.Errorf("Probe err = %q, want it to surface the bounded psql stderr tail", perr.Error())
		}
	})

	t.Run("refused dial is connect", func(t *testing.T) {
		// Bind then close to obtain a port that is almost certainly free, so
		// the subsequent dial is refused rather than timing out.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		_, portStr, _ := net.SplitHostPort(l.Addr().String())
		port, _ := strconv.Atoi(portStr)
		_ = l.Close()

		tool := New("/secrets/.pgpass", 5*time.Second)
		tool.psqlBin = fakePsqlBin(t, "#!/bin/sh\nexit 0\n") // must never run on a refused dial

		_, kind, perr := tool.Probe(probeTestCtx(t), dump.Conn{Host: "127.0.0.1", Port: port, DBName: "db", User: "u"})
		if kind != dump.FailConnect {
			t.Fatalf("Probe kind = %v, want FailConnect (dial refused)", kind)
		}
		if perr == nil {
			t.Fatal("Probe err = nil, want the dial error")
		}
	})
}

// Probe's post-dial psql failure has three arms beyond the auth case the
// sibling test covers. A psql that cannot be started (vanished binary, fork
// failure) is an environment fault, not auth: Probe must surface FailNone plus
// the exec error so the orchestrator's classify maps it to ReasonOther,
// mirroring the Dump path. And a non-zero psql exit that wrote nothing to
// stderr must fall back to a synthetic "psql probe exited N" detail rather than
// an empty error string.
func TestProbePostDialPsqlFailureArms(t *testing.T) {
	t.Run("exec start failure after dial is not auth", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		_, portStr, _ := net.SplitHostPort(l.Addr().String())
		port, _ := strconv.Atoi(portStr)

		tool := New("/secrets/.pgpass", 5*time.Second)
		tool.psqlBin = filepath.Join(t.TempDir(), "no-such-psql") // never created

		_, kind, perr := tool.Probe(probeTestCtx(t), dump.Conn{Host: "127.0.0.1", Port: port, DBName: "db", User: "u"})
		if kind != dump.FailNone {
			t.Fatalf("Probe kind = %v, want FailNone (exec start failure is an environment fault, not auth)", kind)
		}
		if perr == nil {
			t.Fatal("Probe err = nil, want the exec start error surfaced")
		}
	})

	t.Run("non-zero exit with empty stderr uses exit-code fallback", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		_, portStr, _ := net.SplitHostPort(l.Addr().String())
		port, _ := strconv.Atoi(portStr)

		tool := New("/secrets/.pgpass", 5*time.Second)
		tool.psqlBin = fakePsqlBin(t, "#!/bin/sh\nexit 3\n") // non-zero, writes nothing to stderr

		_, kind, perr := tool.Probe(probeTestCtx(t), dump.Conn{Host: "127.0.0.1", Port: port, DBName: "db", User: "u"})
		if kind != dump.FailAuth {
			t.Fatalf("Probe kind = %v, want FailAuth (dial ok, psql exited non-zero)", kind)
		}
		if perr == nil || perr.Error() != "psql probe exited 3" {
			t.Fatalf("Probe err = %v, want %q (empty-stderr fallback)", perr, "psql probe exited 3")
		}
	})
}

// On a successful probe Probe reads server_version_num and reports
// version_mismatch only when the shipped client major is older than the
// server. Both arms are exercised with a fake pg_dump (client major) and a fake
// psql (server_version_num) so no real PostgreSQL server is needed.
func TestProbeVersionClassification(t *testing.T) {
	t.Run("server newer than client is version_mismatch", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		_, portStr, _ := net.SplitHostPort(l.Addr().String())
		port, _ := strconv.Atoi(portStr)

		tool := New("/secrets/.pgpass", 5*time.Second)
		tool.dumpBin = fakePsqlBin(t, "#!/bin/sh\necho 'pg_dump (PostgreSQL) 15.2'\n") // client major 15
		tool.psqlBin = fakePsqlBin(t, "#!/bin/sh\necho 170004\n")                      // server major 17

		major, kind, perr := tool.Probe(probeTestCtx(t), dump.Conn{Host: "127.0.0.1", Port: port, DBName: "db", User: "u"})
		if kind != dump.FailVersion {
			t.Fatalf("Probe kind = %v, want FailVersion (server 17 > client 15)", kind)
		}
		if major != 17 {
			t.Errorf("Probe serverMajor = %d, want 17", major)
		}
		// The version-mismatch arm returns a non-nil error carrying the version
		// gap so finish() logs the actionable "client N < server M" for the operator.
		if perr == nil {
			t.Fatal("Probe err = nil, want the version-gap error surfaced for the operator log")
		}
		if !strings.Contains(perr.Error(), "client major 15 older than server major 17") {
			t.Errorf("Probe err = %q, want it to carry the client-older-than-server version gap", perr.Error())
		}
	})

	t.Run("server not newer than client is success", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		_, portStr, _ := net.SplitHostPort(l.Addr().String())
		port, _ := strconv.Atoi(portStr)

		tool := New("/secrets/.pgpass", 5*time.Second)
		tool.dumpBin = fakePsqlBin(t, "#!/bin/sh\necho 'pg_dump (PostgreSQL) 18.0'\n") // client major 18
		tool.psqlBin = fakePsqlBin(t, "#!/bin/sh\necho 180000\n")                      // server major 18

		major, kind, perr := tool.Probe(probeTestCtx(t), dump.Conn{Host: "127.0.0.1", Port: port, DBName: "db", User: "u"})
		if kind != dump.FailNone {
			t.Fatalf("Probe kind = %v, want FailNone (server 18 == client 18)", kind)
		}
		if major != 18 {
			t.Errorf("Probe serverMajor = %d, want 18", major)
		}
		if perr != nil {
			t.Errorf("Probe err = %v, want nil on success", perr)
		}
	})
}

// A context timeout while psql is mid-flight must classify as a context error
// (so classify maps it to timeout/killed), never a spurious auth_error. The
// fake psql execs sleep so the ctx kill closes the stdout pipe promptly.
func TestProbeCtxTimeoutNotAuth(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, portStr, _ := net.SplitHostPort(l.Addr().String())
	port, _ := strconv.Atoi(portStr)

	tool := New("/secrets/.pgpass", 5*time.Second)
	tool.psqlBin = fakePsqlBin(t, "#!/bin/sh\nexec sleep 10\n") // outlives the ctx deadline; exec so kill closes the pipe

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, kind, perr := tool.Probe(ctx, dump.Conn{Host: "127.0.0.1", Port: port, DBName: "db", User: "u"})
	if kind != dump.FailNone {
		t.Fatalf("Probe kind = %v, want FailNone (a ctx timeout must not classify as auth)", kind)
	}
	if !errors.Is(perr, context.DeadlineExceeded) {
		t.Fatalf("Probe err = %v, want context.DeadlineExceeded", perr)
	}
}

// VerifyTOC rejects a non-zero pg_restore --list exit (a corrupt or
// non-archive file), the secondary verify-before-replace guard: a readable
// TOC (exit zero) is nil, a non-zero exit surfaces the bounded pg_restore
// stderr tail, and an empty stderr falls back to a synthetic "pg_restore
// --list exited N". Exercised with a fake restoreBin, so no real pg_restore
// or archive fixture is needed (the same technique as the Probe tests).
func TestVerifyTOCExitClassification(t *testing.T) {
	t.Run("readable toc (exit zero) is nil", func(t *testing.T) {
		tool := New("/secrets/.pgpass", 5*time.Second)
		tool.restoreBin = fakePsqlBin(t, "#!/bin/sh\nexit 0\n")
		if err := tool.VerifyTOC(probeTestCtx(t), "ignored-path"); err != nil {
			t.Fatalf("VerifyTOC(readable toc) = %v, want nil", err)
		}
	})

	t.Run("non-zero exit is rejected and carries the stderr tail", func(t *testing.T) {
		tool := New("/secrets/.pgpass", 5*time.Second)
		tool.restoreBin = fakePsqlBin(t, "#!/bin/sh\necho 'pg_restore: error: did not find magic string in file header' >&2\nexit 1\n")

		err := tool.VerifyTOC(probeTestCtx(t), "ignored-path")
		if err == nil {
			t.Fatal("VerifyTOC(corrupt archive) = nil, want a non-nil error so the dump is discarded")
		}
		if !strings.Contains(err.Error(), "did not find magic string in file header") {
			t.Errorf("VerifyTOC err = %q, want it to carry the bounded pg_restore stderr tail", err.Error())
		}
	})

	t.Run("non-zero exit with empty stderr uses the exit-code fallback", func(t *testing.T) {
		tool := New("/secrets/.pgpass", 5*time.Second)
		tool.restoreBin = fakePsqlBin(t, "#!/bin/sh\nexit 4\n")

		err := tool.VerifyTOC(probeTestCtx(t), "ignored-path")
		if err == nil || err.Error() != "pg_restore --list exited 4" {
			t.Fatalf("VerifyTOC err = %v, want %q (empty-stderr fallback)", err, "pg_restore --list exited 4")
		}
	})
}

// TestVerifyTOCRunErrorMissingBinary covers VerifyTOC's run-error arm: when
// pg_restore cannot be started (vanished binary / fork failure) run() returns a
// non-ExitError, so VerifyTOC returns that exec error directly, distinct from
// the non-zero-exit classification the exit-code arms exercise.
// Pointing restoreBin at a non-existent path reaches it without a real
// pg_restore -- the same fake-binary technique the Probe exec-start test uses.
func TestVerifyTOCRunErrorMissingBinary(t *testing.T) {
	tool := New("/secrets/.pgpass", 5*time.Second)
	tool.restoreBin = filepath.Join(t.TempDir(), "no-such-pg_restore") // never created

	err := tool.VerifyTOC(probeTestCtx(t), "ignored-path")
	if err == nil {
		t.Fatal("VerifyTOC(missing restore binary) = nil, want the exec start error surfaced")
	}
}

func TestBinariesPresent(t *testing.T) {
	t.Run("missing binaries on PATH errors", func(t *testing.T) {
		t.Setenv("PATH", "")
		if err := BinariesPresent(); err == nil {
			t.Fatal("BinariesPresent() = nil with empty PATH, want a missing-binary error")
		}
	})
}

// Probe must NOT report version_mismatch when the client major cannot be resolved
// (clientMajorCached caches 0 on a pg_dump --version failure): a non-resolvable client
// skips the comparison rather than fabricating a mismatch.
func TestProbeUnknownClientMajorSkipsVersionCheck(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, portStr, _ := net.SplitHostPort(l.Addr().String())
	port, _ := strconv.Atoi(portStr)

	tool := New("/secrets/.pgpass", 5*time.Second)
	tool.dumpBin = filepath.Join(t.TempDir(), "no-such-pg_dump") // --version fails -> clientMajor 0
	tool.psqlBin = fakePsqlBin(t, "#!/bin/sh\necho 990000\n")    // server major 99

	major, kind, perr := tool.Probe(probeTestCtx(t), dump.Conn{Host: "127.0.0.1", Port: port, DBName: "db", User: "u"})
	if kind != dump.FailNone {
		t.Fatalf("Probe kind = %v, want FailNone (unknown client major must not fabricate a mismatch)", kind)
	}
	if perr != nil {
		t.Errorf("Probe err = %v, want nil", perr)
	}
	if major != 99 {
		t.Errorf("Probe serverMajor = %d, want 99", major)
	}
}

// Tool.Dump is the os/exec dump boundary. Its success path streams pg_dump stdout
// into w and returns exit 0; a clean non-zero exit returns the code plus the bounded
// stderr tail (no error). Exercised with a fake dumpBin -- the same technique the
// Probe/VerifyTOC tests use -- so no real pg_dump or PostgreSQL server is needed.
func TestDumpStreamsAndClassifiesExit(t *testing.T) {
	t.Run("ok streams stdout and exits zero", func(t *testing.T) {
		tool := New("/secrets/.pgpass", 5*time.Second)
		tool.dumpBin = fakePsqlBin(t, "#!/bin/sh\nprintf 'PGDMP-data'\nexit 0\n")
		var buf bytes.Buffer
		code, tail, err := tool.Dump(probeTestCtx(t), dump.Conn{Host: "h", Port: 5432, DBName: "db", User: "u"}, &buf)
		if err != nil || code != 0 {
			t.Fatalf("Dump = (%d, %q, %v), want (0, _, nil)", code, tail, err)
		}
		if buf.String() != "PGDMP-data" {
			t.Fatalf("stdout = %q, want the streamed archive bytes", buf.String())
		}
	})

	t.Run("clean non-zero exit returns code and bounded stderr tail", func(t *testing.T) {
		tool := New("/secrets/.pgpass", 5*time.Second)
		tool.dumpBin = fakePsqlBin(t, "#!/bin/sh\necho 'pg_dump: error: connection to server failed' >&2\nexit 1\n")
		var buf bytes.Buffer
		code, tail, err := tool.Dump(probeTestCtx(t), dump.Conn{Host: "h", Port: 5432, DBName: "db", User: "u"}, &buf)
		if err != nil {
			t.Fatalf("Dump err = %v, want nil for a clean non-zero exit", err)
		}
		if code != 1 || !strings.Contains(tail, "connection to server failed") {
			t.Fatalf("Dump = (%d, %q), want (1, ...connection to server failed...)", code, tail)
		}
	})
}

// A non-positive dial timeout disables the per-dial sub-timeout: the dial must
// fall back to the parent context, not a zero-length one. The guard is
// `if t.dialTimeout > 0`, so with dialTimeout == 0 the dial uses ctx directly
// and reaches a reachable server (FailNone). Were the guard to admit zero, the
// dial would run under context.WithTimeout(ctx, 0) -- expiring immediately and
// misreporting an open, healthy listener as connect_error.
func TestProbeNonPositiveDialTimeoutUsesParentContext(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, portStr, _ := net.SplitHostPort(l.Addr().String())
	port, _ := strconv.Atoi(portStr)

	tool := New("/secrets/.pgpass", 5*time.Second)
	tool.dialTimeout = 0                                         // disable the dial sub-timeout
	tool.dumpBin = filepath.Join(t.TempDir(), "no-such-pg_dump") // --version fails -> clientMajor 0 -> skip version check
	tool.psqlBin = fakePsqlBin(t, "#!/bin/sh\necho 150000\n")    // reachable server, major 15

	major, kind, perr := tool.Probe(probeTestCtx(t), dump.Conn{Host: "127.0.0.1", Port: port, DBName: "db", User: "u"})
	if kind != dump.FailNone {
		t.Fatalf("Probe kind = %v, want FailNone (a zero dial timeout must dial the open listener via the parent ctx, not expire instantly)", kind)
	}
	if perr != nil {
		t.Errorf("Probe err = %v, want nil", perr)
	}
	if major != 15 {
		t.Errorf("Probe serverMajor = %d, want 15", major)
	}
}
