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

// A relative DUMP_DIR still produces dumps. The orchestrator resolves the
// configured directory to an absolute path once, at construction, because the
// per-server custody check refuses a relative one: a verdict about
// "dumps/h_5432" is a statement about wherever the process happens to be
// standing, which nothing stops another goroutine from changing between the
// check and the write. Operators have never been required to configure an
// absolute path, so without that resolution every database in the run would
// fail mkdir_failed.
func TestRunRelativeDumpDirStillWritesDumps(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	specs := []spec.DBSpec{{Host: "h", Port: 5432, DBName: "app", User: "u"}}

	res := orchestratorFor(t, "dumps", 1, specs).Run(deadlineCtx(t))

	if res[0].Reason != ReasonOK {
		t.Fatalf("relative DUMP_DIR %q: reason = %q, want ok (detail %q)", "dumps", res[0].Reason, res[0].Detail)
	}
	if _, err := os.Stat(filepath.Join(base, "dumps", "h_5432", "app.dump")); err != nil {
		t.Errorf("relative DUMP_DIR %q: dump not found under the resolved directory: %v", "dumps", err)
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

// requireSetgidWidensMkdir makes parent setgid and proves the kernel really does
// store a mode mkdir did not ask for underneath it, skipping the caller
// otherwise. It is the witness that keeps the create-path custody tests below
// honest: on a filesystem that honours every mode request, a test that creates a
// directory and finds it 0700 cannot tell a VERIFIED create from an unverified
// one, and would pass just as happily against the os.MkdirAll it replaced.
// Linux propagates S_ISGID from a setgid parent to a new subdirectory, which is
// a real widening produced by the kernel rather than a mock.
func requireSetgidWidensMkdir(t *testing.T, parent string) {
	t.Helper()
	if err := os.Chmod(parent, 0o700|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	witness := filepath.Join(parent, "witness")
	if err := os.Mkdir(witness, 0o700); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(witness)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetgid == 0 {
		t.Skipf("kernel did not widen a 0o700 mkdir under a setgid parent (got %v); "+
			"this test cannot distinguish a verified create from an unverified one here", fi.Mode())
	}
}

// The per-server directory that receives a full pg_dump must come out owner-only
// as MEASURED, not as requested. os.MkdirAll(dir, 0o700) asked for 0700 and
// never looked at what it got, so on a filesystem storing something wider the
// directory holding every row of every dumped database was born
// group-accessible, from a call that reported success.
func TestEnsureServerDirVerifiesTheModeItCreated(t *testing.T) {
	parent := t.TempDir()
	requireSetgidWidensMkdir(t, parent)

	dir := filepath.Join(parent, spec.ServerDir("h1", 5432))
	if err := orchestratorFor(t, parent, 1, nil).ensureServerDir(dir); err != nil {
		t.Fatalf("ensureServerDir: %v", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode(); got != os.ModeDir|0o700 {
		t.Fatalf("created dir mode = %v, want %v: the mode it created was not verified",
			got, os.ModeDir|0o700)
	}
}

// The same property end-to-end: a real run under a widening parent leaves the
// directory it dumped into owner-only, and the dump still lands.
func TestRunVerifiesTheServerDirModeItCreated(t *testing.T) {
	dir := t.TempDir()
	requireSetgidWidensMkdir(t, dir)

	specs := []spec.DBSpec{{Host: "h1", Port: 5432, DBName: "app", User: "u"}}
	res := orchestratorFor(t, dir, 1, specs).Run(deadlineCtx(t))
	if res[0].Reason != ReasonOK {
		t.Fatalf("reason = %q, want ok (detail %q)", res[0].Reason, res[0].Detail)
	}
	sub := filepath.Join(dir, "h1_5432")
	fi, err := os.Lstat(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode(); got != os.ModeDir|0o700 {
		t.Fatalf("server dir mode = %v, want %v", got, os.ModeDir|0o700)
	}
	if _, err := os.Stat(filepath.Join(sub, "app.dump")); err != nil {
		t.Errorf("dump should still land in the verified dir: %v", err)
	}
}

// A pre-existing server directory carrying group access is REFUSED, not adopted
// and not chmod'd into compliance: dumping into it would publish the database to
// whoever already had access, and repairing a directory this process did not
// create would take over a name another principal may own. The failure is the
// per-DB mkdir_failed reason, so the operator sees it per database rather than
// losing the whole run.
// TestEnsureServerDirRepairsItsOwnGroupAccessibleOutput pins the migration
// contract. An earlier release created these per-server directories with a plain
// MkdirAll(0o700), so on a widening dataset they are already sitting at 0770 —
// and the library's default rule, never repair a pre-existing directory, would
// refuse every one of them and fail every database with mkdir_failed on the
// first run after the upgrade. WithRepairOwnedDir narrows them instead.
//
// This is not a weakening: EnsurePrivateDir has already proved the directory is
// owned by our own euid before the repair is considered, and a directory cannot
// be planted under another uid's ownership. What it costs is that a deliberately
// group-readable per-server directory would be narrowed — which is the right
// trade here and NOT at the cycle dir, whose comment says so: that one is on
// ephemeral tmpfs where no legacy mode can exist, so it still refuses.
func TestEnsureServerDirRepairsItsOwnGroupAccessibleOutput(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, spec.ServerDir("h1", 5432))
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Mkdir applies umask; force the wide mode explicitly.
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := orchestratorFor(t, parent, 1, nil).ensureServerDir(dir); err != nil {
		t.Fatalf("a 0750 directory this app itself created was refused: %v; "+
			"every database would fail with mkdir_failed on the first run after upgrade", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v, want 0700: the pre-existing directory was adopted without being repaired",
			fi.Mode().Perm())
	}
}
