package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noEnv is the environment of a machine with nothing exported, which is where
// the files are supposed to be the only voice.
func noEnv(string) string { return "" }

// withWorkspace answers for KROWK_WORKSPACE alone, which is the only variable
// any of this consults.
func withWorkspace(v string) func(string) string {
	return func(k string) string {
		if k == "KROWK_WORKSPACE" {
			return v
		}
		return ""
	}
}

// isolate points the global config at a scratch directory and returns a
// checkout that is a git repository, so a test never reads the config of
// whoever is running it and never depends on this repository's own .krowk.
func isolate(t *testing.T) (globalPath, repoDir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)

	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, "krowk", "config.json"), repo
}

// write puts a config file on disk the way a person would, rather than through
// Set, so the parsing is tested against what someone actually typed.
func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repoConfig(dir string) string { return filepath.Join(dir, ".krowk", "config.json") }

// The whole point of the package: the strongest layer present wins, and every
// layer below it still gets its turn when the ones above stay quiet.
func TestLoadPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		global     string
		repo       string
		env        string
		flag       string
		want       string
		wantSource string
	}{{
		name:       "global alone",
		global:     `{"workspace": "from-global"}`,
		want:       "from-global",
		wantSource: SourceGlobal,
	}, {
		name:       "repo beats global",
		global:     `{"workspace": "from-global"}`,
		repo:       `{"workspace": "from-repo"}`,
		want:       "from-repo",
		wantSource: SourceRepo,
	}, {
		name:       "global shows through a repo file that says nothing",
		global:     `{"workspace": "from-global"}`,
		repo:       `{}`,
		want:       "from-global",
		wantSource: SourceGlobal,
	}, {
		name:       "env beats repo",
		global:     `{"workspace": "from-global"}`,
		repo:       `{"workspace": "from-repo"}`,
		env:        "from-env",
		want:       "from-env",
		wantSource: SourceEnv,
	}, {
		name:       "repo shows through an unset env",
		repo:       `{"workspace": "from-repo"}`,
		want:       "from-repo",
		wantSource: SourceRepo,
	}, {
		name:       "flag beats env",
		global:     `{"workspace": "from-global"}`,
		repo:       `{"workspace": "from-repo"}`,
		env:        "from-env",
		flag:       "from-flag",
		want:       "from-flag",
		wantSource: SourceFlag,
	}, {
		name:       "env shows through an unpassed flag",
		env:        "from-env",
		want:       "from-env",
		wantSource: SourceEnv,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			globalPath, repo := isolate(t)
			if tc.global != "" {
				write(t, globalPath, tc.global)
			}
			if tc.repo != "" {
				write(t, repoConfig(repo), tc.repo)
			}

			c, err := Load(repo, withWorkspace(tc.env), Overrides{Workspace: tc.flag})
			if err != nil {
				t.Fatal(err)
			}
			if c.Workspace != tc.want {
				t.Errorf("workspace = %q, want %q", c.Workspace, tc.want)
			}
			// Knowing which file to go and edit is half of what this is for, so
			// the source is as much of the answer as the value.
			if got := c.Sources["workspace"]; got != tc.wantSource {
				t.Errorf("source = %q, want %q", got, tc.wantSource)
			}
		})
	}
}

