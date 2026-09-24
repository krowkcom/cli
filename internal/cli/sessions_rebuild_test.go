package cli

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/store"
)

// fileHash is what "deleted nothing" is checked against: the bytes, not a
// stat that a rewrite in place would leave looking the same.
func fileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// rebuildHarness is an import harness with all three fixtures seeded and
// imported once, so there is a real krowk.db to rebuild.
func rebuildHarness(t *testing.T) (*harness, func(string) string, string) {
	t.Helper()
	h, home := importHarness(t)
	seedClaude(t, home)
	seedCursor(t, home)
	seedOpencode(t, home)
	env := func(k string) string { return h.env[k] }
	mustRun(t, h, "sessions", "import", "--from", "all", "--json")
	return h, env, store.DBPath(store.Env(env))
}

func removedPaths(t *testing.T, stdout string) []string {
	t.Helper()
	var e struct {
		Data struct {
			Removed *[]string `json:"removed"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatalf("not an envelope: %v\n%s", err, stdout)
	}
	if e.Data.Removed == nil {
		t.Fatalf("the rebuild report carries no removed list:\n%s", stdout)
	}
	return *e.Data.Removed
}

// The gate's hint names rebuild, and rebuild is what gets past it: a file
// stamped with a version this build does not know is refused by import,
// and after `sessions rebuild --yes` it is a v1 file holding the fixtures.
func TestRebuildRecoversFromASchemaMismatch(t *testing.T) {
	h, env, dbPath := rebuildHarness(t)
	want := rowCounts(t, env)

	db, err := store.Open(store.Env(env))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	code, stdout, stderr := h.run("sessions", "import", "--from", "all", "--json")
	if code == 0 || !strings.Contains(stdout+stderr, "krowk sessions rebuild") {
		t.Fatalf("import on a v2 file: exit %d, want a failure naming rebuild\n%s%s", code, stdout, stderr)
	}

	code, stdout, stderr = h.run("sessions", "rebuild", "--yes", "--json")
	if code != 0 {
		t.Fatalf("rebuild exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if removed := removedPaths(t, stdout); !slices.Contains(removed, dbPath) {
		t.Errorf("removed = %v, want it to name %s", removed, dbPath)
	}
	var e importEnvelope
	if err := json.Unmarshal([]byte(stdout), &e); err != nil || !e.OK || len(e.Data.Providers) != 3 {
		t.Errorf("rebuild did not answer with import's per-provider envelope:\n%s", stdout)
	}

	db, err = store.Open(store.Env(env))
	if err != nil {
		t.Fatalf("the rebuilt store does not open: %v", err)
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if version != store.SchemaVersion {
		t.Errorf("rebuilt user_version = %d, want %d", version, store.SchemaVersion)
	}
	if got := rowCounts(t, env); got["session"] != want["session"] || got["message"] != want["message"] ||
		got["part"] != want["part"] {
		t.Errorf("rebuilt store holds %v, the first import held %v", got, want)
	}
}

// No terminal and no --yes is a refusal before anything is touched: the
// file's bytes are the same afterwards.
func TestRebuildWithoutYesOffATerminalDeletesNothing(t *testing.T) {
	h, _, dbPath := rebuildHarness(t)
	before := fileHash(t, dbPath)

	code, stdout, stderr := h.run("sessions", "rebuild", "--json")
	if code == 0 {
		t.Fatalf("rebuild without --yes exited 0:\n%s", stdout)
	}
	if !strings.Contains(stderr, "confirmation_required") || !strings.Contains(stderr, "--yes") {
		t.Errorf("the refusal does not say --yes is what is missing:\n%s", stderr)
	}
	if fileHash(t, dbPath) != before {
		t.Error("a refused rebuild changed krowk.db")
	}
}

// Exactly the database and its two sidecars go; the lock and anything else
// in the directory stay.
func TestRebuildDeletesOnlyTheStoreAndItsSidecars(t *testing.T) {
	h, _, dbPath := rebuildHarness(t)
	dir := filepath.Dir(dbPath)
	stray := filepath.Join(dir, "notes.txt")
	for _, p := range []string{stray, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			continue // a sidecar SQLite left behind is as good as a seeded one
		}
		if err := os.WriteFile(p, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	code, stdout, stderr := h.run("sessions", "rebuild", "--yes", "--json")
	if code != 0 {
		t.Fatalf("rebuild exited %d\n%s%s", code, stdout, stderr)
	}
	want := []string{dbPath, dbPath + "-wal", dbPath + "-shm"}
	if got := removedPaths(t, stdout); !slices.Equal(got, want) {
		t.Errorf("removed = %v, want %v", got, want)
	}
	if b, err := os.ReadFile(stray); err != nil || string(b) != "keep" {
		t.Errorf("a stray file in the store directory did not survive: %v", err)
	}
	if _, err := os.Stat(importLockPath(dbPath)); err != nil {
		t.Errorf("import.lock did not survive: %v", err)
	}
}

// A held lock is an import in progress: rebuild refuses at once, the same
// way a second import does, and the file is untouched.
func TestRebuildRefusesWhileAnotherHoldsTheLock(t *testing.T) {
	h, _, dbPath := rebuildHarness(t)
	before := fileHash(t, dbPath)
	held, err := lockImport(importLockPath(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	code, _, stderr := h.run("sessions", "rebuild", "--yes", "--json")
	if code != 6 || !strings.Contains(stderr, "import_locked") {
		t.Errorf("exit = %d, want 6 import_locked\n%s", code, stderr)
	}
	if fileHash(t, dbPath) != before {
		t.Error("a rebuild refused by the lock changed krowk.db")
	}
}

// Doctor's store check passes on what rebuild leaves behind.
func TestDoctorStoreCheckPassesAfterARebuild(t *testing.T) {
	h, _, _ := rebuildHarness(t)
	if code, stdout, stderr := h.run("sessions", "rebuild", "--yes", "--json"); code != 0 {
		t.Fatalf("rebuild exited %d\n%s%s", code, stdout, stderr)
	}
	b, _ := json.Marshal(doctorReport(t, h)["store"])
	var check struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(b, &check); err != nil || check.Status != "pass" {
		t.Errorf("store check after a rebuild = %s, want pass", b)
	}
}

// --yes belongs to rebuild, and rebuild takes none of import's flags.
func TestRebuildFlagsAreRefusedElsewhere(t *testing.T) {
	h, _ := importHarness(t)
	for _, args := range [][]string{
		{"sessions", "import", "--from", "all", "--yes"},
		{"sessions", "rebuild", "--yes", "--from", "claude"},
		{"sessions", "rebuild", "--yes", "--dry-run"},
		{"sessions", "rebuild", "--yes", "--limit", "1"},
	} {
		code, _, stderr := h.run(append(args, "--json")...)
		if code != 1 || !strings.Contains(stderr, "bad_flag") {
			t.Errorf("`krowk %s`: exit %d, want 1 bad_flag\n%s", strings.Join(args, " "), code, stderr)
		}
	}
}
