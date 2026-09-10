package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// Doctor carries the store health check: pass on a fresh home, with the
// harness StatusCheck shape (name/status/message, hint only on failure).
func TestDoctorIncludesAStoreCheck(t *testing.T) {
	h := newHarness(t, 0)
	h.env["HOME"] = t.TempDir()
	h.env["XDG_DATA_HOME"] = ""

	report := doctorReport(t, h)
	raw, ok := report["store"]
	if !ok {
		t.Fatal("doctor has no store key")
	}
	b, _ := json.Marshal(raw)
	var check struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Message string `json:"message"`
		Hint    string `json:"hint"`
	}
	if err := json.Unmarshal(b, &check); err != nil {
		t.Fatalf("store check is not a StatusCheck: %v\n%s", err, b)
	}
	if check.Name != "store" || check.Status != "pass" {
		t.Errorf("store check = %+v, want name store status pass", check)
	}
	if !strings.Contains(check.Message, "krowk.db") {
		t.Errorf("store message = %q, want the store path", check.Message)
	}
	if check.Hint != "" {
		t.Errorf("passing store check carries hint %q", check.Hint)
	}
}

// A bad HOME fails the check with a hint naming internal/store, never a
// reinstall — and doctor itself still exits 0, because its job is to
// describe a broken setup, not to be stopped by one.
func TestDoctorStoreCheckFailsClosedOnABadHome(t *testing.T) {
	h := newHarness(t, 0)
	h.env["HOME"] = ""
	h.env["XDG_DATA_HOME"] = "relative"

	code, stdout, stderr := h.run("doctor")
	if code != 0 {
		t.Fatalf("doctor exited %d, stderr: %s", code, stderr)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	b, _ := json.Marshal(report["store"])
	var check struct {
		Status string `json:"status"`
		Hint   string `json:"hint"`
	}
	if err := json.Unmarshal(b, &check); err != nil {
		t.Fatalf("store check is not a StatusCheck: %v\n%s", err, b)
	}
	if check.Status != "fail" {
		t.Errorf("store check = %s, want fail on a bad HOME", b)
	}
	var hint string
	hint = check.Hint
	if !strings.Contains(hint, "internal/store") {
		t.Errorf("store hint = %q, want it to name internal/store", hint)
	}
	if strings.Contains(strings.ToLower(string(b)), "reinstall") {
		t.Errorf("store check = %s, must not say reinstall", b)
	}
}
