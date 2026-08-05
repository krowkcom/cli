package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/api"
)

// loginHarness is the harness without a key in the environment, and the path
// login will write to. KROWK_TOKEN would otherwise outrank everything stored,
// which is the one thing these tests are trying to observe. The config
// directory is already a scratch one — newHarness gives every test its own.
func loginHarness(t *testing.T) (*harness, string) {
	t.Helper()
	return newHarness(t, 0).anonymous(), api.CredentialsPath()
}

// stored reads the credentials file the way the next command would.
func stored(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no credentials at %s: %v", path, err)
	}
	var c map[string]any
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("credentials are not JSON: %v\n%s", err, data)
	}
	return c
}

// Login asks the registry who the key is before writing it down, and keeps the
// answer. The round trip is what makes every later command able to say where an
// upload lands without making one of its own.
func TestAuthLoginVerifiesTheKeyAndRecordsWhatCameBack(t *testing.T) {
	h, path := loginHarness(t)

	code, stdout, stderr := h.run("auth", "login", "--token", "krowk_sk_pasted")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}

	c := stored(t, path)
	if c["token"] != "krowk_sk_pasted" {
		t.Errorf("token = %v, want the pasted key", c["token"])
	}
	if c["key_id"] == nil || c["key_id"] == "" {
		t.Errorf("credentials name no key: %v", c)
	}
	workspace, _ := c["workspace"].(string)
	if !strings.HasPrefix(workspace, "ws_") {
		t.Errorf("workspace = %v, want the one the registry named", c["workspace"])
	}
	// Someone logging in is about to push. Saying where that lands is the
	// difference between a confirmation and a receipt.
	if !strings.Contains(stdout, workspace) {
		t.Errorf("login output does not say where uploads land:\n%s", stdout)
	}
}

// A key the registry disowns is not worth keeping, and writing it over a
// working one would leave the machine worse off than never having logged in.
func TestAuthLoginRefusesAKeyTheRegistryRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"Provide a valid API key."}}`)
	}))
	t.Cleanup(srv.Close)

	h, path := loginHarness(t)
	if _, err := api.SaveCredentials("krowk_sk_working", api.Identity{
		KeyID: "key_good", Workspace: "ws_acme",
	}); err != nil {
		t.Fatal(err)
	}
	h.env["KROWK_API_URL"] = srv.URL + "/v1"

	body := h.fails("auth", "login", "--token", "krowk_sk_typo")
	if body["error"] != "unauthorized" {
		t.Errorf("error = %v, want unauthorized", body["error"])
	}
	// The standing advice for a rejected key is to log in — which is the command
	// that just failed. Telling someone to retry what they are doing is not a fix.
	if fix, _ := body["fix"].(string); strings.Contains(fix, "auth login") {
		t.Errorf("fix = %q, which is the command that just failed", fix)
	}

	c := stored(t, path)
	if c["token"] != "krowk_sk_working" || c["key_id"] != "key_good" {
		t.Errorf("a rejected login overwrote the working key: %v", c)
	}
}

// An unreachable registry says nothing about the key. Logging in on a plane and
// pushing after landing is a real thing to do, so the token is stored anyway —
// with the confirmation withheld rather than faked.
func TestAuthLoginStoresAnUnconfirmedKeyWhenTheRegistryIsSilent(t *testing.T) {
	h, path := loginHarness(t)
	// A port nothing is listening on: the request fails before any status.
	h.env["KROWK_API_URL"] = "http://127.0.0.1:1/v1"

	code, stdout, stderr := h.run("auth", "login", "--token", "krowk_sk_offline")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s — an unreachable registry is not a bad key", code, stderr)
	}
	if !strings.Contains(stdout, "unconfirmed") {
		t.Errorf("output claims more than it knows:\n%s", stdout)
	}

	c := stored(t, path)
	if c["token"] != "krowk_sk_offline" {
		t.Errorf("token = %v, want it stored despite the silence", c["token"])
	}
	// Nothing was confirmed, so nothing is recorded. An identity here would be
	// invented.
	if _, named := c["key_id"]; named {
		t.Errorf("credentials name a key the registry never confirmed: %v", c)
	}
}

// A registry that answers with something other than a key is a misconfigured
// URL, not a bad key — same treatment as silence.
func TestAuthLoginStoresWhenTheAnswerIsNotARegistrys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"welcome":"to the website"}`)
	}))
	t.Cleanup(srv.Close)

	h, path := loginHarness(t)
	h.env["KROWK_API_URL"] = srv.URL + "/v1"

	code, stdout, stderr := h.run("auth", "login", "--token", "krowk_sk_maybe")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "unconfirmed") {
		t.Errorf("output claims more than it knows:\n%s", stdout)
	}
	if c := stored(t, path); c["token"] != "krowk_sk_maybe" {
		t.Errorf("token = %v, want it stored", c["token"])
	}
}

// Login without a key never reaches the registry — there is nothing to ask
// about.
func TestAuthLoginWithoutAKeySaysWhatToPaste(t *testing.T) {
	h, _ := loginHarness(t)

	if got := h.fails("auth", "login")["error"]; got != "missing_token" {
		t.Errorf("error = %v, want missing_token", got)
	}
}

// Doctor names the two things that decide where an upload lands: which of the
// two sources the token came from, and the workspace login recorded for it.
func TestDoctorNamesTheTokenSourceAndTheRecordedWorkspace(t *testing.T) {
	h, _ := loginHarness(t)

	if _, _, stderr := h.run("auth", "login", "--token", "krowk_sk_pasted"); stderr != "" {
		t.Fatalf("login failed: %s", stderr)
	}

	report := doctorReport(t, h)
	if src, _ := report["token_source"].(string); src != api.TokenSourceFile {
		t.Errorf("token_source = %v, want %q", report["token_source"], api.TokenSourceFile)
	}
	if ws, _ := report["workspace"].(string); !strings.HasPrefix(ws, "ws_") {
		t.Errorf("workspace = %v, want the one login recorded", report["workspace"])
	}
}

// KROWK_TOKEN outranks the file, so the recorded workspace belongs to a key
// that is not in play. Doctor has to withhold it: naming a workspace uploads
// are not going to would be a wrong answer given confidently.
func TestDoctorWithholdsTheRecordedWorkspaceWhenTheEnvironmentSuppliesTheToken(t *testing.T) {
	h, _ := loginHarness(t)

	if _, _, stderr := h.run("auth", "login", "--token", "krowk_sk_pasted"); stderr != "" {
		t.Fatalf("login failed: %s", stderr)
	}
	recorded, _ := doctorReport(t, h)["workspace"].(string)
	if recorded == "" {
		t.Fatal("nothing was recorded, so there is nothing to withhold")
	}

	h.env["KROWK_TOKEN"] = "krowk_sk_from_ci"

	report := doctorReport(t, h)
	if src, _ := report["token_source"].(string); src != api.TokenSourceEnv {
		t.Errorf("token_source = %v, want %q", report["token_source"], api.TokenSourceEnv)
	}
	ws, _ := report["workspace"].(string)
	if ws == recorded {
		t.Errorf("workspace = %v, which belongs to the key in the file, not the one in use", ws)
	}
	if !strings.Contains(ws, "unknown") {
		t.Errorf("workspace = %v, want it to say it does not know", ws)
	}
}

func doctorReport(t *testing.T, h *harness) map[string]any {
	t.Helper()
	code, stdout, stderr := h.run("doctor")
	if code != 0 {
		t.Fatalf("doctor exited %d, stderr: %s", code, stderr)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	return report
}
