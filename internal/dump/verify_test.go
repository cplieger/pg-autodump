package dump

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestStageAndReplace(t *testing.T) {
	const dbname = "app"
	tests := []struct {
		name       string
		pg         *fakePG
		wantReason Reason
		wantFile   bool // should <dbname>.dump exist with new content afterward
	}{
		{
			name: "ok writes and replaces",
			pg: &fakePG{dump: func(_ context.Context, _ Conn, w io.Writer) (int, string, error) {
				_, _ = io.WriteString(w, "newdump")
				return 0, "", nil
			}},
			wantReason: ReasonOK,
			wantFile:   true,
		},
		{
			name:       "empty dump is rejected",
			pg:         &fakePG{dump: func(_ context.Context, _ Conn, _ io.Writer) (int, string, error) { return 0, "", nil }},
			wantReason: ReasonEmpty,
		},
		{
			name: "truncated dump is rejected",
			pg: &fakePG{
				dump: func(_ context.Context, _ Conn, w io.Writer) (int, string, error) {
					_, _ = io.WriteString(w, "partial")
					return 0, "", nil
				},
				verify: func(_ context.Context, _ string) error { return errors.New("TOC unreadable") },
			},
			wantReason: ReasonTruncated,
		},
		{
			name:       "non-zero exit is pg_error",
			pg:         &fakePG{dump: func(_ context.Context, _ Conn, _ io.Writer) (int, string, error) { return 1, "FATAL: nope", nil }},
			wantReason: ReasonPGError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, dbname+".dump")
			const known = "KNOWN-GOOD"
			if err := os.WriteFile(target, []byte(known), 0o600); err != nil {
				t.Fatal(err)
			}

			res := stageAndReplace(deadlineCtx(t), tt.pg, dir, dbname+".dump", Conn{Host: "h", Port: 5432, DBName: dbname, User: "u"})
			if res.Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q (detail %q)", res.Reason, tt.wantReason, res.Detail)
			}

			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("target missing: %v", err)
			}
			if tt.wantFile {
				if string(got) == known {
					t.Fatal("target was not replaced on success")
				}
			} else if string(got) != known {
				// A failed dump never overwrites the known-good file.
				t.Fatalf("known-good backup was clobbered on failure: got %q", got)
			}
		})
	}
}

func TestStageAndReplaceContextTimeout(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	pg := &fakePG{dump: func(_ context.Context, _ Conn, w io.Writer) (int, string, error) {
		cancel() // simulate the run being cancelled mid-dump
		_, _ = io.WriteString(w, "partial")
		return 0, "", nil
	}}
	res := stageAndReplace(ctx, pg, dir, "app.dump", Conn{Host: "h", Port: 5432, DBName: "app", User: "u"})
	if res.Reason != ReasonKilled {
		t.Fatalf("reason = %q, want killed", res.Reason)
	}
}

// A context cancelled after a successful verify -- so the cancel lands on
// pending.Commit -- classifies as killed/timeout, not rename_failed. atomicfile's
// Commit checks ctx.Err() at the top of its temp-side barrier and returns a
// context-wrapped error on cancel; the Commit branch must treat that as a ctx
// cancel rather than a filesystem rename fault (which would send an operator
// chasing a phantom disk/permissions problem during what was really a
// timeout/shutdown).
func TestStageAndReplaceCommitContextCancel(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	pg := &fakePG{
		dump: func(_ context.Context, _ Conn, w io.Writer) (int, string, error) {
			_, _ = io.WriteString(w, "complete-archive")
			return 0, "", nil
		},
		verify: func(_ context.Context, _ string) error {
			cancel() // cancel after a clean verify so the cancel lands on Commit
			return nil
		},
	}

	res := stageAndReplace(ctx, pg, dir, "app.dump", Conn{Host: "h", Port: 5432, DBName: "app", User: "u"})

	if res.Reason != ReasonKilled {
		t.Fatalf("reason = %q, want killed (a cancel during Commit must not be reported as rename_failed)", res.Reason)
	}
}

