package harness

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// unmanaged reports whether err is the gate's refusal, and about which path.
func unmanaged(t *testing.T, err error) string {
	t.Helper()
	var target *UnmanagedError
	if !errors.As(err, &target) {
		t.Fatalf("err = %v, want an *UnmanagedError", err)
	}
	return target.Path
}

func TestClaimDirCreatesAndMarksADirectoryThatIsNotThere(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills", "krowk")

	if err := ClaimDir(dir); err != nil {
		t.Fatalf("ClaimDir: %v", err)
	}
	if !DirOwned(dir) {
		t.Fatal("a directory krowk just created is not marked as its own")
	}
	data, err := os.ReadFile(filepath.Join(dir, ManagedMarker))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "managed by krowk") {
		t.Fatalf("marker says %q, which does not tell the reader who wrote the directory", data)
	}
}

func TestClaimDirAcceptsWhatKrowkMayWriteAndRefusesWhatItMayNot(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(t *testing.T, dir string)
		wantErr bool
	}{
		{
			name:    "a directory that is not there",
			arrange: func(*testing.T, string) {},
		},
		{
			name: "an empty directory somebody made by hand",
			arrange: func(t *testing.T, dir string) {
				mkdirAll(t, dir)
			},
		},
		{
			name: "a directory krowk already claimed",
			arrange: func(t *testing.T, dir string) {
				mkdirAll(t, dir)
				writeFile(t, filepath.Join(dir, ManagedMarker), managedMarkerContent)
				writeFile(t, filepath.Join(dir, "SKILL.md"), "# krowk\n")
			},
		},
		{
			name: "somebody's own populated skill directory",
			arrange: func(t *testing.T, dir string) {
				mkdirAll(t, dir)
				writeFile(t, filepath.Join(dir, "SKILL.md"), "# mine\n")
			},
			wantErr: true,
		},
		{
			name: "a file in the directory's name",
			arrange: func(t *testing.T, dir string) {
				mkdirAll(t, filepath.Dir(dir))
				writeFile(t, dir, "not a directory")
			},
			wantErr: true,
		},
		{
			name: "a marker that is a directory rather than a file",
			arrange: func(t *testing.T, dir string) {
				mkdirAll(t, filepath.Join(dir, ManagedMarker))
				mkdirAll(t, filepath.Join(dir, "SKILL.md"))
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "krowk")
			tc.arrange(t, dir)

			err := ClaimDir(dir)
			if tc.wantErr {
				if got := unmanaged(t, err); got != dir {
					t.Fatalf("refusal names %q, want %q", got, dir)
				}
				return
			}
			if err != nil {
				t.Fatalf("ClaimDir: %v", err)
			}
			if !DirOwned(dir) {
				t.Fatal("a claimed directory carries no marker")
			}
		})
	}
}

func TestClaimDirLeavesSomebodyElsesFilesAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "krowk")
	mkdirAll(t, dir)
	writeFile(t, filepath.Join(dir, "SKILL.md"), "# mine\n")

	if err := ClaimDir(dir); err == nil {
		t.Fatal("ClaimDir claimed a populated directory it did not write")
	}
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil || string(data) != "# mine\n" {
		t.Fatalf("SKILL.md = %q, %v — the refusal was not a no-op", data, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ManagedMarker)); err == nil {
		t.Fatal("a refused directory was marked anyway")
	}
}

func TestClaimDirRefusesToWriteThroughASymlinkedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege this test should not assume")
	}
	root := t.TempDir()
	target := filepath.Join(root, "elsewhere")
	mkdirAll(t, target)
	link := filepath.Join(root, "krowk")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if got := unmanaged(t, ClaimDir(link)); got != link {
		t.Fatalf("refusal names %q, want %q", got, link)
	}
	if _, err := os.Lstat(filepath.Join(target, ManagedMarker)); err == nil {
		t.Fatal("the marker was written through the symlink, into a directory nothing inspected")
	}
}

func TestClaimDirWillNotReclaimAPopulatedDirectoryWhoseMarkerWentMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "krowk")
	if err := ClaimDir(dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "SKILL.md"), "# krowk\n")
	if err := os.Remove(filepath.Join(dir, ManagedMarker)); err != nil {
		t.Fatal(err)
	}

	// Still populated, so this only works because the second claim is asking
	// about a directory it wrote — which it cannot know. It refuses, which is
	// the conservative answer, and the person moves it aside.
	if err := ClaimDir(dir); err == nil {
		t.Fatal("a populated directory with no marker was claimed")
	}
}

