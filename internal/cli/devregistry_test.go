package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
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
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
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
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
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
	for _, tc := range []struct{ bound, asked, want string }{
		{"[::]:8787", ":8787", "http://localhost:8787"},
		// ":0" means "any port", and the bound address says which one it was.
		{"[::]:41234", ":0", "http://localhost:41234"},
		// A hostname the user typed survives, even though the listener
		// reports the IP it resolved to.
		{"192.0.2.1:9000", "files.internal:9000", "http://files.internal:9000"},
		// "any port" with a named host still takes the port from the bind.
		{"127.0.0.1:41234", "localhost:0", "http://localhost:41234"},
		// A wildcard bind listens everywhere but dials nowhere, so the URL
		// says localhost instead; loopback IPs fold to the same name, so the
		// default bind stays the address --dev dials.
		{"0.0.0.0:8787", "0.0.0.0:8787", "http://localhost:8787"},
		{"[::]:8787", "[::]:8787", "http://localhost:8787"},
		{"127.0.0.1:8787", defaultRegistryAddr, "http://localhost:8787"},
		{"[::1]:8787", "[::1]:8787", "http://localhost:8787"},
	} {
		if got := localBase(tc.bound, tc.asked); got != tc.want {
			t.Errorf("localBase(%q, %q) = %q, want %q", tc.bound, tc.asked, got, tc.want)
		}
	}
}

// The registry takes uploads with no key and serves the bytes back to anyone
// who can reach it. Off-box by default would hand that to whoever shares the
// network.
func TestTheLocalRegistryBindsLoopbackByDefault(t *testing.T) {
	host := listenHost(defaultRegistryAddr)
	if !isLoopbackHost(host) {
		t.Errorf("default listen host = %q, want loopback", host)
	}
	// And still on the port --dev looks for, or the shorthand breaks.
	if !reachableByDev(defaultRegistryAddr) {
		t.Errorf("--dev cannot reach the default address %q", defaultRegistryAddr)
	}
}

func TestReachableByDev(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8787", true},
		{"localhost:8787", true},
		{":8787", true}, // every interface includes loopback
		{"0.0.0.0:8787", true},
		{"127.0.0.1:9000", false}, // right host, wrong port
		{"192.168.1.5:8787", false},
		// Loopback, but not where localhost lands — --dev cannot connect, so the
		// banner must advise KROWK_API_URL instead.
		{"127.0.0.2:8787", false},
	} {
		if got := reachableByDev(tc.addr); got != tc.want {
			t.Errorf("reachableByDev(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// An address without a port cannot bind, so it must not first be announced as a
// listening URL and warned about as if it were open to the network.
func TestAnAddressWithoutAPortIsRejected(t *testing.T) {
	// "127.0.0.1:" splits cleanly but binds a kernel-picked port the banner has
	// no name for; "127.0.0.1:http" binds port 80 while the banner prints the
	// name verbatim; ":08787" binds 8787 under a different spelling.
	// "127.0.0.1:99999" is the same failure by overflow: announced, never bound.
	for _, addr := range []string{
		"8787", "localhost", "127.0.0.1", "127.0.0.1:", ":",
		"127.0.0.1:http", ":-1",
		"127.0.0.1:99999", ":65536", "127.0.0.1:08787",
	} {
		if err := usableAddr(addr); err == nil {
			t.Errorf("usableAddr(%q) = nil, want an error naming the right shape", addr)
		}
	}
	// ":0" passes: the kernel picks the port, and since the bind precedes the
	// banner, the banner reports the port it picked rather than misreporting.
	for _, addr := range []string{
		":8787", "127.0.0.1:8787", "0.0.0.0:9000", ":65535", ":0", "127.0.0.1:0",
		defaultRegistryAddr,
	} {
		if err := usableAddr(addr); err != nil {
			t.Errorf("usableAddr(%q) = %v", addr, err)
		}
	}
}

// Bind-before-banner makes ":0" a feature rather than a misreport: the kernel
// picks a free port, the registry serves on it, and the banner names the port
// that was actually bound, with advice that connects.
func TestAddrPortZeroServesOnAKernelPickedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serveOn(io.Discard, ln, "127.0.0.1:0", flags{}) }()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	banner := registryBanner(ln.Addr().String(), "127.0.0.1:0")
	if !strings.Contains(banner, "krowk registry listening on http://localhost:"+port+"\n") {
		t.Errorf("banner = %q, want it to carry the bound port %s", banner, port)
	}
	if !strings.Contains(banner, "KROWK_API_URL=http://localhost:"+port+"/v1") {
		t.Errorf("banner = %q, want KROWK_API_URL advice naming the bound port", banner)
	}

	// And a registry really answers on the port the kernel picked.
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("nothing answered on the bound port: %v", err)
	}
	resp.Body.Close()

	ln.Close()
	var apiErr *api.Error
	if serveErr := <-done; !errors.As(serveErr, &apiErr) || apiErr.Code() != "registry_unavailable" {
		t.Errorf("serve after the listener closed = %v, want registry_unavailable", serveErr)
	}
}

