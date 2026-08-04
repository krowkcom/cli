package cli

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/registry"
	"github.com/krowkcom/cli/internal/runctx"
)

// harness runs the CLI against a throwaway registry, the way a user would.
type harness struct {
	t       *testing.T
	env     map[string]string
	fixture string
}

func newHarness(t *testing.T, limitBytes int64) *harness {
	t.Helper()

	server := httptest.NewServer(registry.Handler(limitBytes, ""))
	t.Cleanup(server.Close)

	fixture := filepath.Join(t.TempDir(), "checkout-after.png")
	if err := os.WriteFile(fixture, []byte("fake png bytes for the test"), 0o600); err != nil {
		t.Fatal(err)
	}

	return &harness{
		t:       t,
		env:     map[string]string{"KROWK_API_URL": server.URL + "/v1", "KROWK_TOKEN": "krk_test"},
		fixture: fixture,
	}
}

func (h *harness) run(args ...string) (code int, stdout, stderr string) {
	h.t.Helper()
	var out, errOut bytes.Buffer
	code = Run(args, &out, &errOut, func(k string) string { return h.env[k] }, false)
	return code, out.String(), errOut.String()
}

// envelope is the JSON shape every non-human result comes back in.
type envelope struct {
	OK    bool           `json:"ok"`
	Data  artifact       `json:"data"`
	Error map[string]any `json:"error"`
}

type artifact struct {
	ID       string         `json:"id"`
	URL      string         `json:"url"`
	Bytes    int64          `json:"bytes"`
	Files    []any          `json:"files"`
	Metadata map[string]any `json:"metadata"`
}

func decode(t *testing.T, s string) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal([]byte(s), &e); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return e
}

func TestUploadsCreateReturnsURLAndRoundTripsMetadata(t *testing.T) {
	h := newHarness(t, 0)

	code, stdout, stderr := h.run(
		"uploads", "create", h.fixture,
		"--pull-request=https://github.com/acme/storefront/pull/412",
		"--reference=https://linear.app/acme/issue/ENG-9",
		"--reference=https://sentry.io/issues/1",
		"--session=sess_abc123",
		"--title=Checkout — mobile",
	)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}

	e := decode(t, stdout)
	if !e.OK {
		t.Fatalf("not ok: %s", stdout)
	}
	if want := "/a/" + e.Data.ID; !strings.HasSuffix(e.Data.URL, want) {
		t.Errorf("URL %q does not end in %q", e.Data.URL, want)
	}

	meta := e.Data.Metadata
	if got := meta["pull_request"]; got != "https://github.com/acme/storefront/pull/412" {
		t.Errorf("pull_request = %v", got)
	}
	if got := meta["session"]; got != "sess_abc123" {
		t.Errorf("session = %v", got)
	}
	if got := meta["client"]; got != "krowk-cli/"+Version {
		t.Errorf("client = %v", got)
	}
	refs, _ := meta["reference"].([]any)
	if len(refs) != 2 || refs[1] != "https://sentry.io/issues/1" {
		t.Errorf("reference = %v, want both links in order", meta["reference"])
	}
	// Detected without a flag: the CLI's own repository.
	if commit, _ := meta["commit"].(string); !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		t.Errorf("commit = %q, want a detected SHA", commit)
	}
}

func TestSameBytesUploadToTheSameURL(t *testing.T) {
	h := newHarness(t, 0)

	_, first, _ := h.run("push", h.fixture)
	_, second, _ := h.run("push", h.fixture, "--session=a-later-retry")

	if a, b := decode(t, first).Data.URL, decode(t, second).Data.URL; a != b {
		t.Errorf("retry produced a second link: %q then %q", a, b)
	}
}

