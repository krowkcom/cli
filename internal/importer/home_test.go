package importer

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/harness"
)

// testEnv is the same env-as-a-lookup shape the rest of the repo tests with,
// so no test touches the process environment.
func testEnv(pairs map[string]string) harness.Env {
	return func(k string) string { return pairs[k] }
}

func homeEnv(home string) harness.Env {
	return testEnv(map[string]string{"HOME": home})
}

func TestOpenHomeReadsUnderHome(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	rel := filepath.Join(".claude", "projects", "a.jsonl")
	if err := os.WriteFile(filepath.Join(home, rel), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := OpenHome(homeEnv(home), rel, 0)
	if err != nil {
		t.Fatalf("OpenHome: %v", err)
	}
	defer f.Close()
	data, err := ReadHome(homeEnv(home), rel, 0)
	if err != nil || string(data) != "{}\n" {
		t.Fatalf("ReadHome = %q, %v", data, err)
	}
}

// Acceptance: the trusted-home helper refuses a path outside home and a
// symlink pointing outside home, both with a typed error and without
// reading.
func TestOpenHomeRefusesOutsideHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	outside := filepath.Join(root, "checkout")
	for _, d := range []string{home, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	secret := filepath.Join(outside, "secret.jsonl")
	if err := os.WriteFile(secret, []byte("{\"secret\":true}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tests := []struct {
		name string
		rel  string
		want error
	}{
		{
			name: "absolute path elsewhere",
			rel:  secret,
			want: ErrOutsideHome,
		},
		{
			name: "relative path climbing out with ..",
			rel:  filepath.Join("..", "checkout", "secret.jsonl"),
			want: ErrOutsideHome,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := OpenHome(homeEnv(home), tt.rel, 0)
			if f != nil {
				f.Close()
				t.Fatal("OpenHome returned a file, want a refusal")
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			// And nothing was read: the refusal has to happen before any
			// byte of the file it refused.
			data, err := ReadHome(homeEnv(home), tt.rel, 0)
			if data != nil {
				t.Fatalf("ReadHome returned %d bytes, want none", len(data))
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("ReadHome err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestOpenHomeRefusesEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, "sessions"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	secret := filepath.Join(checkout, "sessions", "a.jsonl")
	if err := os.WriteFile(secret, []byte("{\"secret\":true}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The leaf is a link out of home: the file is under home on paper and
	// in a checkout in fact.
	if err := os.Symlink(secret, filepath.Join(home, "leaf.jsonl")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	// And a whole directory linked out, which is the dotfile-repository
	// shape: the escape is a component, not the leaf.
	if err := os.Symlink(filepath.Join(checkout, "sessions"), filepath.Join(home, "linked")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	for _, rel := range []string{"leaf.jsonl", filepath.Join("linked", "a.jsonl")} {
		t.Run(rel, func(t *testing.T) {
			f, err := OpenHome(homeEnv(home), rel, 0)
			if f != nil {
				f.Close()
				t.Fatal("OpenHome returned a file, want a refusal")
			}
			if !errors.Is(err, ErrEscapingSymlink) {
				t.Fatalf("err = %v, want ErrEscapingSymlink", err)
			}
			data, rerr := ReadHome(homeEnv(home), rel, 0)
			if data != nil {
				t.Fatalf("ReadHome returned %d bytes, want none", len(data))
			}
			if !errors.Is(rerr, ErrEscapingSymlink) {
				t.Fatalf("ReadHome err = %v, want ErrEscapingSymlink", rerr)
			}
		})
	}
}

// A symlink whose target does not exist yet is the case path resolution is
// most likely to get wrong: EvalSymlinks reports it as not existing, which
// is true of the target and false of the link. Filing it as a missing
// component would vouch for a path that starts reading outside home the
// moment somebody creates the target.
//
// It is refused whichever way it dangles. An inward-dangling link is
// harmless today and would be approved on the strength of a file that does
// not exist; an answer that changes when somebody else creates a file is not
// an answer worth giving.
func TestHomePathRefusesDanglingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	tests := []struct {
		name   string
		link   string
		target string
	}{
		{
			name:   "dangling out of home",
			link:   "out.jsonl",
			target: filepath.Join(root, "checkout", "not-yet.jsonl"),
		},
		{
			name:   "dangling inside home",
			link:   "in.jsonl",
			target: filepath.Join(home, "not-yet.jsonl"),
		},
		{
			name:   "dangling directory component",
			link:   filepath.Join("linkdir", "a.jsonl"),
			target: filepath.Join(root, "checkout", "sessions"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			link := filepath.Join(home, tt.link)
			if filepath.Dir(tt.link) != "." {
				link = filepath.Join(home, filepath.Dir(tt.link))
			}
			if err := os.Symlink(tt.target, link); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
			t.Cleanup(func() { _ = os.Remove(link) })
			if _, err := HomePath(homeEnv(home), tt.link); !errors.Is(err, ErrEscapingSymlink) {
				t.Fatalf("HomePath = %v, want ErrEscapingSymlink", err)
			}
			data, err := ReadHome(homeEnv(home), tt.link, 0)
			if data != nil {
				t.Fatalf("ReadHome returned %d bytes, want none", len(data))
			}
			if !errors.Is(err, ErrEscapingSymlink) {
				t.Fatalf("ReadHome err = %v, want ErrEscapingSymlink", err)
			}
		})
	}
}

// Home is trusted, so a link that stays inside home is followed: dotfiles
// are legitimately symlinked about within a home directory.
func TestOpenHomeFollowsSymlinkInsideHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	home := t.TempDir()
	real := filepath.Join(home, "real.jsonl")
	if err := os.WriteFile(real, []byte("{\"ok\":true}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(real, filepath.Join(home, "link.jsonl")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	data, err := ReadHome(homeEnv(home), "link.jsonl", 0)
	if err != nil {
		t.Fatalf("ReadHome: %v", err)
	}
	if string(data) != "{\"ok\":true}\n" {
		t.Fatalf("data = %q", data)
	}
}

func TestOpenHomeRefusesOversizeFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "big.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := OpenHome(homeEnv(home), "big.jsonl", 1024)
	if f != nil {
		f.Close()
		t.Fatal("OpenHome returned a file, want a refusal")
	}
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestOpenHomeRefusesDirectory(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "dir"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f, err := OpenHome(homeEnv(home), "dir", 0)
	if f != nil {
		f.Close()
		t.Fatal("OpenHome opened a directory")
	}
	if !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("err = %v, want ErrNotRegularFile", err)
	}
}

func TestHomePathNoHomeAndRelativeHome(t *testing.T) {
	tests := []struct {
		name string
		env  harness.Env
	}{
		{name: "no home in env", env: testEnv(map[string]string{})},
		{name: "nil env", env: nil},
		{name: "relative home never lands under cwd", env: homeEnv("relative/home")},
		// A home of "/" would make "under home" true of every file on the
		// machine, which is not a home directory, it is a broken
		// environment.
		{name: "filesystem root is not a home directory", env: homeEnv("/")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := HomePath(tt.env, "a.jsonl"); !errors.Is(err, ErrNoHome) {
				t.Fatalf("err = %v, want ErrNoHome", err)
			}
		})
	}
}

// A file that is not there yet still resolves: Discover has to be able to
// ask about a path before anything writes to it.
func TestHomePathAllowsMissingLeaf(t *testing.T) {
	home := t.TempDir()
	got, err := HomePath(homeEnv(home), filepath.Join("a", "b", "c.jsonl"))
	if err != nil {
		t.Fatalf("HomePath: %v", err)
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got != filepath.Join(realHome, "a", "b", "c.jsonl") {
		t.Fatalf("HomePath = %q", got)
	}
}

func TestHomePathHomeItself(t *testing.T) {
	home := t.TempDir()
	if _, err := HomePath(homeEnv(home), "."); err != nil {
		t.Fatalf("HomePath(home) = %v, want the home directory itself to pass", err)
	}
}

// /home/user-backup is not under /home/user, however much it looks like it.
func TestHomePathSiblingPrefixRefused(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "user")
	sibling := filepath.Join(root, "user-backup")
	for _, d := range []string{home, sibling} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	if _, err := HomePath(homeEnv(home), filepath.Join(sibling, "a.jsonl")); !errors.Is(err, ErrOutsideHome) {
		t.Fatalf("err = %v, want ErrOutsideHome", err)
	}
}

func TestDefaultMaxBytesIs64MiB(t *testing.T) {
	if DefaultMaxBytes != 64*1024*1024 {
		t.Fatalf("DefaultMaxBytes = %d, want 64 MiB", DefaultMaxBytes)
	}
}