// Most machines have neither file, and that is not a state worth complaining
// about.
func TestLoadWithNoFilesAnywhere(t *testing.T) {
	_, repo := isolate(t)

	c, err := Load(repo, noEnv, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace != "" {
		t.Errorf("workspace = %q, want empty", c.Workspace)
	}
	// Nothing set it, so nothing claims to have set it — an empty value with a
	// source attached would be a lie about a layer.
	if len(c.Sources) != 0 {
		t.Errorf("sources = %v, want none", c.Sources)
	}
}

// Outside a checkout there is no repo layer at all, and Load must not invent
// one or fall over looking for it.
func TestLoadOutsideARepository(t *testing.T) {
	globalPath, _ := isolate(t)
	write(t, globalPath, `{"workspace": "from-global"}`)

	c, err := Load(t.TempDir(), noEnv, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace != "from-global" {
		t.Errorf("workspace = %q, want from-global", c.Workspace)
	}
}

// A broken file is a refusal, not a shrug: it was written to steer uploads, and
// carrying on would send them somewhere the file says they are not going. The
// path has to be in the message, since the two files are otherwise identical to
// look at.
func TestLoadRejectsMalformedJSON(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"truncated", `{"workspace": "acme"`},
		{"not an object", `"acme"`},
		{"known key of the wrong type", `{"workspace": 7}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, repo := isolate(t)
			path := repoConfig(repo)
			write(t, path, tc.body)

			_, err := Load(repo, noEnv, Overrides{})
			if err == nil {
				t.Fatal("loaded a broken config without complaint")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name %q", err, path)
			}
		})
	}
}

// The global file gets the same treatment, and the error names it rather than
// the repo file that was fine.
func TestLoadRejectsMalformedGlobalJSON(t *testing.T) {
	globalPath, repo := isolate(t)
	write(t, globalPath, `{oops}`)

	_, err := Load(repo, noEnv, Overrides{})
	if err == nil {
		t.Fatal("loaded a broken global config without complaint")
	}
	if !strings.Contains(err.Error(), globalPath) {
		t.Errorf("error %q does not name %q", err, globalPath)
	}
}

// An older krowk meeting a key from a newer one has to keep working: it reads
// what it knows and leaves the rest alone.
func TestLoadIgnoresUnknownKeys(t *testing.T) {
	_, repo := isolate(t)
	write(t, repoConfig(repo), `{"workspace": "acme", "retention": 90, "future": {"nested": true}}`)

	c, err := Load(repo, noEnv, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace != "acme" {
		t.Errorf("workspace = %q, want acme", c.Workspace)
	}
}

// Clearing a key by blanking it must not blank the layer underneath — an empty
// value is "I have nothing to say", not "use the empty workspace".
func TestBlankValuesDoNotOverride(t *testing.T) {
	for _, empty := range []string{`""`, `"   "`, "\"\\t\\n\""} {
		globalPath, repo := isolate(t)
		write(t, globalPath, `{"workspace": "from-global"}`)
		write(t, repoConfig(repo), `{"workspace": `+empty+`}`)

		c, err := Load(repo, withWorkspace("  "), Overrides{Workspace: " "})
		if err != nil {
			t.Fatal(err)
		}
		if c.Workspace != "from-global" {
			t.Errorf("workspace = %q with repo value %s, want from-global", c.Workspace, empty)
		}
		if got := c.Sources["workspace"]; got != SourceGlobal {
			t.Errorf("source = %q, want %q", got, SourceGlobal)
		}
	}
}

// Values are taken as given. A workspace with surrounding space is more likely
// a name this build has no business editing than a mistake worth guessing at.
func TestValuesAreStoredAsWritten(t *testing.T) {
	_, repo := isolate(t)
	write(t, repoConfig(repo), `{"workspace": " acme "}`)

	c, err := Load(repo, noEnv, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace != " acme " {
		t.Errorf("workspace = %q, want %q", c.Workspace, " acme ")
	}
}

// krowk is run from wherever the work is, which is hardly ever the top of the
// checkout.
func TestRepoPathFindsTheRootFromASubdirectory(t *testing.T) {
	_, repo := isolate(t)
	deep := filepath.Join(repo, "cmd", "krowk", "testdata")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	path, inRepo := RepoPath(deep)
	if !inRepo {
		t.Fatal("did not recognise a subdirectory of a repository")
	}
	if want := repoConfig(repo); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// Worktrees and submodules record their git directory in a .git file, and those
// checkouts are repositories like any other.
func TestRepoPathAcceptsGitAsAFile(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: /elsewhere/.git/worktrees/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, inRepo := RepoPath(repo)
	if !inRepo {
		t.Fatal("a worktree checkout was not recognised as a repository")
	}
	if want := repoConfig(repo); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestRepoPathOutsideARepository(t *testing.T) {
	path, inRepo := RepoPath(t.TempDir())
	if inRepo {
		t.Errorf("claimed a bare directory is a repository, at %q", path)
	}
}

// The path is answered before the file exists, because that is where `config
// set` has to write the first time.
func TestRepoPathAnswersBeforeTheFileExists(t *testing.T) {
	_, repo := isolate(t)

	path, inRepo := RepoPath(repo)
	if !inRepo || path == "" {
		t.Fatalf("path = %q, inRepo = %v", path, inRepo)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the file exists already: %v", err)
	}
}

func TestGlobalPathHonoursXDGConfigHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	if got, want := GlobalPath(), filepath.Join(dir, "krowk", "config.json"); got != want {
		t.Errorf("GlobalPath() = %q, want %q", got, want)
	}
}

func TestKnownKeys(t *testing.T) {
	keys := KnownKeys()
	if len(keys) != 1 || keys[0] != "workspace" {
		t.Errorf("KnownKeys() = %v", keys)
	}
}

// The first `config set` in a checkout has neither the file nor the directory
// to write it into.
func TestSetCreatesTheFileAndItsDirectory(t *testing.T) {
	_, repo := isolate(t)
	path := repoConfig(repo)

	if err := Set(path, "workspace", "acme"); err != nil {
		t.Fatal(err)
	}

	c, err := Load(repo, noEnv, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace != "acme" {
		t.Errorf("workspace = %q, want acme", c.Workspace)
	}
	// The repo file is meant to be committed and read by everyone in the
	// checkout, so it is not a secret and must not be written like one.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("config mode = %o, want 644", perm)
	}
}

// An older build editing a file a newer one wrote must not take the newer
// build's settings with it.
func TestSetAndUnsetPreserveUnknownKeys(t *testing.T) {
	_, repo := isolate(t)
	path := repoConfig(repo)
	write(t, path, `{"workspace": "old", "retention": 90}`)

	if err := Set(path, "workspace", "acme"); err != nil {
		t.Fatal(err)
	}
	if got := raw(t, path)["retention"]; got != float64(90) {
		t.Errorf("retention = %v after Set, want 90", got)
	}

	if err := Unset(path, "workspace"); err != nil {
		t.Fatal(err)
	}
	after := raw(t, path)
	if got := after["retention"]; got != float64(90) {
		t.Errorf("retention = %v after Unset, want 90", got)
	}
	if _, ok := after["workspace"]; ok {
		t.Errorf("workspace survived Unset: %v", after)
	}
}

// Unsetting is a statement about the end state, so asking twice, or asking of a
// file nobody ever wrote, is not a failure.
func TestUnsetIsIdempotentAndForgivesAMissingFile(t *testing.T) {
	_, repo := isolate(t)
	path := repoConfig(repo)

	if err := Unset(path, "workspace"); err != nil {
		t.Fatalf("unset on a missing file: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("unset created a file: %v", err)
	}

	if err := Set(path, "workspace", "acme"); err != nil {
		t.Fatal(err)
	}
	if err := Unset(path, "workspace"); err != nil {
		t.Fatal(err)
	}
	if err := Unset(path, "workspace"); err != nil {
		t.Fatalf("second unset: %v", err)
	}
}

// A key krowk does not understand would sit in the file looking effective and
// never do anything, so it is refused — and the refusal says what would have
// worked.
func TestSetAndUnsetRejectUnknownKeys(t *testing.T) {
	_, repo := isolate(t)
	path := repoConfig(repo)

	for name, err := range map[string]error{
		"Set":   Set(path, "workspce", "acme"),
		"Unset": Unset(path, "workspce"),
	} {
		if err == nil {
			t.Fatalf("%s accepted an unknown key", name)
		}
		if !strings.Contains(err.Error(), "workspace") {
			t.Errorf("%s error %q does not name the valid keys", name, err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a rejected key still wrote a file: %v", err)
	}
}

// raw reads a config file as JSON meant nothing to this package, which is how a
// newer build's keys have to look coming back out.
func raw(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