// A context cancel landing while VerifyTOC runs must classify as killed/timeout,
// not truncated: the run was aborted, the staged file is not proven corrupt. The
// discriminator sub-test pins that a verify error under a live context still
// classifies as truncated, so only the cancel branch flips the reason.
func TestStageAndReplaceVerifyContextCancel(t *testing.T) {
	t.Run("cancel during verify is killed not truncated", func(t *testing.T) {
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		pg := &fakePG{
			dump: func(_ context.Context, _ Conn, w io.Writer) (int, string, error) {
				_, _ = io.WriteString(w, "complete-archive")
				return 0, "", nil
			},
			verify: func(_ context.Context, _ string) error {
				cancel()
				return errors.New("verify aborted")
			},
		}

		res := stageAndReplace(ctx, pg, dir, "app.dump", Conn{Host: "h", Port: 5432, DBName: "app", User: "u"})

		if res.Reason != ReasonKilled {
			t.Fatalf("reason = %q, want killed (a cancel during VerifyTOC must not be reported as truncated)", res.Reason)
		}
	})

	t.Run("verify error under a live context is still truncated", func(t *testing.T) {
		dir := t.TempDir()
		pg := &fakePG{
			dump: func(_ context.Context, _ Conn, w io.Writer) (int, string, error) {
				_, _ = io.WriteString(w, "partial")
				return 0, "", nil
			},
			verify: func(_ context.Context, _ string) error { return errors.New("TOC unreadable") },
		}

		res := stageAndReplace(deadlineCtx(t), pg, dir, "app.dump", Conn{Host: "h", Port: 5432, DBName: "app", User: "u"})

		if res.Reason != ReasonTruncated {
			t.Fatalf("reason = %q, want truncated (live ctx; only the cancel branch flips to killed)", res.Reason)
		}
	})
}

// A context cancel that lands before the temp file is created (atomicfile checks
// ctx at NewPendingFile time) must classify as killed/timeout, not the generic
// ReasonOther: the run was aborted, not a temp-create fault.
func TestStageAndReplaceNewPendingContextCancel(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dead ctx before stageAndReplace: NewPendingFile sees it first
	res := stageAndReplace(ctx, &fakePG{}, dir, "app.dump", Conn{Host: "h", Port: 5432, DBName: "app", User: "u"})
	if res.Reason != ReasonKilled {
		t.Fatalf("reason = %q, want killed (a cancel at temp-create must not be ReasonOther)", res.Reason)
	}
}

// When pending.Commit fails for a real filesystem reason while the context is
// still live, stageAndReplace must classify the outcome as rename_failed -- not
// killed/timeout and not ok. Pre-creating the target as a directory makes
// atomicfile's final rename (temp -> target) fail with a non-context error, the
// only deterministic way to reach the rename_failed branch.
func TestStageAndReplaceCommitRenameFailedLiveCtx(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "app.dump"), 0o755); err != nil {
		t.Fatal(err)
	}
	pg := &fakePG{dump: func(_ context.Context, _ Conn, w io.Writer) (int, string, error) {
		_, _ = io.WriteString(w, "complete-archive")
		return 0, "", nil
	}}

	res := stageAndReplace(deadlineCtx(t), pg, dir, "app.dump", Conn{Host: "h", Port: 5432, DBName: "app", User: "u"})

	if res.Reason != ReasonRenameFailed {
		t.Fatalf("reason = %q, want rename_failed (a live-ctx Commit failure must classify as rename_failed, not killed)", res.Reason)
	}
}

// A non-context dump error with a zero exit code classifies as ReasonOther
// carrying the dump error text, distinct from the size/verify path.
func TestStageAndReplaceDumpErrorExitZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pg := &fakePG{dump: func(_ context.Context, _ Conn, _ io.Writer) (int, string, error) {
		return 0, "", errors.New("pipe error")
	}}

	res := stageAndReplace(deadlineCtx(t), pg, dir, "app.dump", Conn{Host: "h", Port: 5432, DBName: "app", User: "u"})

	if res.Reason != ReasonOther {
		t.Fatalf("reason = %q, want other (dump error with exit 0)", res.Reason)
	}
	if res.Detail != "pipe error" {
		t.Fatalf("detail = %q, want %q (the dump error text)", res.Detail, "pipe error")
	}
}

// A temp file that cannot be created in a missing directory (with a live ctx)
// classifies as ReasonOther with a non-empty "cannot create temp file" detail.
func TestStageAndReplaceUnwritableDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	res := stageAndReplace(deadlineCtx(t), &fakePG{}, missing, "app.dump",
		Conn{Host: "h", Port: 5432, DBName: "app", User: "u"})
	if res.Reason != ReasonOther {
		t.Fatalf("reason = %q, want other (the temp file cannot be created in a missing dir)", res.Reason)
	}
	if res.Detail == "" {
		t.Fatal("detail is empty, want a 'cannot create temp file' message")
	}
}

// stageAndReplace classifies an empty stderr tail as a generic line and a
// non-empty tail as an annotated line.
func TestStderrDetail(t *testing.T) {
	t.Parallel()
	if got := stderrDetail(""); got != "dump failed (pg_dump exited non-zero)" {
		t.Errorf("stderrDetail(%q) = %q, want generic message", "", got)
	}
	if got := stderrDetail("FATAL: nope"); got != "dump failed: FATAL: nope" {
		t.Errorf("stderrDetail(%q) = %q, want annotated message", "FATAL: nope", got)
	}
}