func TestTheBannerWarnsOnlyWhenBoundOffBox(t *testing.T) {
	// The default: loopback on the dev port, so --dev is the advice and there is
	// nothing to warn about.
	def := registryBanner("127.0.0.1:8787", defaultRegistryAddr)
	if !strings.Contains(def, "krowk registry listening on http://localhost:8787\n") {
		t.Errorf("banner is missing the address:\n%s", def)
	}
	if !strings.Contains(def, "--dev") {
		t.Errorf("default banner should point at --dev:\n%s", def)
	}
	if strings.Contains(def, "reachable from the network") {
		t.Errorf("loopback should not warn:\n%s", def)
	}

	// Off-box, deliberately or by binding everything: say so, and say why.
	for _, addr := range []string{"0.0.0.0:8787", ":8787", "192.168.1.5:8787"} {
		got := registryBanner(addr, addr)
		if !strings.Contains(got, "reachable from the network") {
			t.Errorf("%s should warn:\n%s", addr, got)
		}
		if !strings.Contains(got, "needs no key") {
			t.Errorf("%s: the warning should say why it matters:\n%s", addr, got)
		}
	}

	// A non-default port cannot be reached by --dev, so the banner says what can.
	other := registryBanner("127.0.0.1:9000", "127.0.0.1:9000")
	if !strings.Contains(other, "KROWK_API_URL=http://localhost:9000/v1") {
		t.Errorf("banner should give the env var for a non-default port:\n%s", other)
	}
}

// The banner only appears after a successful bind, so a script keying off
// "listening" never proceeds against a port that failed to open.
func TestRegistryServeFailsToBindWithoutPrintingTheBanner(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	var out bytes.Buffer
	serveErr := registryServe(&out, flags{addr: taken.Addr().String()})

	var apiErr *api.Error
	if !errors.As(serveErr, &apiErr) || apiErr.Code() != "registry_unavailable" {
		t.Fatalf("serve on a taken port = %v, want registry_unavailable", serveErr)
	}
	if !strings.Contains(apiErr.Fix(), taken.Addr().String()) {
		t.Errorf("fix = %q, want it to name the address", apiErr.Fix())
	}
	if out.Len() != 0 {
		t.Errorf("banner printed despite the failed bind: %q", out.String())
	}
}

// An empty --addr means the default port; holding that port shows the fallback
// engaged without leaving a server running.
func TestRegistryServeFallsBackToTheDefaultAddr(t *testing.T) {
	if taken, err := net.Listen("tcp", defaultRegistryAddr); err == nil {
		defer taken.Close()
	} // if the listen failed, something else holds the port — same outcome

	var out bytes.Buffer
	serveErr := registryServe(&out, flags{addr: ""})

	var apiErr *api.Error
	if !errors.As(serveErr, &apiErr) || !strings.Contains(apiErr.Fix(), defaultRegistryAddr) {
		t.Fatalf("serve with an empty addr = %v, want a bind failure naming %s", serveErr, defaultRegistryAddr)
	}
}

