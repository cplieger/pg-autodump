package pg

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/dump"
)

// The child shape (Cancel, WaitDelay, Setpgid) is shared with docker-renovate-scheduler.
func TestNewCommand(t *testing.T) {
	t.Parallel()
	cmd := newCommand(t.Context(), "sleep", "1")
	if cmd.Cancel == nil {
		t.Error("Cancel not set (graceful SIGTERM on ctx cancellation expected)")
	}
	if cmd.WaitDelay <= 0 {
		t.Errorf("WaitDelay = %v, want a positive grace window before SIGKILL escalation", cmd.WaitDelay)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid not set: the child must run in its own process group, or " +
			"a group-forwarding init above the daemon (dumb-init, tini -g) forwards the " +
			"docker-stop SIGTERM to the whole group and kills the in-flight dump, " +
			"defeating the shutdown drain")
	}
}

// Proves the OS honors Setpgid: the child's process group must differ from
// the test process's, or a group-directed SIGTERM at PID 1 would reach it.
func TestNewCommand_ChildRunsInOwnProcessGroup(t *testing.T) {
	t.Parallel()
	cmd := newCommand(t.Context(), "sleep", "2")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	childPgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid(child) failed: %v", err)
	}
	ownPgid, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatalf("Getpgid(self) failed: %v", err)
	}
	if childPgid == ownPgid {
		t.Errorf("child pgid = %d equals parent pgid; child must lead its own process group", childPgid)
	}
	if childPgid != cmd.Process.Pid {
		t.Errorf("child pgid = %d, want %d (the child should lead its own group)", childPgid, cmd.Process.Pid)
	}
}

// Cancellation must reach every member of the child's process group, not just
// the child, so a forked worker dies with its leader.
//
// The lingering member is a direct child of the test binary that joins the
// child's group, so the test observes its death at an instant it controls; a
// descendant forked inside the child would orphan at leader exit instead.
func TestNewCommand_CancelSignalsTheWholeGroup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	leader := newCommand(ctx, "sleep", "30")
	if err := leader.Start(); err != nil {
		t.Fatalf("Start(leader) failed: %v", err)
	}
	t.Cleanup(func() {
		_ = leader.Process.Kill()
		_ = leader.Wait()
	})

	member := exec.Command("sleep", "30")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
	if err := member.Start(); err != nil {
		t.Fatalf("Start(group member) failed: %v", err)
	}
	pgid, err := syscall.Getpgid(member.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid(group member) failed: %v", err)
	}
	if pgid != leader.Process.Pid {
		t.Fatalf("group member pgid = %d, want %d: the fixture never joined the child's group, "+
			"so every assertion below would pass for the wrong reason", pgid, leader.Process.Pid)
	}

	exited := make(chan error, 1)
	go func() { exited <- member.Wait() }()

	cancel()

	select {
	case err := <-exited:
		if err == nil {
			t.Errorf("group member exited cleanly (status 0), want termination by the cancel's signal")
		}
	case <-time.After(10 * time.Second):
		_ = member.Process.Kill()
		t.Error("group member outlived the cancel by 10s: the cancel reached only the child, " +
			"so a worker forked by pg_dump would survive an abandoned dump")
	}
}