func TestMultipleFilesLandUnderOneArtifact(t *testing.T) {
	h := newHarness(t, 0)
	second := filepath.Join(filepath.Dir(h.fixture), "checkout-before.png")
	if err := os.WriteFile(second, []byte("the other fake png"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := h.run("push", h.fixture, second)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	e := decode(t, stdout)
	if len(e.Data.Files) != 2 {
		t.Errorf("files = %v, want both under one artifact", e.Data.Files)
	}
	if e.Data.Bytes != int64(len("fake png bytes for the test")+len("the other fake png")) {
		t.Errorf("bytes = %d, want the sum of both files", e.Data.Bytes)
	}
}

func TestFlagsMayFollowFilenames(t *testing.T) {
	h := newHarness(t, 0)

	code, stdout, stderr := h.run("uploads", "create", h.fixture, "--session=after-the-file")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if got := decode(t, stdout).Data.Metadata["session"]; got != "after-the-file" {
		t.Errorf("session = %v", got)
	}
}

func TestMarkdownFormatIsPasteReady(t *testing.T) {
	h := newHarness(t, 0)

	_, stdout, _ := h.run("uploads", "create", h.fixture, "--title=Checkout", "--format=markdown")

	want := regexp.MustCompile(`^\[!\[Checkout]\(http.+/preview\.png\)]\(http.+\)$`)
	if !want.MatchString(strings.TrimSpace(stdout)) {
		t.Errorf("markdown = %q", stdout)
	}
}

// Slack renders no markdown image embeds but unfurls a bare URL itself, so the
// plain link has to be obtainable on its own.
func TestURLFormatIsJustTheLink(t *testing.T) {
	h := newHarness(t, 0)

	code, stdout, stderr := h.run("push", h.fixture, "--format=url")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	got := strings.TrimSpace(stdout)
	if !regexp.MustCompile(`^http://127\.0\.0\.1:\d+/a/[0-9a-f]+$`).MatchString(got) {
		t.Errorf("url = %q, want the bare link and nothing else", got)
	}
}

func TestJSONOutputCarriesBothPasteForms(t *testing.T) {
	h := newHarness(t, 0)

	_, stdout, _ := h.run("push", h.fixture, "--title=Checkout")

	var e struct {
		Paste struct {
			Markdown string `json:"markdown"`
			URL      string `json:"url"`
		} `json:"paste"`
		Data artifact `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if !strings.HasPrefix(e.Paste.Markdown, "[![Checkout](") {
		t.Errorf("paste.markdown = %q, want the image embed", e.Paste.Markdown)
	}
	if e.Paste.URL != e.Data.URL {
		t.Errorf("paste.url = %q, want the artifact URL %q", e.Paste.URL, e.Data.URL)
	}
}

func TestMissingFileFailsBeforeUploading(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_API_URL"] = "http://127.0.0.1:1/v1" // any upload attempt would fail loudly

	code, _, stderr := h.run("uploads", "create", "nope.png")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	e := decode(t, stderr)
	if e.Error["error"] != "file_unreadable" || e.Error["fix"] == nil {
		t.Errorf("error = %v, want file_unreadable with a fix", e.Error)
	}
}

func TestRejectedUploadSurfacesLimitAndFix(t *testing.T) {
	h := newHarness(t, 10) // 10-byte limit, so the fixture is too large

	code, _, stderr := h.run("uploads", "create", h.fixture)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	e := decode(t, stderr)
	if e.Error["error"] != "artifact_too_large" {
		t.Fatalf("error = %v", e.Error)
	}
	if e.Error["limit_bytes"] != float64(10) || e.Error["status"] != float64(413) {
		t.Errorf("body = %v, want the limit and the status", e.Error)
	}
	if e.Error["retryable"] != false {
		t.Errorf("retryable = %v, want false so the agent stops", e.Error["retryable"])
	}
}

func TestUnreachableRegistryIsAnActionableError(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_API_URL"] = "http://127.0.0.1:1/v1"

	code, _, stderr := h.run("uploads", "create", h.fixture)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	e := decode(t, stderr)
	if e.Error["error"] != "network_unreachable" || !strings.Contains(e.Error["fix"].(string), "KROWK_API_URL") {
		t.Errorf("error = %v", e.Error)
	}
}

func TestUnknownCommandIsReadable(t *testing.T) {
	h := newHarness(t, 0)

	code, _, stderr := h.run("uploads", "yeet")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := decode(t, stderr).Error["error"]; got != "unknown_command" {
		t.Errorf("error = %v", got)
	}
}

func TestAuthVerifyReportsTheKeyAndItsScopes(t *testing.T) {
	h := newHarness(t, 0)

	code, stdout, stderr := h.run("auth", "verify")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var e struct {
		OK   bool `json:"ok"`
		Data struct {
			Valid     bool     `json:"valid"`
			KeyID     string   `json:"key_id"`
			Workspace string   `json:"workspace"`
			Scopes    []string `json:"scopes"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if !e.OK || !e.Data.Valid || e.Data.KeyID == "" {
		t.Fatalf("verify = %s", stdout)
	}
	if !slices.Contains(e.Data.Scopes, "artifacts:write") {
		t.Errorf("scopes = %v, want artifacts:write", e.Data.Scopes)
	}
}

func TestAuthVerifyWithoutAKeySaysSoWithoutCallingOut(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_TOKEN"] = ""
	h.env["KROWK_API_URL"] = "http://127.0.0.1:1/v1" // any call would fail loudly

	code, _, stderr := h.run("auth", "verify")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := decode(t, stderr).Error["error"]; got != "not_authenticated" {
		t.Errorf("error = %v, want not_authenticated", got)
	}
}

func TestAuthVerifyRejectsAKeyTheRegistryDoesNotKnow(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_TOKEN"] = "hunter2"

	code, _, stderr := h.run("auth", "verify")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := decode(t, stderr).Error["error"]; got != "invalid_key" {
		t.Errorf("error = %v, want invalid_key", got)
	}
}

// A key that cannot write should fail at the manifest, and the message should
// point at the self-check rather than leaving the agent to guess.
func TestReadOnlyKeyFailsThePushWithAPointerToVerify(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_TOKEN"] = "krk_ro_readonly"

	code, _, stderr := h.run("push", h.fixture)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	e := decode(t, stderr)
	if e.Error["error"] != "insufficient_scope" {
		t.Fatalf("error = %v, want insufficient_scope", e.Error)
	}
	fix, _ := e.Error["fix"].(string)
	if !strings.Contains(fix, "krowk auth verify") {
		t.Errorf("fix = %q, want it to name the self-check", fix)
	}
}

func TestAnonymousPushIsAllowed(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_TOKEN"] = ""

	// Anonymous uploads are a supported path, not a rejected one; the auth hint
	// only appears when the registry actually turns the call down.
	if code, _, stderr := h.run("push", h.fixture); code != 0 {
		t.Fatalf("anonymous push should work: exit %d, %s", code, stderr)
	}
}

func TestAnonymousPushPrintsAClaimURL(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_TOKEN"] = ""

	code, stdout, stderr := h.run("push", h.fixture)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}

	var e struct {
		Data struct {
			URL       string `json:"url"`
			Anonymous bool   `json:"anonymous"`
			ClaimURL  string `json:"claim_url"`
		} `json:"data"`
		Paste struct {
			Markdown string `json:"markdown"`
			URL      string `json:"url"`
		} `json:"paste"`
		Breadcrumbs []struct {
			Action string `json:"action"`
			Cmd    string `json:"cmd"`
		} `json:"breadcrumbs"`
	}
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if !e.Data.Anonymous || e.Data.ClaimURL == "" {
		t.Fatalf("anonymous push = %s, want it flagged with a claim URL", stdout)
	}

	// The claim URL is a capability: anyone holding it can adopt the upload, so
	// it must not ride along in anything meant for a pull request comment.
	if strings.Contains(e.Paste.Markdown, e.Data.ClaimURL) || strings.Contains(e.Paste.URL, e.Data.ClaimURL) {
		t.Errorf("the claim URL leaked into the paste output: %+v", e.Paste)
	}

	var claim bool
	for _, b := range e.Breadcrumbs {
		if b.Action == "claim" {
			claim = true
		}
	}
	if !claim {
		t.Errorf("breadcrumbs = %+v, want a claim step", e.Breadcrumbs)
	}
}

func TestKeyedPushIsNotClaimable(t *testing.T) {
	h := newHarness(t, 0)

	_, stdout, _ := h.run("push", h.fixture)
	var e struct {
		Data struct {
			Anonymous bool   `json:"anonymous"`
			ClaimURL  string `json:"claim_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatal(err)
	}
	if e.Data.Anonymous || e.Data.ClaimURL != "" {
		t.Errorf("a keyed push should already belong to the workspace: %s", stdout)
	}
}

func TestDoctorReportsTheKeyAndReachability(t *testing.T) {
	h := newHarness(t, 0)

	code, stdout, stderr := h.run("doctor")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if status, _ := report["api_status"].(string); !strings.HasPrefix(status, "reachable") {
		t.Errorf("api_status = %v, want reachable", report["api_status"])
	}
	if key, _ := report["key"].(string); !strings.Contains(key, "artifacts:write") {
		t.Errorf("key = %v, want the scopes summarised", report["key"])
	}

	// No key at all reads differently from a rejected one.
	h.env["KROWK_TOKEN"] = ""
	_, stdout, _ = h.run("doctor")
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if key, _ := report["key"].(string); !strings.Contains(key, "anonymous") {
		t.Errorf("key = %v, want it to say uploads will be anonymous", report["key"])
	}
}

func TestDoctorSaysUnreachableWhenNothingIsListening(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_API_URL"] = "http://127.0.0.1:1/v1"

	_, stdout, _ := h.run("doctor")
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if status, _ := report["api_status"].(string); !strings.HasPrefix(status, "unreachable") {
		t.Errorf("api_status = %v, want unreachable", report["api_status"])
	}
}

func TestDoctorLabelsWhichRegistryItIsTalkingTo(t *testing.T) {
	h := newHarness(t, 0)

	mode := func(args ...string) string {
		t.Helper()
		_, stdout, _ := h.run(args...)
		var report map[string]any
		if err := json.Unmarshal([]byte(stdout), &report); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, stdout)
		}
		got, _ := report["registry"].(string)
		return got
	}

	// The harness points KROWK_API_URL at a throwaway server.
	if got := mode("doctor"); got != "custom (KROWK_API_URL)" {
		t.Errorf("registry = %q, want it flagged as custom", got)
	}

	// --dev overrides that, and says so.
	if got := mode("doctor", "--dev"); got != "local" {
		t.Errorf("registry with --dev = %q, want local", got)
	}

	h.env["KROWK_API_URL"] = ""
	if got := mode("doctor"); got != "production" {
		t.Errorf("registry with nothing set = %q, want production", got)
	}
	h.env["KROWK_DEV"] = "yes"
	if got := mode("doctor"); got != "local" {
		t.Errorf("registry with KROWK_DEV = %q, want local", got)
	}
}

// Every command builds its client through one helper, so checking the helper
// checks the wiring — without depending on whether :8787 happens to be free.
func TestDevFlagRedirectsTheClient(t *testing.T) {
	env := func(m map[string]string) runctx.Env {
		return func(k string) string { return m[k] }
	}
	dev := strings.TrimRight(api.DevBaseURL, "/")

	if got := newClient(flags{dev: true}, env(nil)).BaseURL; got != dev {
		t.Errorf("--dev = %q, want %q", got, dev)
	}
	// A flag typed on the command line beats an ambient variable.
	staging := env(map[string]string{"KROWK_API_URL": "https://staging/v1"})
	if got := newClient(flags{dev: true}, staging).BaseURL; got != dev {
		t.Errorf("--dev with KROWK_API_URL set = %q, want %q", got, dev)
	}
	if got := newClient(flags{}, staging).BaseURL; got != "https://staging/v1" {
		t.Errorf("KROWK_API_URL = %q", got)
	}
	if got := newClient(flags{}, env(nil)).BaseURL; got != strings.TrimRight(api.DefaultBaseURL, "/") {
		t.Errorf("default = %q, want the public registry", got)
	}
}

func TestLocalBaseTurnsAListenAddressIntoAURL(t *testing.T) {
	if got := localBase(":8787"); got != "http://localhost:8787" {
		t.Errorf("localBase(:8787) = %q", got)
	}
	if got := localBase("127.0.0.1:9000"); got != "http://127.0.0.1:9000" {
		t.Errorf("localBase(127.0.0.1:9000) = %q", got)
	}
}

func TestHelpMentionsTheLocalRegistry(t *testing.T) {
	h := newHarness(t, 0)

	_, stdout, _ := h.run("--help")
	for _, want := range []string{"registry serve", "--dev", "KROWK_DEV", "http://localhost:8787/v1"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help is missing %q", want)
		}
	}
}

func TestHelpAndVersion(t *testing.T) {
	h := newHarness(t, 0)

	if code, stdout, _ := h.run("--version"); code != 0 || strings.TrimSpace(stdout) != Version {
		t.Errorf("--version = %q (exit %d)", stdout, code)
	}
	if code, stdout, _ := h.run(); code != 0 || !strings.Contains(stdout, "uploads create") {
		t.Errorf("bare invocation should print help, got %q (exit %d)", stdout, code)
	}
}
