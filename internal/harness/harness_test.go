package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// envFrom is the Env every test uses: a fixed map, so nothing reads the
// environment the test runner happens to have.
func envFrom(pairs map[string]string) Env {
	return func(key string) string { return pairs[key] }
}

// homeEnv is the common case — a temporary directory standing in for $HOME.
func homeEnv(home string) Env {
	return envFrom(map[string]string{"HOME": home, "USERPROFILE": home})
}

func TestWorstReturnsTheMostSevereStatus(t *testing.T) {
	cases := []struct {
		name   string
		checks []StatusCheck
		want   string
	}{
		{"nothing checked passes", nil, StatusPass},
		{"all passing", []StatusCheck{Pass("a", "ok"), Pass("b", "ok")}, StatusPass},
		{"a warning wins over passes", []StatusCheck{Pass("a", "ok"), Warn("b", "hm", "h")}, StatusWarn},
		{"a failure wins over warnings", []StatusCheck{Warn("a", "hm", "h"), Fail("b", "no", "h")}, StatusFail},
		{"order does not matter", []StatusCheck{Fail("a", "no", "h"), Warn("b", "hm", "h")}, StatusFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Worst(tc.checks); got != tc.want {
				t.Fatalf("Worst() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStatusCheckOmitsTheHintWhenThereIsNothingToSuggest(t *testing.T) {
	data, err := json.Marshal(Pass("Claude Code", "Installed"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(data), `{"name":"Claude Code","status":"pass","message":"Installed"}`; got != want {
		t.Fatalf("passing check marshalled as %s, want %s", got, want)
	}

	data, err = json.Marshal(Fail("Claude Code", "Not found", "Install it"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"name":"Claude Code","status":"fail","message":"Not found","hint":"Install it"}`
	if string(data) != want {
		t.Fatalf("failing check marshalled as %s, want %s", data, want)
	}
}

func TestHomeDirComesFromTheEnvItWasGiven(t *testing.T) {
	if got := HomeDir(homeEnv("/tmp/somewhere")); got != filepath.Clean("/tmp/somewhere") {
		t.Fatalf("HomeDir = %q", got)
	}
	if got := HomeDir(envFrom(nil)); got != "" {
		t.Fatalf("HomeDir with no HOME = %q, want empty", got)
	}
	if got := HomeDir(nil); got != "" {
		t.Fatalf("HomeDir(nil) = %q, want empty", got)
	}
}

func TestLookPathFindsOnlyExecutableFilesOnTheGivenPath(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "claude"))

	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "notexec"), []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := envFrom(map[string]string{"PATH": dir + string(os.PathListSeparator) + other})
	if got := LookPath(env, "claude"); got != filepath.Join(dir, "claude") {
		t.Fatalf("LookPath = %q, want the executable in %s", got, dir)
	}
	if got := LookPath(env, "missing"); got != "" {
		t.Fatalf("LookPath for a missing binary = %q, want empty", got)
	}
	if runtime.GOOS != "windows" {
		if got := LookPath(env, "notexec"); got != "" {
			t.Fatalf("LookPath found a non-executable file: %q", got)
		}
	}
}

// writeExecutable writes a file that LookPath will accept as a binary.
func writeExecutable(t *testing.T, path string) {
	t.Helper()
	mode := os.FileMode(0o600)
	if runtime.GOOS != "windows" {
		mode = 0o700
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestLookPathIgnoresRelativePathEntries(t *testing.T) {
	// A checkout somebody else wrote, carrying a file called claude, with "."
	// on PATH. Whether an agent is installed must not depend on which
	// directory the process is standing in.
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "claude"))
	t.Chdir(dir)

	for _, entry := range []string{".", "", "relative/bin"} {
		env := envFrom(map[string]string{"PATH": entry})
		if got := LookPath(env, "claude"); got != "" {
			t.Fatalf("LookPath with PATH=%q = %q, want empty", entry, got)
		}
	}
}

func TestExecutableNamesFollowPathExtOnWindows(t *testing.T) {
	env := envFrom(map[string]string{"PATHEXT": ".COM" + string(os.PathListSeparator) + ".EXE"})
	got := executableNames(env, "claude")
	if runtime.GOOS != "windows" {
		if len(got) != 1 || got[0] != "claude" {
			t.Fatalf("executableNames = %v, want just the bare name off Windows", got)
		}
		return
	}
	// The bare name is not among them: nothing on Windows is executable
	// because of its permissions.
	want := []string{"claude.COM", "claude.EXE"}
	if len(got) != len(want) {
		t.Fatalf("executableNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("executableNames = %v, want %v", got, want)
		}
	}
	// An empty PATHEXT still gets the Windows defaults rather than nothing.
	if len(executableNames(envFrom(nil), "claude")) < 2 {
		t.Fatal("an empty PATHEXT left no executable suffixes to try")
	}
}

func TestReadConfigFileRefusesWhatItShouldNotRead(t *testing.T) {
	dir := t.TempDir()

	if _, err := readConfigFile(filepath.Join(dir, "absent.json"), true, maxProjectConfigBytes); !isNotExist(err) {
		t.Fatalf("reading a missing file = %v, want a not-exist error", err)
	}

	notAFile := filepath.Join(dir, "directory.json")
	if err := os.MkdirAll(notAFile, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfigFile(notAFile, true, maxProjectConfigBytes); err == nil || isNotExist(err) {
		t.Fatalf("reading a directory = %v, want a plain refusal", err)
	}

	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, make([]byte, maxProjectConfigBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfigFile(big, false, maxProjectConfigBytes); err == nil {
		t.Fatal("an oversized config was read instead of refused")
	}

	small := filepath.Join(dir, "small.json")
	if err := os.WriteFile(small, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := readConfigFile(small, false, maxProjectConfigBytes)
	if err != nil || string(data) != "{}" {
		t.Fatalf("readConfigFile = %q, %v", data, err)
	}
}

func TestReadConfigFileRefusesASymlinkedUntrustedConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege this test should not assume")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if _, err := readConfigFile(link, false, maxProjectConfigBytes); !errors.Is(err, errIsSymlink) {
		t.Fatalf("reading an untrusted symlink = %v, want %v", err, errIsSymlink)
	}
	// A home-owned dotfile may legitimately be a link into a dotfile repo.
	if data, err := readConfigFile(link, true, maxUserConfigBytes); err != nil || string(data) != "{}" {
		t.Fatalf("reading a trusted symlink = %q, %v", data, err)
	}
}