func TestParseMajor(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{name: "standard pg_dump line", in: "pg_dump (PostgreSQL) 18.1", want: 18},
		{name: "debian build suffix", in: "pg_dump (PostgreSQL) 17.4 (Debian 17.4-1)", want: 17},
		{name: "bare server line", in: "PostgreSQL 16.2", want: 16},
		{name: "empty string", in: "", want: 0},
		{name: "no digit token", in: "no version here", want: 0},
		{name: "leading v prefix has no digit run", in: "v18.1", want: 0},
		{name: "whitespace padded", in: "   18.3   ", want: 18},
		{name: "all-digit token reaches end of token", in: "18", want: 18},
		{name: "leading run includes an internal zero (major 10)", in: "pg_dump (PostgreSQL) 10.4", want: 10},
		{name: "leading run includes a nine (major 9)", in: "9.6", want: 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseMajor(tt.in); got != tt.want {
				t.Errorf("parseMajor(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestBoundedBuffer(t *testing.T) {
	t.Run("under cap stores all and reports the full write", func(t *testing.T) {
		b := &boundedBuffer{max: 10}
		n, err := b.Write([]byte("abc"))
		if n != 3 || err != nil {
			t.Fatalf("Write(\"abc\") = (%d, %v), want (3, nil)", n, err)
		}
		if b.String() != "abc" {
			t.Errorf("String() = %q, want %q", b.String(), "abc")
		}
	})

	t.Run("over cap truncates content but still reports the full write", func(t *testing.T) {
		b := &boundedBuffer{max: 4}
		n, err := b.Write([]byte("abcdefgh"))
		if n != 8 || err != nil {
			t.Fatalf("Write(8 bytes) = (%d, %v), want (8, nil)", n, err)
		}
		if b.String() != "abcd" {
			t.Errorf("String() = %q, want %q (capped at max)", b.String(), "abcd")
		}
	})

	t.Run("accumulates across writes and stops at cap", func(t *testing.T) {
		b := &boundedBuffer{max: 4}
		_, _ = b.Write([]byte("ab"))
		n, _ := b.Write([]byte("cdef"))
		if n != 4 {
			t.Fatalf("second Write reported %d, want 4 (always the full input length)", n)
		}
		if b.String() != "abcd" {
			t.Errorf("String() = %q, want %q", b.String(), "abcd")
		}
	})

	t.Run("zero cap stores nothing but never blocks the writer", func(t *testing.T) {
		b := &boundedBuffer{max: 0}
		n, err := b.Write([]byte("x"))
		if n != 1 || err != nil {
			t.Fatalf("Write(\"x\") = (%d, %v), want (1, nil)", n, err)
		}
		if b.String() != "" {
			t.Errorf("String() = %q, want empty", b.String())
		}
	})
}

func TestChildEnv(t *testing.T) {
	t.Run("includes PGPASSFILE and a derived statement_timeout", func(t *testing.T) {
		env := New("/secrets/.pgpass", 5*time.Second).childEnv()
		var hasPass, hasOpts bool
		for _, e := range env {
			if e == "PGPASSFILE=/secrets/.pgpass" {
				hasPass = true
			}
			if e == "PGOPTIONS=-c statement_timeout=5000" {
				hasOpts = true
			}
		}
		if !hasPass {
			t.Errorf("childEnv() missing PGPASSFILE=/secrets/.pgpass, got %v", env)
		}
		if !hasOpts {
			t.Errorf("childEnv() missing PGOPTIONS=-c statement_timeout=5000, got %v", env)
		}
	})

	t.Run("omits PGOPTIONS when statement timeout is zero", func(t *testing.T) {
		env := New("/secrets/.pgpass", 0).childEnv()
		for _, e := range env {
			if strings.HasPrefix(e, "PGOPTIONS=") {
				t.Errorf("childEnv() set %q with a zero timeout, want no PGOPTIONS entry", e)
			}
		}
	})
}

func TestPGToolRejectsDeadlinelessContext(t *testing.T) {
	tool := New("/secrets/.pgpass", 5*time.Second)
	conn := dump.Conn{Host: "h", Port: 5432, DBName: "db", User: "u"}

	if _, _, err := tool.Dump(t.Context(), conn, io.Discard); err != ErrNoDeadline {
		t.Errorf("Dump(no-deadline ctx) err = %v, want ErrNoDeadline", err)
	}
	if err := tool.VerifyTOC(t.Context(), "ignored-path"); err != ErrNoDeadline {
		t.Errorf("VerifyTOC(no-deadline ctx) err = %v, want ErrNoDeadline", err)
	}
	if _, _, err := tool.Probe(t.Context(), conn); err != ErrNoDeadline {
		t.Errorf("Probe(no-deadline ctx) err = %v, want ErrNoDeadline", err)
	}
}

// The dial timeout is what separates a definitive connect_error from a slow host.
func TestNewConfiguresDialTimeout(t *testing.T) {
	if got := New("/secrets/.pgpass", 5*time.Second).dialTimeout; got != 5*time.Second {
		t.Errorf("New(...).dialTimeout = %v, want 5s", got)
	}
}
