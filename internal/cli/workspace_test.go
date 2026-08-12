package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/krowkcom/cli/internal/registry"
)

// workspaceHarness is the ordinary harness with one addition: it remembers the
// Authorization header of the last request, because which key a command sent
// is the entire fact these tests exist to check and the registry itself never
// says.
type workspaceHarness struct {
	*harness
	mu   sync.Mutex
	last string
}

func newWorkspaceHarness(t *testing.T) *workspaceHarness {
	t.Helper()
	h := &workspaceHarness{harness: newHarness(t, 0)}

	inner := registry.Handler(0, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only requests that carried a key are recorded: a push is several
		// calls, and the presigned PUT to "storage" carries none, which would
		// otherwise blank the very header the test is about.
		if auth := r.Header.Get("Authorization"); auth != "" {
			h.mu.Lock()
			h.last = auth
			h.mu.Unlock()
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	h.env["KROWK_API_URL"] = server.URL + "/v1"
	return h
}

func (h *workspaceHarness) lastAuthorization() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

// repoDir puts the test inside a fresh git repository, because the repo config
// is discovered from the working directory and these tests must never write
// into the checkout the suite itself runs in.
func repoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return dir
}

// storeKeys seeds the credentials file with a key per named workspace, the
// last one as the default — the state a machine is in after logging into each
// in turn.
func storeKeys(t *testing.T, h *workspaceHarness, workspaces ...string) {
	t.Helper()
	entries := map[string]map[string]string{}
	for _, ws := range workspaces {
		entries[ws] = map[string]string{
			"token": "krowk_sk_" + ws, "key_id": "key_" + ws, "workspace": ws,
		}
	}
	data, err := json.Marshal(map[string]any{
		"default": workspaces[len(workspaces)-1], "workspaces": entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "krowk", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	delete(h.env, "KROWK_TOKEN")
}

func TestConfigSetPinsTheRepositoryAndPushUsesIt(t *testing.T) {
	h := newWorkspaceHarness(t)
	repoDir(t)
	storeKeys(t, h, "ws_acme", "ws_personal")

	code, stdout, stderr := h.run("config", "set", "workspace", "ws_acme", "--json")
	if code != 0 {
		t.Fatalf("config set = %d, stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, ".krowk") {
		t.Errorf("the receipt does not name the repo file: %s", stdout)
	}

	// The pin decides which key the push carries: the default points at
	// ws_personal, and the repo config must outrank it.
	code, _, stderr = h.run("push", h.fixture, "--json")
	if code != 0 {
		t.Fatalf("push in a pinned repo = %d, stderr %s", code, stderr)
	}
	if got := h.lastAuthorization(); got != "Bearer krowk_sk_ws_acme" {
		t.Errorf("push sent %q, want the pinned workspace's key", got)
	}
}

func TestWorkspaceResolutionPrecedence(t *testing.T) {
	h := newWorkspaceHarness(t)
	dir := repoDir(t)
	storeKeys(t, h, "ws_repo", "ws_env", "ws_flag", "ws_default")

	if err := os.MkdirAll(filepath.Join(dir, ".krowk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".krowk", "config.json"),
		[]byte(`{"workspace": "ws_repo"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// The repo config outranks the stored default…
	if code, _, stderr := h.run("push", h.fixture, "--json"); code != 0 {
		t.Fatalf("push = %d, stderr %s", code, stderr)
	} else if got := h.lastAuthorization(); got != "Bearer krowk_sk_ws_repo" {
		t.Errorf("repo config: sent %q", got)
	}

	// …the environment outranks the repo config…
	h.env["KROWK_WORKSPACE"] = "ws_env"
	if code, _, stderr := h.run("push", h.fixture, "--json"); code != 0 {
		t.Fatalf("push = %d, stderr %s", code, stderr)
	} else if got := h.lastAuthorization(); got != "Bearer krowk_sk_ws_env" {
		t.Errorf("env: sent %q", got)
	}

	// …and the flag outranks everything.
	if code, _, stderr := h.run("push", h.fixture, "--workspace", "ws_flag", "--json"); code != 0 {
		t.Fatalf("push = %d, stderr %s", code, stderr)
	} else if got := h.lastAuthorization(); got != "Bearer krowk_sk_ws_flag" {
		t.Errorf("flag: sent %q", got)
	}
}

func TestANamedWorkspaceWithNoKeyRefusesRatherThanUploadingAnonymously(t *testing.T) {
	h := newWorkspaceHarness(t)
	repoDir(t)
	storeKeys(t, h, "ws_acme")

	code, _, stderr := h.run("push", h.fixture, "--workspace", "ws_nowhere", "--json")
	if code != exitAuth {
		t.Errorf("push --workspace with no key = %d, want %d", code, exitAuth)
	}
	if !strings.Contains(stderr, "no_key_for_workspace") {
		t.Errorf("the refusal does not name itself: %s", stderr)
	}
	// The one key that is stored is worth naming — the fix is a copy-paste,
	// not a guess at spelling.
	if !strings.Contains(stderr, "ws_acme") {
		t.Errorf("the refusal does not say what is stored: %s", stderr)
	}
}

func TestKrowkTokenOutranksAPinnedWorkspace(t *testing.T) {
	h := newWorkspaceHarness(t)
	repoDir(t)
	storeKeys(t, h, "ws_acme")
	h.env["KROWK_TOKEN"] = "krowk_sk_from_ci"
	h.env["KROWK_WORKSPACE"] = "ws_acme"

	// CI injects a token into a checkout whose config names a workspace this
	// machine never logged into; the token is the key that was meant.
	if code, _, stderr := h.run("push", h.fixture, "--json"); code != 0 {
		t.Fatalf("push = %d, stderr %s", code, stderr)
	} else if got := h.lastAuthorization(); got != "Bearer krowk_sk_from_ci" {
		t.Errorf("sent %q, want the environment's key", got)
	}
}

func TestAMalformedConfigFailsRatherThanBeingShruggedOff(t *testing.T) {
	h := newWorkspaceHarness(t)
	dir := repoDir(t)
	if err := os.MkdirAll(filepath.Join(dir, ".krowk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".krowk", "config.json"),
		[]byte(`{"workspace": `), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := h.run("push", h.fixture, "--json")
	if code != exitUsage {
		t.Errorf("push over a broken config = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "bad_config") {
		t.Errorf("the failure does not name itself: %s", stderr)
	}
}

func TestConfigSetOutsideARepositoryPointsAtGlobal(t *testing.T) {
	h := newWorkspaceHarness(t)
	t.Chdir(t.TempDir())

	code, _, stderr := h.run("config", "set", "workspace", "ws_acme", "--json")
	if code != exitUsage {
		t.Errorf("config set outside a repo = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "--global") {
		t.Errorf("the refusal does not offer --global: %s", stderr)
	}

	if code, _, stderr := h.run("config", "set", "workspace", "ws_acme", "--global", "--json"); code != 0 {
		t.Fatalf("config set --global = %d, stderr %s", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "krowk", "config.json"))
	if err != nil {
		t.Fatalf("the global config was not written: %v", err)
	}
	if !strings.Contains(string(data), "ws_acme") {
		t.Errorf("global config = %s", data)
	}
}

func TestConfigSetRejectsAKeyThisBuildDoesNotKnow(t *testing.T) {
	h := newWorkspaceHarness(t)
	repoDir(t)

	code, _, stderr := h.run("config", "set", "workspce", "ws_acme", "--json")
	if code != exitUsage {
		t.Errorf("config set with a typo'd key = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "unknown_config_key") || !strings.Contains(stderr, "workspace") {
		t.Errorf("the refusal does not name the valid keys: %s", stderr)
	}
}

func TestConfigShowSaysWhichLayerWon(t *testing.T) {
	h := newWorkspaceHarness(t)
	dir := repoDir(t)
	if err := os.MkdirAll(filepath.Join(dir, ".krowk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".krowk", "config.json"),
		[]byte(`{"workspace": "ws_acme"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := h.run("config", "show", "--json")
	if code != 0 {
		t.Fatalf("config show = %d, stderr %s", code, stderr)
	}
	var envelope struct {
		Data struct {
			Workspace string            `json:"workspace"`
			Sources   map[string]string `json:"sources"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("config show did not answer JSON: %v\n%s", err, stdout)
	}
	if envelope.Data.Workspace != "ws_acme" {
		t.Errorf("workspace = %q", envelope.Data.Workspace)
	}
	if got := envelope.Data.Sources["workspace"]; got != "repo config" {
		t.Errorf("source = %q, want the repo config named", got)
	}
}

func TestWorkspacesListNamesTheStoreAndTheResolution(t *testing.T) {
	h := newWorkspaceHarness(t)
	repoDir(t)
	storeKeys(t, h, "ws_acme", "ws_personal")

	code, stdout, stderr := h.run("workspaces", "list", "--json")
	if code != 0 {
		t.Fatalf("workspaces list = %d, stderr %s", code, stderr)
	}
	var envelope struct {
		Data struct {
			Stored []struct {
				Name    string `json:"name"`
				Default bool   `json:"default"`
			} `json:"stored"`
			Resolved string `json:"resolved"`
			Source   string `json:"source"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("workspaces list did not answer JSON: %v\n%s", err, stdout)
	}
	if len(envelope.Data.Stored) != 2 {
		t.Fatalf("stored = %+v, want both keys", envelope.Data.Stored)
	}
	// Nothing named a workspace here, so the stored default answers — and the
	// listing says so rather than leaving the resolution blank.
	if envelope.Data.Resolved != "ws_personal" || envelope.Data.Source != "stored default" {
		t.Errorf("resolved = %q (%q), want the stored default named",
			envelope.Data.Resolved, envelope.Data.Source)
	}

	// The bare group runs the listing too: `krowk workspaces` is the question
	// as a person types it.
	if code, _, _ := h.run("workspaces", "--json"); code != 0 {
		t.Errorf("bare `krowk workspaces` = %d, want the listing", code)
	}
}

// TestLoginRecordsTheWorkspaceTitleForTheListing follows the workspace's human
// name from the registry through login into `workspaces list` — the whole
// journey it exists for, since a picker built from the store has nothing else
// to offer a person but slugs.
func TestLoginRecordsTheWorkspaceTitleForTheListing(t *testing.T) {
	h := newWorkspaceHarness(t)
	repoDir(t)
	delete(h.env, "KROWK_TOKEN")

	if code, _, stderr := h.run("auth", "login", "--token", "krowk_sk_titled", "--json"); code != 0 {
		t.Fatalf("auth login = %d, stderr %s", code, stderr)
	}

	code, stdout, stderr := h.run("workspaces", "list", "--json")
	if code != 0 {
		t.Fatalf("workspaces list = %d, stderr %s", code, stderr)
	}
	var envelope struct {
		Data struct {
			Stored []struct {
				Name          string `json:"name"`
				WorkspaceName string `json:"workspace_name"`
			} `json:"stored"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("workspaces list did not answer JSON: %v\n%s", err, stdout)
	}
	if len(envelope.Data.Stored) != 1 {
		t.Fatalf("stored = %+v, want the one key just logged in", envelope.Data.Stored)
	}
	got := envelope.Data.Stored[0]
	if !strings.HasPrefix(got.Name, "ws_") {
		t.Errorf("entry name = %q, want the ws_ slug the registry reported", got.Name)
	}
	if got.WorkspaceName != "Local workspace" {
		t.Errorf("workspace_name = %q, want the title the registry sent at login", got.WorkspaceName)
	}
}

// TestThePickerNeverAppearsOffATerminal holds the interactive fallback to its
// gate: everywhere these tests run — no TTY, JSON asked for — a missing
// argument must stay the immediate error it always was, because a prompt shown
// to an agent or a pipe is not a question, it is a hang.
func TestThePickerNeverAppearsOffATerminal(t *testing.T) {
	h := newWorkspaceHarness(t)
	repoDir(t)
	storeKeys(t, h, "ws_acme", "ws_personal")

	code, _, stderr := h.run("workspaces", "use", "--json")
	if code != exitUsage {
		t.Errorf("workspaces use with no name off a TTY = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "no_workspace") {
		t.Errorf("the refusal does not name itself: %s", stderr)
	}

	code, _, stderr = h.run("config", "set", "workspace", "--json")
	if code != exitUsage {
		t.Errorf("config set with no value off a TTY = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "missing_argument") {
		t.Errorf("the refusal does not name itself: %s", stderr)
	}
}

func TestWorkspacesUseRepointsTheDefault(t *testing.T) {
	h := newWorkspaceHarness(t)
	repoDir(t)
	storeKeys(t, h, "ws_acme", "ws_personal")

	if code, _, stderr := h.run("workspaces", "use", "ws_acme", "--json"); code != 0 {
		t.Fatalf("workspaces use = %d, stderr %s", code, stderr)
	}
	// The pointer moved, so an unadorned push now carries the other key.
	if code, _, stderr := h.run("push", h.fixture, "--json"); code != 0 {
		t.Fatalf("push = %d, stderr %s", code, stderr)
	} else if got := h.lastAuthorization(); got != "Bearer krowk_sk_ws_acme" {
		t.Errorf("sent %q, want the new default's key", got)
	}

	code, _, stderr := h.run("workspaces", "use", "ws_nowhere", "--json")
	if code != exitUsage {
		t.Errorf("use with an unknown name = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "unknown_workspace") {
		t.Errorf("the refusal does not name itself: %s", stderr)
	}
}
