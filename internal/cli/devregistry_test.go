package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/runctx"
)

func TestAuthVerifyReportsTheKeyAndItsWorkspace(t *testing.T) {
	h := newHarness(t, 0)

	code, stdout, stderr := h.run("auth", "verify")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var e struct {
		OK   bool `json:"ok"`
		Data struct {
			KeyID     string `json:"key_id"`
			Workspace string `json:"workspace"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if !e.OK || e.Data.KeyID == "" {
		t.Fatalf("verify = %s", stdout)
	}
	// The workspace is what verifying is for: it says where an upload would land.
	if e.Data.Workspace == "" {
		t.Errorf("verify named no workspace: %s", stdout)
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
	if key["key_id"] == nil {
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
	if !e.OK || e.Data["key_id"] == nil {
		t.Errorf("--format url should fall back to the envelope, got %s", stdout)
	}
}

func TestAuthVerifyWithoutAKeySaysSoWithoutCallingOut(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_TOKEN"] = ""
	h.env["KROWK_API_URL"] = "http://127.0.0.1:1/v1" // any call would fail loudly

	code, _, stderr := h.run("auth", "verify")
	if code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
	if got := decode(t, stderr).Error["error"]; got != "not_authenticated" {
		t.Errorf("error = %v, want not_authenticated", got)
	}
}

// The registry's answer is what counts, not the token's shape. A key it does not
// know is a 401 — the same refusal every other endpoint gives.
func TestAuthVerifyRejectsAKeyTheRegistryDoesNotKnow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"Provide a valid API key."}}`)
	}))
	t.Cleanup(srv.Close)

	h := newHarness(t, 0)
	h.env["KROWK_API_URL"] = srv.URL + "/v1"
	h.env["KROWK_TOKEN"] = "hunter2"

	code, _, stderr := h.run("auth", "verify")
	if code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
	if got := decode(t, stderr).Error["error"]; got != "unauthorized" {
		t.Errorf("error = %v, want unauthorized", got)
	}
}

// A 200 is not a yes on its own: the endpoint has to have named a key, or the
// answer came from something that is not the registry.
func TestAuthVerifyRejectsAnAnswerThatNamesNoKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"welcome":"to the website"}`)
	}))
	t.Cleanup(srv.Close)

	h := newHarness(t, 0)
	h.env["KROWK_API_URL"] = srv.URL + "/v1"
	h.env["KROWK_TOKEN"] = "hunter2"

	code, _, stderr := h.run("auth", "verify")
	if code != 7 {
		t.Fatalf("exit %d, want 7", code)
	}
	if got := decode(t, stderr).Error["error"]; got != "malformed_response" {
		t.Errorf("error = %v, want malformed_response", got)
	}
}

// A rejected key is exactly the moment the self-check earns its keep, so the
// fix points at `auth verify` — and a keyless rejection points at `auth login`
// instead, since there is no key to verify.
func TestRejectedKeyOnPushPointsAtVerify(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"Provide a valid API key."}}`)
	}))
	t.Cleanup(srv.Close)

	h := newHarness(t, 0)
	h.env["KROWK_API_URL"] = srv.URL + "/v1"

	body := h.fails("push", h.fixture)
	fix, _ := body["fix"].(string)
	if body["error"] != "unauthorized" || !strings.Contains(fix, "auth verify") {
		t.Errorf("keyed 401 fix = %q, want it to point at `krowk auth verify`", fix)
	}

	// The self-check itself is exempt: telling `auth verify` to run
	// `auth verify` is a loop, not a fix.
	body = h.fails("auth", "verify")
	fix, _ = body["fix"].(string)
	if strings.Contains(fix, "auth verify") {
		t.Errorf("verify's own 401 fix = %q, must not point back at the command that just ran", fix)
	}

	// No key sent: nothing to verify, so the advice is to log in.
	body = h.anonymous().fails("push", h.fixture)
	fix, _ = body["fix"].(string)
	if body["error"] != "unauthorized" || !strings.Contains(fix, "auth login") {
		t.Errorf("keyless 401 fix = %q, want it to point at `krowk auth login`", fix)
	}
	if strings.Contains(fix, "auth verify") {
		t.Errorf("keyless 401 fix = %q, must not suggest verifying a key that does not exist", fix)
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
	// The summary names the key and the workspace it acts in, which is what
	// someone reading doctor's output needs to confirm.
	if key, _ := report["key"].(string); !strings.Contains(key, "ws_") {
		t.Errorf("key = %v, want the workspace summarised", report["key"])
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

// An HTTP response of any status proves reachability, so doctor reports the
// status that actually arrived rather than assuming the probe's 200.
func TestDoctorReportsTheStatusThatArrived(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"no_such_endpoint","message":"No such endpoint."}}`)
	}))
	t.Cleanup(srv.Close)

	h := newHarness(t, 0)
	h.env["KROWK_API_URL"] = srv.URL + "/v1"

	_, stdout, _ := h.run("doctor")
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if status, _ := report["api_status"].(string); !strings.HasPrefix(status, "reachable (HTTP 404)") {
		t.Errorf("api_status = %q, want the 404 the server sent", status)
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

// A URL that never parses into anything dialable reaches nothing; doctor must
// not call it reachable.
func TestDoctorSaysUnreachableForAMalformedURL(t *testing.T) {
	h := newHarness(t, 0)
	h.env["KROWK_API_URL"] = "notaurl"

	_, stdout, _ := h.run("doctor")
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if status, _ := report["api_status"].(string); !strings.HasPrefix(status, "unreachable") {
		t.Errorf("api_status = %q, want unreachable", status)
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

	h.env["KROWK_DEV"] = "yes"
	h.env["KROWK_API_URL"] = ""
	// Keyless too, so a doctor pointed at nothing makes no verify call.
	h.env["KROWK_TOKEN"] = ""
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
	baseURL := func(f flags, e runctx.Env) string {
		t.Helper()
		client, err := newClient(f, e)
		if err != nil {
			t.Fatalf("newClient: %v", err)
		}
		return client.BaseURL
	}

	if got := baseURL(flags{dev: true}, env(nil)); got != dev {
		t.Errorf("--dev = %q, want %q", got, dev)
	}
	// A flag typed on the command line beats an ambient variable.
	staging := env(map[string]string{"KROWK_API_URL": "https://staging/v1"})
	if got := baseURL(flags{dev: true}, staging); got != dev {
		t.Errorf("--dev with KROWK_API_URL set = %q, want %q", got, dev)
	}
	if got := baseURL(flags{}, staging); got != "https://staging/v1" {
		t.Errorf("KROWK_API_URL = %q", got)
	}
	if got := baseURL(flags{}, env(nil)); got != strings.TrimRight(api.DefaultBaseURL, "/") {
		t.Errorf("default = %q, want the public registry", got)
	}
}

// The local stand-in registry is no longer a CLI command — it moved to
// `go run ./internal/devregistry`, so no shipped binary offers to host uploads.
// The help still teaches --dev, which is the part callers use.
func TestHelpMentionsTheLocalRegistry(t *testing.T) {
	h := newHarness(t, 0)

	_, stdout, _ := h.run("--help")
	for _, want := range []string{"--dev", "KROWK_DEV", "http://localhost:8787/v1"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help is missing %q", want)
		}
	}
	if strings.Contains(stdout, "registry serve") {
		t.Errorf("help still advertises `registry serve`:\n%s", stdout)
	}
}