func TestWriteManagedFileWritesAndOverwritesARegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")

	if err := WriteManagedFile(path, []byte("# first\n")); err != nil {
		t.Fatalf("WriteManagedFile: %v", err)
	}
	if err := WriteManagedFile(path, []byte("# second\n")); err != nil {
		t.Fatalf("WriteManagedFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "# second\n" {
		t.Fatalf("file = %q, %v", data, err)
	}
}

func TestWriteManagedFileRefusesAnythingThatIsNotARegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	mkdirAll(t, path) // a directory in the file's name

	if got := unmanaged(t, WriteManagedFile(path, []byte("x"))); got != path {
		t.Fatalf("refusal names %q, want %q", got, path)
	}
}

func TestWriteManagedFileRefusesToFollowASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege this test should not assume")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "somebody-elses.md")
	writeFile(t, target, "# theirs\n")
	path := filepath.Join(dir, "SKILL.md")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if got := unmanaged(t, WriteManagedFile(path, []byte("# ours\n"))); got != path {
		t.Fatalf("refusal names %q, want %q", got, path)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "# theirs\n" {
		t.Fatalf("the symlink's target = %q, %v — it was written through", data, err)
	}
}

func TestVersionStampRoundTripsAndIsAbsentUntilWritten(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "krowk")
	if err := ClaimDir(dir); err != nil {
		t.Fatal(err)
	}
	if got := InstalledVersion(dir); got != "" {
		t.Fatalf("InstalledVersion = %q before anything stamped it, want \"\"", got)
	}
	if err := StampVersion(dir, "1.2.3"); err != nil {
		t.Fatalf("StampVersion: %v", err)
	}
	if got := InstalledVersion(dir); got != "1.2.3" {
		t.Fatalf("InstalledVersion = %q, want 1.2.3", got)
	}
	if err := StampVersion(dir, "1.3.0"); err != nil {
		t.Fatalf("StampVersion: %v", err)
	}
	if got := InstalledVersion(dir); got != "1.3.0" {
		t.Fatalf("InstalledVersion = %q after an upgrade, want 1.3.0", got)
	}
}

func TestInstalledVersionAnswersNothingForAStampItCannotTrust(t *testing.T) {
	t.Run("a directory in the stamp's name", func(t *testing.T) {
		dir := t.TempDir()
		mkdirAll(t, filepath.Join(dir, InstalledVersionFile))
		if got := InstalledVersion(dir); got != "" {
			t.Fatalf("InstalledVersion = %q, want \"\"", got)
		}
	})

	t.Run("a stamp longer than a version could be", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, InstalledVersionFile), strings.Repeat("9", 4096))
		got := InstalledVersion(dir)
		if len(got) > maxVersionStampBytes {
			t.Fatalf("InstalledVersion read %d bytes, want at most %d", len(got), maxVersionStampBytes)
		}
	})

	t.Run("no directory at all", func(t *testing.T) {
		if got := InstalledVersion(""); got != "" {
			t.Fatalf("InstalledVersion = %q, want \"\"", got)
		}
	})
}