// registry.Handler treats <= 0 as "use the default", so a negative limit has
// to be rejected up front or it silently means 100 MiB.
func TestNegativeLimitBytesIsRejected(t *testing.T) {
	h := newHarness(t, 0)

	code, _, stderr := h.run("registry", "serve", "--limit-bytes", "-5")
	if code == 0 {
		t.Fatal("a negative --limit-bytes should fail")
	}
	e := decode(t, stderr)
	if e.Error["error"] != "bad_flag" {
		t.Errorf("error = %v, want bad_flag", e.Error["error"])
	}
}

// The check lives in the serve path, so asking for help never trips over a
// flag value that only `registry serve` cares about.
func TestHelpWinsOverANegativeLimitBytes(t *testing.T) {
	h := newHarness(t, 0)

	code, stdout, stderr := h.run("--help", "--limit-bytes", "-5")
	if code != 0 || !strings.Contains(stdout, "uploads create") {
		t.Errorf("--help with a bad --limit-bytes = %q (exit %d), stderr: %s", stdout, code, stderr)
	}
}

// The one path the handler tests cannot see: the flags actually reaching
// registry.Handler. A limit small enough to refuse the fixture proves it.
func TestRegistryServeWiresLimitBytesIntoTheHandler(t *testing.T) {
	h := newHarness(t, 0)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serveOn(io.Discard, ln, ln.Addr().String(), flags{limitBytes: 4}) }()

	h.env["KROWK_API_URL"] = "http://" + ln.Addr().String() + "/v1"
	code, _, stderr := h.run("push", h.fixture)
	if code == 0 {
		t.Fatal("a push above --limit-bytes should fail")
	}
	e := decode(t, stderr)
	if e.Error["error"] != "invalid" {
		t.Errorf("error = %v, want invalid", e.Error["error"])
	}
	if msg, _ := e.Error["message"].(string); !strings.Contains(msg, "4 bytes") {
		t.Errorf("message = %q, want the limit in it", msg)
	}

	ln.Close()
	var apiErr *api.Error
	if serveErr := <-done; !errors.As(serveErr, &apiErr) || apiErr.Code() != "registry_unavailable" {
		t.Errorf("serve after the listener closed = %v, want registry_unavailable", serveErr)
	}
}

// --site rebrands the public links only, so a push still lands on the local
// listener while the returned URLs carry the named origin.
func TestRegistryServeWiresSiteIntoTheHandler(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- serveOn(io.Discard, ln, ln.Addr().String(), flags{site: "https://files.example"})
	}()

	// The declare call answers with the links; the --site origin has to be baked
	// into them. Asked over the wire directly, because this stand-in signs its
	// upload URL for the same origin — a push through the client would then try
	// to send the bytes to files.example, which is the point of the flag when
	// demoing and useless inside a test.
	res, err := http.Post("http://"+ln.Addr().String()+"/v1/artifacts", "application/json",
		strings.NewReader(`{"artifact":{"filename":"shot.png","content_type":"image/png","byte_size":5}}`))
	if err != nil {
		t.Fatal(err)
	}
	var declared struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(res.Body).Decode(&declared); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if !strings.HasPrefix(declared.URL, "https://files.example/") {
		t.Errorf("artifact url = %q, want the --site origin baked in", declared.URL)
	}

	ln.Close()
	var apiErr *api.Error
	if serveErr := <-done; !errors.As(serveErr, &apiErr) || apiErr.Code() != "registry_unavailable" {
		t.Errorf("serve after the listener closed = %v, want registry_unavailable", serveErr)
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
