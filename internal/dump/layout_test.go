package dump

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/pg-autodump/internal/spec"
)

// orchestratorFor builds an Orchestrator over the given specs with the default
// (success) fakePG, a fixed 2026 clock, and keep.
func orchestratorFor(t *testing.T, dir string, keep int, specs []spec.DBSpec) *Orchestrator {
	t.Helper()
	return New(&Params{
		PG:          &fakePG{},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:         func() time.Time { return time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC) },
		DumpDir:     dir,
		Specs:       specs,
		DumpTimeout: 30 * time.Second,
		Concurrency: 2,
		Keep:        keep,
	})
}

// Two databases sharing a name on different hosts must not collide: each lands
// under its own <host>_<port>/ subdirectory, so both backups survive. This is
// the h-f3 silent-overwrite regression test.
func TestRunHostQualifiedNoCollision(t *testing.T) {
	dir := t.TempDir()
	specs := []spec.DBSpec{
		{Host: "h1", Port: 5432, DBName: "app", User: "u"},
		{Host: "h2", Port: 5432, DBName: "app", User: "u"},
	}
	res := orchestratorFor(t, dir, 1, specs).Run(deadlineCtx(t))
	for i, r := range res {
		if r.Reason != ReasonOK {
			t.Fatalf("spec[%d] reason = %q, want ok (detail %q)", i, r.Reason, r.Detail)
		}
	}
	for _, sub := range []string{"h1_5432", "h2_5432"} {
		p := filepath.Join(dir, sub, "app.dump")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %q to exist: %v", p, err)
		}
	}
}

// The same database name on the same host but two ports (two containers) also
// stays distinct.
func TestRunHostQualifiedPortDisambiguation(t *testing.T) {
	dir := t.TempDir()
	specs := []spec.DBSpec{
		{Host: "h", Port: 5432, DBName: "app", User: "u"},
		{Host: "h", Port: 5433, DBName: "app", User: "u"},
	}
	res := orchestratorFor(t, dir, 1, specs).Run(deadlineCtx(t))
	for i, r := range res {
		if r.Reason != ReasonOK {
			t.Fatalf("spec[%d] reason = %q, want ok", i, r.Reason)
		}
	}
	for _, sub := range []string{"h_5432", "h_5433"} {
		if _, err := os.Stat(filepath.Join(dir, sub, "app.dump")); err != nil {
			t.Errorf("expected %q/app.dump: %v", sub, err)
		}
	}
}

// An IPv6 host lands under its '@'-encoded subdirectory, distinct from a
// same-named database on a hostname server.
func TestRunHostQualifiedIPv6(t *testing.T) {
	dir := t.TempDir()
	specs := []spec.DBSpec{
		{Host: "2001:db8::1", Port: 5432, DBName: "app", User: "u"},
		{Host: "db.example.com", Port: 5432, DBName: "app", User: "u"},
	}
	res := orchestratorFor(t, dir, 1, specs).Run(deadlineCtx(t))
	for i, r := range res {
		if r.Reason != ReasonOK {
			t.Fatalf("spec[%d] reason = %q, want ok", i, r.Reason)
		}
	}
	for _, sub := range []string{"@2001-db8--1_5432", "db.example.com_5432"} {
		if _, err := os.Stat(filepath.Join(dir, sub, "app.dump")); err != nil {
			t.Errorf("expected %q/app.dump: %v", sub, err)
		}
	}
}

// When the per-server subdirectory cannot be created (here a regular file
// occupies its path), that database fails with reason mkdir_failed and a detail
// naming the directory, and other databases in the run are unaffected.
func TestRunMkdirFailedIsPerDB(t *testing.T) {
	dir := t.TempDir()
	// Occupy "h1_5432" with a regular file so MkdirAll fails for that server.
	if err := os.WriteFile(filepath.Join(dir, "h1_5432"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	specs := []spec.DBSpec{
		{Host: "h1", Port: 5432, DBName: "app", User: "u"},
		{Host: "h2", Port: 5432, DBName: "app", User: "u"},
	}
	res := orchestratorFor(t, dir, 1, specs).Run(deadlineCtx(t))

	if res[0].Reason != ReasonMkdirFailed {
		t.Fatalf("spec[0] reason = %q, want mkdir_failed (detail %q)", res[0].Reason, res[0].Detail)
	}
	if !strings.Contains(res[0].Detail, "h1_5432") {
		t.Fatalf("spec[0] detail = %q, want it to name the server dir", res[0].Detail)
	}
	if res[1].Reason != ReasonOK {
		t.Fatalf("spec[1] reason = %q, want ok (other databases unaffected)", res[1].Reason)
	}
}

// With keep>1, retention is scoped to each server's subdirectory: pruning one
// server's old copies never touches another server's copies, even when they
// share a database name.
func TestRunRetentionIsolatedPerServer(t *testing.T) {
	dir := t.TempDir()
	old := []string{
		"app.20200101T000000Z.dump",
		"app.20200102T000000Z.dump",
		"app.20200103T000000Z.dump",
	}
	for _, sub := range []string{"h1_5432", "h2_5432"} {
		d := filepath.Join(dir, sub)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, ts := range old {
			if err := os.WriteFile(filepath.Join(d, ts), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	specs := []spec.DBSpec{
		{Host: "h1", Port: 5432, DBName: "app", User: "u"},
		{Host: "h2", Port: 5432, DBName: "app", User: "u"},
	}
	// keep=2: each server keeps its 2 newest (fresh 2026 dump + newest 2020),
	// pruning exactly the 2 oldest 2020 copies in EACH subdir independently.
	res := orchestratorFor(t, dir, 2, specs).Run(deadlineCtx(t))
	for i, r := range res {
		if r.Reason != ReasonOK {
			t.Fatalf("spec[%d] reason = %q, want ok", i, r.Reason)
		}
	}
	for _, sub := range []string{"h1_5432", "h2_5432"} {
		d := filepath.Join(dir, sub)
		for _, gone := range []string{"app.20200101T000000Z.dump", "app.20200102T000000Z.dump"} {
			if _, err := os.Stat(filepath.Join(d, gone)); !os.IsNotExist(err) {
				t.Errorf("%s/%s should be pruned, stat err = %v", sub, gone, err)
			}
		}
		if _, err := os.Stat(filepath.Join(d, "app.20200103T000000Z.dump")); err != nil {
			t.Errorf("%s: newest 2020 copy should survive: %v", sub, err)
		}
		if _, err := os.Stat(filepath.Join(d, "app.20260615T000000Z.dump")); err != nil {
			t.Errorf("%s: fresh 2026 dump should exist: %v", sub, err)
		}
	}
}