func TestIsManagedCopyOnlyVouchesForADirectoryHoldingKrowksOwnFiles(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(t *testing.T, dir string)
		want    bool
	}{
		{
			name: "the marker, the stamp and the allowed file",
			arrange: func(t *testing.T, dir string) {
				mkdirAll(t, dir)
				writeFile(t, filepath.Join(dir, ManagedMarker), managedMarkerContent)
				writeFile(t, filepath.Join(dir, InstalledVersionFile), "1.2.3\n")
				writeFile(t, filepath.Join(dir, "SKILL.md"), "# krowk\n")
			},
			want: true,
		},
		{
			name: "the marker alone",
			arrange: func(t *testing.T, dir string) {
				mkdirAll(t, dir)
				writeFile(t, filepath.Join(dir, ManagedMarker), managedMarkerContent)
			},
			want: true,
		},
		{
			name: "a file the user added beside krowk's",
			arrange: func(t *testing.T, dir string) {
				mkdirAll(t, dir)
				writeFile(t, filepath.Join(dir, ManagedMarker), managedMarkerContent)
				writeFile(t, filepath.Join(dir, "notes.md"), "mine\n")
			},
		},
		{
			name: "a subdirectory the user added",
			arrange: func(t *testing.T, dir string) {
				mkdirAll(t, filepath.Join(dir, "reference"))
				writeFile(t, filepath.Join(dir, ManagedMarker), managedMarkerContent)
			},
		},
		{
			name: "krowk's own files, but no marker",
			arrange: func(t *testing.T, dir string) {
				mkdirAll(t, dir)
				writeFile(t, filepath.Join(dir, "SKILL.md"), "# krowk\n")
			},
		},
		{
			name:    "nothing there",
			arrange: func(*testing.T, string) {},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "krowk")
			tc.arrange(t, dir)
			if got := IsManagedCopy(dir, "SKILL.md"); got != tc.want {
				t.Fatalf("IsManagedCopy = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsManagedCopyRefusesAMarkerThatIsASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege this test should not assume")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "krowk")
	mkdirAll(t, dir)
	target := filepath.Join(root, "marker")
	writeFile(t, target, managedMarkerContent)
	if err := os.Symlink(target, filepath.Join(dir, ManagedMarker)); err != nil {
		t.Fatal(err)
	}

	if IsManagedCopy(dir, "SKILL.md") {
		t.Fatal("a symlink planted in the marker's name conferred ownership")
	}
	if DirOwned(dir) {
		t.Fatal("DirOwned accepted a symlinked marker")
	}
}

func TestUnmanagedErrorSaysWhatToDoAboutIt(t *testing.T) {
	err := &UnmanagedError{Path: "/home/someone/.claude/skills/krowk"}
	msg := err.Error()
	if !strings.Contains(msg, "/home/someone/.claude/skills/krowk") ||
		!strings.Contains(msg, "not written by krowk") ||
		!strings.Contains(msg, "move it aside") {
		t.Fatalf("message %q does not name the path, the reason and the remedy", msg)
	}
}

func TestClaimDirCreatesEveryMissingParent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "one", "two", "three", "krowk")

	if err := ClaimDir(dir); err != nil {
		t.Fatalf("ClaimDir: %v", err)
	}
	if !DirOwned(dir) {
		t.Fatal("the directory at the end of a missing chain was not claimed")
	}
}

func TestClaimDirAsksAgainWhenSomethingElseWonTheRace(t *testing.T) {
	// What os.Mkdir returning EEXIST means, staged directly: by the time the
	// directory is created, it is somebody else's populated one. The second
	// pass has to inspect it rather than assume krowk made it.
	root := t.TempDir()
	dir := filepath.Join(root, "krowk")
	mkdirAll(t, dir)
	writeFile(t, filepath.Join(dir, "SKILL.md"), "# theirs\n")

	if got := unmanaged(t, ClaimDir(dir)); got != dir {
		t.Fatalf("refusal names %q, want %q", got, dir)
	}
	if _, err := os.Lstat(filepath.Join(dir, ManagedMarker)); err == nil {
		t.Fatal("the loser of the race claimed the winner's directory")
	}
}

func TestIsManagedCopyRefusesAMarkerSayingSomethingElse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "krowk")
	mkdirAll(t, dir)
	writeFile(t, filepath.Join(dir, ManagedMarker), "copied out of somebody's dotfiles\n")
	writeFile(t, filepath.Join(dir, "SKILL.md"), "# theirs\n")

	if IsManagedCopy(dir, "SKILL.md") {
		t.Fatal("a marker krowk never wrote vouched for a directory krowk never made")
	}
	// The name alone is still enough to write there — the two questions are
	// different, and only the destructive one reads the contents.
	if !DirOwned(dir) {
		t.Fatal("DirOwned should still see a regular file in the marker's name")
	}
}

func TestManagedFilesEndUpAtTheModeTheyAreDocumentedAt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no meaningful answer for a Unix mode")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte("# old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteManagedFile(path, []byte("# new\n")); err != nil {
		t.Fatalf("WriteManagedFile: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode = %v, want 0644 — an agent has to be able to read it", got)
	}
}
