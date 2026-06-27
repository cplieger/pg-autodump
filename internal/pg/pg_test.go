package pg

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/dump"
)

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

	if _, _, err := tool.Dump(context.Background(), conn, io.Discard); err != ErrNoDeadline {
		t.Errorf("Dump(no-deadline ctx) err = %v, want ErrNoDeadline", err)
	}
	if err := tool.VerifyTOC(context.Background(), "ignored-path"); err != ErrNoDeadline {
		t.Errorf("VerifyTOC(no-deadline ctx) err = %v, want ErrNoDeadline", err)
	}
	if _, _, err := tool.Probe(context.Background(), conn); err != ErrNoDeadline {
		t.Errorf("Probe(no-deadline ctx) err = %v, want ErrNoDeadline", err)
	}
}
