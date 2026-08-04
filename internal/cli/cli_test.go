package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// A read-only key is valid, and the output has to say — in both formats — that
// it cannot upload, before a push finds out the hard way.
func TestAuthVerifyWithAReadOnlyKeySaysItCannotUpload(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_TOKEN"] = "krk_ro_readonly"

	code, stdout, stderr := h.run("auth", "verify", "--format=human")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "cannot upload") || !strings.Contains(stdout, "artifacts:write") {
		t.Errorf("human output = %q, want it to say the key cannot upload and name the missing scope", stdout)
	}

	code, stdout, stderr = h.run("auth", "verify", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var e struct {
		OK   bool `json:"ok"`
		Data struct {
			Valid  bool     `json:"valid"`
			Scopes []string `json:"scopes"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if !e.OK || !e.Data.Valid {
		t.Fatalf("a read-only key is still a valid key: %s", stdout)
	}
	if slices.Contains(e.Data.Scopes, "artifacts:write") {
		t.Errorf("scopes = %v, want artifacts:write absent", e.Data.Scopes)
	}
}

// --quiet is documented as "no envelope", for verify like everything else.
func TestAuthVerifyQuietIsTheBareKey(t *testing.T) {
	h := newHarness(t, 0)

	code, stdout, stderr := h.run("auth", "verify", "--quiet")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var key map[string]any
	if err := json.Unmarshal([]byte(stdout), &key); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if _, wrapped := key["ok"]; wrapped {
		t.Errorf("--quiet should drop the envelope, got %s", stdout)
	}
	if key["valid"] != true {
		t.Errorf("quiet output = %s, want the key itself", stdout)
	}
}

// There is no link to a key, so --format url (and markdown) degrade to the
// JSON envelope, as the help text says.
func TestAuthVerifyURLFormatFallsBackToJSON(t *testing.T) {
	h := newHarness(t, 0)

	code, stdout, stderr := h.run("auth", "verify", "--format=url")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var e struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if !e.OK || e.Data["valid"] != true {
		t.Errorf("--format url should fall back to the envelope, got %s", stdout)
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

// The mock accepts anonymous pushes, but a real registry may not; when it says
// no to an unauthenticated client, the fix has to name `auth login`.
func TestAnonymousRejectionPointsAtLogin(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_TOKEN"] = ""

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"no_key","fix":"authenticate","retryable":false}`)
	}))
	t.Cleanup(srv.Close)
	h.env["KROWK_API_URL"] = srv.URL + "/v1"

	code, _, stderr := h.run("push", h.fixture)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	fix, _ := decode(t, stderr).Error["fix"].(string)
	if !strings.Contains(fix, "auth login") {
		t.Errorf("fix = %q, want it to name `krowk auth login`", fix)
	}
}

// A 403 off a presigned storage URL means the upload target went bad, not the
// key; the auth hint must not send the agent chasing `auth verify`.
func TestStorageRejectionDoesNotBlameTheKey(t *testing.T) {
	h := newHarness(t, 0)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/artifacts":
			fmt.Fprintf(w, `{"id":"art_1","uploads":[{"filename":%q,"method":"PUT","url":%q}],"finalize_url":%q}`,
				filepath.Base(h.fixture), srv.URL+"/put", srv.URL+"/v1/artifacts/art_1/finalize")
		case r.Method == http.MethodPut && r.URL.Path == "/put":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":"upload_expired","fix":"start the upload again","retryable":false}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	h.env["KROWK_API_URL"] = srv.URL + "/v1"

	code, _, stderr := h.run("push", h.fixture)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	e := decode(t, stderr)
	if e.Error["error"] != "upload_expired" {
		t.Fatalf("error = %v, want upload_expired", e.Error)
	}
	if fix, _ := e.Error["fix"].(string); strings.Contains(fix, "auth verify") {
		t.Errorf("fix = %q, must not point a storage failure at the key", fix)
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

// send accepts any 2xx, so doctor must report the status that actually
// arrived rather than assuming 200.
func TestDoctorReportsTheStatusThatArrived(t *testing.T) {
	h := newHarness(t, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"valid":true,"key_id":"key_1","workspace":"ws","scopes":["artifacts:write"]}`)
	}))
	t.Cleanup(srv.Close)
	h.env["KROWK_API_URL"] = srv.URL + "/v1"

	_, stdout, _ := h.run("doctor")
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if status, _ := report["api_status"].(string); status != "reachable (HTTP 202)" {
		t.Errorf("api_status = %q, want the 202 the registry sent", status)
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

// A URL that never parses reaches nothing; doctor must not call it reachable.
func TestDoctorSaysUnreachableForAMalformedURL(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_API_URL"] = "notaurl"

	_, stdout, _ := h.run("doctor")
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	status, _ := report["api_status"].(string)
	if !strings.HasPrefix(status, "unreachable") || !strings.Contains(status, "bad_api_url") {
		t.Errorf("api_status = %q, want unreachable — bad_api_url", status)
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
