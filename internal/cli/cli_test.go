package cli

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/registry"
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

func TestHelpAndVersion(t *testing.T) {
	h := newHarness(t, 0)

	if code, stdout, _ := h.run("--version"); code != 0 || strings.TrimSpace(stdout) != Version {
		t.Errorf("--version = %q (exit %d)", stdout, code)
	}
	if code, stdout, _ := h.run(); code != 0 || !strings.Contains(stdout, "uploads create") {
		t.Errorf("bare invocation should print help, got %q (exit %d)", stdout, code)
	}
}
