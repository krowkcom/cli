package harness

import (
	"encoding/json"
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
