package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// errorAs is errors.As, named so the assertions below read as assertions.
func errorAs(err error, target any) bool { return errors.As(err, target) }

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDescribeIsStableAndSensitive(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "before.png", "one")
	b := write(t, dir, "after.png", "two")

	manifest, key, err := Describe([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}

	// Same inputs, same key — that is what makes a rerun free.
	if _, again, _ := Describe([]string{a, b}); again != key {
		t.Errorf("key is not stable: %q then %q", key, again)
	}
	// Order is part of the identity, because it decides which bytes land under
	// which name.
	if _, swapped, _ := Describe([]string{b, a}); swapped == key {
		t.Error("swapping the files produced the same key")
	}

	if len(manifest) != 2 {
		t.Fatalf("manifest has %d entries, want 2", len(manifest))
	}
	sum := sha256.Sum256([]byte("one"))
	if manifest[0].Digest != hex.EncodeToString(sum[:]) {
		t.Errorf("digest = %q, want the sha256 of the contents", manifest[0].Digest)
	}
	if manifest[0].Bytes != 3 || manifest[0].Filename != "before.png" {
		t.Errorf("manifest[0] = %+v", manifest[0])
	}
	if manifest[0].ContentType != "image/png" {
		t.Errorf("content type = %q, want image/png", manifest[0].ContentType)
	}
}

func TestDescribeRejectsWhatItCannotRead(t *testing.T) {
	if _, _, err := Describe(nil); err == nil {
		t.Error("no files should be an error")
	}
	_, _, err := Describe([]string{filepath.Join(t.TempDir(), "absent.png")})
	if err == nil || err.(*Error).Code() != "file_unreadable" {
		t.Errorf("err = %v, want file_unreadable", err)
	}
	if _, _, err := Describe([]string{t.TempDir()}); err == nil {
		t.Error("a directory is not a regular file")
	}
}

// step records one leg of the handshake.
type step struct {
	name string
	auth string
	key  string
}

// stubRegistry is the smallest thing that speaks the handshake, so the client's
// side of it can be asserted without the full mock.
func stubRegistry(t *testing.T, steps *[]step, mu *sync.Mutex, finalizeHost func() string) *httptest.Server {
	t.Helper()

	var srv *httptest.Server
	mux := http.NewServeMux()

	record := func(name string, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		*steps = append(*steps, step{
			name: name,
			auth: r.Header.Get("Authorization"),
			key:  r.Header.Get("Idempotency-Key"),
		})
	}

	mux.HandleFunc("POST /v1/artifacts", func(w http.ResponseWriter, r *http.Request) {
		record("begin", r)
		var body beginRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("begin body: %v", err)
		}
		if len(body.Files) != 1 || body.Files[0].Digest == "" {
			t.Errorf("manifest = %+v, want one file with a digest", body.Files)
		}
		targets := []UploadTarget{{
			Filename: body.Files[0].Filename,
			Method:   http.MethodPut,
			URL:      srv.URL + "/blobs/tok",
			Headers:  map[string]string{"Content-Type": body.Files[0].ContentType},
		}}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(beginResponse{
			ID:          "abc1234",
			Uploads:     targets,
			FinalizeURL: finalizeHost() + "/v1/artifacts/abc1234/finalize",
		})
	})

	mux.HandleFunc("PUT /blobs/tok", func(w http.ResponseWriter, r *http.Request) {
		record("put", r)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /v1/artifacts/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		record("finalize", r)
		w.Header().Set("X-RateLimit-Remaining", "97")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Artifact{
			ID:    r.PathValue("id"),
			URL:   "https://krowk.com/a/" + r.PathValue("id"),
			Bytes: 3,
		})
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestHandshakeRunsThreeStepsAndKeepsTheTokenOffStorage(t *testing.T) {
	var mu sync.Mutex
	var steps []step

	srv := stubRegistry(t, &steps, &mu, func() string { return "" }) // relative finalize_url
	client := New(srv.URL+"/v1", "krk_secret")
	client.Sleep = func(time.Duration) {}

	file := write(t, t.TempDir(), "shot.png", "one")
	artifact, err := client.CreateArtifact(context.Background(), []string{file}, map[string]string{"repo": "acme/x"})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if artifact.URL != "https://krowk.com/a/abc1234" {
		t.Errorf("URL = %q", artifact.URL)
	}
	if artifact.RateLimitRemaining != "97" {
		t.Errorf("rate limit = %q, want it carried off the finalize response", artifact.RateLimitRemaining)
	}

	if len(steps) != 3 || steps[0].name != "begin" || steps[1].name != "put" || steps[2].name != "finalize" {
		t.Fatalf("steps = %+v, want begin -> put -> finalize", steps)
	}
	// The presigned URL carries its own authorisation. Sending the API token to
	// whatever host it names would hand the key to a third party.
	if steps[1].auth != "" {
		t.Errorf("the blob PUT carried Authorization %q", steps[1].auth)
	}
	if steps[0].auth != "Bearer krk_secret" || steps[2].auth != "Bearer krk_secret" {
		t.Errorf("the API steps should be authenticated: %+v", steps)
	}
	// One key for the whole handshake, so either end can dedupe on it.
	if steps[0].key == "" || steps[0].key != steps[2].key {
		t.Errorf("idempotency keys = %q and %q", steps[0].key, steps[2].key)
	}
}

func TestFinalizeOnAnotherHostIsRefused(t *testing.T) {
	var elsewhere int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere++
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()

	var mu sync.Mutex
	var steps []step
	srv := stubRegistry(t, &steps, &mu, func() string { return other.URL })

	client := New(srv.URL+"/v1", "krk_secret")
	client.Sleep = func(time.Duration) {}

	file := write(t, t.TempDir(), "shot.png", "one")
	_, err := client.CreateArtifact(context.Background(), []string{file}, nil)
	if err == nil {
		t.Fatal("a finalize_url on a foreign host should be refused")
	}
	if code := err.(*Error).Code(); code != "untrusted_endpoint" {
		t.Errorf("error = %q, want untrusted_endpoint", code)
	}
	if elsewhere != 0 {
		t.Errorf("the client sent %d request(s) to the foreign host", elsewhere)
	}
}

// A presigned URL legitimately names a foreign host, but a compromised registry
// must not be able to aim the client at the machine it runs on or the network
// around it — a CI runner can reach plenty the registry cannot.
func TestUploadTargetsInsideTheNetworkAreRefused(t *testing.T) {
	production := New("https://api.krowk.com/v1", "krk_secret")

	for _, raw := range []string{
		"https://127.0.0.1/blob",
		"https://localhost/blob",
		"https://[::1]/blob",
		"https://10.0.0.5/blob",
		"https://192.168.1.10/blob",
		"https://172.16.4.4/blob",
		"https://169.254.169.254/latest/meta-data/",
		"https://0.0.0.0/blob",
		// Carrier-grade NAT: not private by Go's reckoning, but where Tailscale
		// and several CI providers keep their internal hosts.
		"https://100.64.1.2/blob",
		"https://198.18.0.1/blob",
		"https://192.0.0.1/blob",
		"https://240.0.0.1/blob",
		"https://255.255.255.255/blob",
		"https://239.1.2.3/blob",
		// NAT64: on an IPv6-only network this is the metadata service, and it is
		// none of loopback, private or link-local as far as Go is concerned.
		"https://[64:ff9b::a9fe:a9fe]/latest/meta-data/",
		"https://[64:ff9b::7f00:1]/blob",
		// IPv4-mapped, the other spelling of the same reach.
		"https://[::ffff:169.254.169.254]/blob",
	} {
		if _, err := production.storageOrigin(raw); err == nil {
			t.Errorf("%s was accepted", raw)
		} else if code := err.(*Error).Code(); code != "untrusted_endpoint" {
			t.Errorf("%s: error = %q, want untrusted_endpoint", raw, code)
		}
	}

	// The artifact is the payload, so it does not travel in the clear.
	if _, err := production.storageOrigin("http://storage.example.com/blob"); err == nil {
		t.Error("a plaintext upload URL was accepted")
	} else if code := err.(*Error).Code(); code != "insecure_upload_url" {
		t.Errorf("error = %q, want insecure_upload_url", code)
	}

	// A real storage host still works.
	if _, err := production.storageOrigin("https://storage.example.com/blob?sig=abc"); err != nil {
		t.Errorf("a legitimate storage URL was refused: %v", err)
	}
}

// Against a local registry, local targets are the expected case, not an attack.
func TestLocalRegistryMayUseLocalUploadTargets(t *testing.T) {
	local := New(DevBaseURL, "")

	for _, raw := range []string{
		"http://localhost:8787/v1/blobs/tok",
		"http://127.0.0.1:8787/v1/blobs/tok",
	} {
		if _, err := local.storageOrigin(raw); err != nil {
			t.Errorf("%s was refused against a local registry: %v", raw, err)
		}
	}
}

// The contract documents one method. Honouring target.Method would let a
// response body choose what request this process makes.
func TestUploadAlwaysUsesPUT(t *testing.T) {
	var methods []string
	var srv *httptest.Server

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/artifacts", func(w http.ResponseWriter, r *http.Request) {
		var body beginRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(beginResponse{
			ID: "abc1234",
			Uploads: []UploadTarget{{
				Filename: body.Files[0].Filename,
				// A registry asking for something else entirely.
				Method: http.MethodDelete,
				URL:    srv.URL + "/blobs/tok",
			}},
			FinalizeURL: "/v1/artifacts/abc1234/finalize",
		})
	})
	mux.HandleFunc("/blobs/tok", func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/artifacts/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Artifact{ID: "abc1234", URL: "https://krowk.com/a/abc1234"})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	client := New(srv.URL+"/v1", "krk_secret")
	client.Sleep = func(time.Duration) {}

	file := write(t, t.TempDir(), "shot.png", "one")
	if _, err := client.CreateArtifact(context.Background(), []string{file}, nil); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if len(methods) != 1 || methods[0] != http.MethodPut {
		t.Errorf("methods = %v, want a single PUT regardless of what the registry asked for", methods)
	}
}

// storageOrigin can only vet the URL the registry returned, not where that URL
// leads. Without this the guard above costs one redirect to get past: the host
// changes, and a 302 arrives at the next one as a GET, so the method restriction
// goes with it.
func TestAnUploadTargetMayNotRedirect(t *testing.T) {
	var internalHits int
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHits++
	}))
	defer internal.Close()

	// A storage host distinct from the registry's own, as a presigned URL is.
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/latest/meta-data/iam/", http.StatusFound)
	}))
	defer storage.Close()

	registry := httptest.NewServer(handshake(storage.URL + "/blobs/tok"))
	defer registry.Close()

	client := New(registry.URL+"/v1", "krk_secret")
	client.Sleep = func(time.Duration) {}

	file := write(t, t.TempDir(), "shot.png", "one")
	_, err := client.CreateArtifact(context.Background(), []string{file}, nil)
	if err == nil {
		t.Fatal("a redirect away from the upload target was followed, and the upload reported success")
	}
	var apiErr *Error
	if !errorAs(err, &apiErr) || apiErr.Code() != "upload_redirected" {
		t.Errorf("error = %v, want upload_redirected", err)
	}
	if internalHits != 0 {
		t.Errorf("the redirect target was contacted %d time(s)", internalHits)
	}
}

// The API's own host is allowed to redirect — http -> https in front of a
// self-hosted registry is ordinary — and permitDial still covers where it lands.
func TestTheAPIHostMayRedirect(t *testing.T) {
	client := New("https://api.krowk.com/v1", "krk_secret")

	from, _ := http.NewRequest(http.MethodPost, "https://api.krowk.com/v1/artifacts", nil)
	to, _ := http.NewRequest(http.MethodPost, "https://api.krowk.com/v1/artifacts/", nil)
	if err := client.checkRedirect(to, []*http.Request{from}); err != nil {
		t.Errorf("the API host was refused a redirect: %v", err)
	}
}

// The dialer hook is the half that survives DNS: internalHost resolves a name to
// judge it, the transport resolves again to dial it, and the registry owns the
// name in the case that matters. This asserts the hook is actually wired into the
// client's transport, not merely correct in isolation.
func TestTheDialerRefusesInternalAddresses(t *testing.T) {
	noProxy(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a request reached a loopback address")
	}))
	defer srv.Close()

	// Production base URL, so the local exemption does not apply.
	client := New("https://api.krowk.com/v1", "krk_secret")

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.HTTP.Do(req); err == nil {
		t.Fatal("the client connected to a loopback address")
	} else if !strings.Contains(err.Error(), "untrusted_endpoint") {
		t.Errorf("error = %v, want it to name untrusted_endpoint", err)
	}

	// A private address is refused the same way a loopback one is.
	if err := client.permitDial("tcp", "10.0.0.7:3128", nil); err == nil {
		t.Error("a private address was permitted with no proxy configured")
	}

	// A local registry means local addresses throughout, so the hook stands down.
	local := New(DevBaseURL, "")
	if err := local.permitDial("tcp", "127.0.0.1:8787", nil); err != nil {
		t.Errorf("a local registry was refused its own address: %v", err)
	}
}

// A zone makes an address unparseable, and permitDial judges what it can parse —
// so without stripping it, "fe80::1%eth0" is waved through at the dial.
func TestTheDialerStripsAnIPv6Zone(t *testing.T) {
	noProxy(t)
	client := New("https://api.krowk.com/v1", "krk_secret")

	for _, address := range []string{"[fe80::1%eth0]:443", "[fe80::1]:443", "[64:ff9b::a9fe:a9fe]:443"} {
		if err := client.permitDial("tcp6", address, nil); err == nil {
			t.Errorf("%s was permitted", address)
		}
	}
}

// Behind a proxy every connection goes to the proxy, and corporate proxies sit on
// private addresses — so judging the dialled address there refuses every upload
// from the environments this tool exists for.
func TestUploadsWorkBehindAProxyOnAPrivateAddress(t *testing.T) {
	var proxied int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied++
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	t.Setenv("HTTP_PROXY", proxy.URL)
	client := New("https://api.krowk.com/v1", "krk_secret")
	if !client.isProxy(strings.TrimPrefix(proxy.URL, "http://")) {
		t.Fatalf("the client did not recognise %s as the proxy", proxy.URL)
	}
	// The proxy is on loopback here, which is the shape of a proxy on 10.0.0.0/8.
	// The request is plain http so it is forwarded rather than tunnelled with
	// CONNECT, which a bare test server cannot answer.
	client.HTTP.Transport.(*http.Transport).Proxy = func(*http.Request) (*url.URL, error) {
		return url.Parse(proxy.URL)
	}

	req, err := http.NewRequest(http.MethodGet, "http://storage.example.com/blob", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.HTTP.Do(req); err != nil {
		t.Fatalf("an upload behind a proxy was refused: %v", err)
	}
	if proxied != 1 {
		t.Errorf("the proxy saw %d requests, want 1", proxied)
	}

	// Only the proxy's own address earns the exemption. A request that skipped the
	// proxy — NO_PROXY covers its host — is judged like any other, so configuring
	// a proxy must not switch the boundary off wholesale.
	for _, address := range []string{"169.254.169.254:80", "10.0.0.99:443", "127.0.0.1:9999"} {
		if err := client.permitDial("tcp", address, nil); err == nil {
			t.Errorf("%s was permitted because a proxy is configured elsewhere", address)
		}
	}
}

func TestProxyAddressesReadsTheSpellingsPeopleUse(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"http://10.0.0.7:3128", "10.0.0.7:3128"},
		{"10.0.0.7:3128", "10.0.0.7:3128"}, // no scheme, which url.Parse reads as a path
		{"http://10.0.0.7", "10.0.0.7:80"},
		{"https://10.0.0.7", "10.0.0.7:443"},
		{"socks5://10.0.0.7", "10.0.0.7:1080"},
	} {
		if got := proxyAddresses(tc.raw); !slices.Contains(got, tc.want) {
			t.Errorf("proxyAddresses(%q) = %v, want it to include %q", tc.raw, got, tc.want)
		}
	}
}

// noProxy clears the proxy environment, so the tests that assert the dial hook is
// on duty do not depend on whoever is running them.
func noProxy(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy",
	} {
		t.Setenv(k, "")
	}
}

// Go decides whether to forward Authorization on a redirect by host alone, so
// https -> http on one host would carry the token in the clear.
func TestARedirectMayNotDowngradeTheScheme(t *testing.T) {
	client := New("https://api.krowk.com/v1", "krk_secret")

	from, _ := http.NewRequest(http.MethodPost, "https://api.krowk.com/v1/artifacts", nil)
	to, _ := http.NewRequest(http.MethodPost, "http://api.krowk.com/v1/artifacts", nil)

	err := client.checkRedirect(to, []*http.Request{from})
	if err == nil {
		t.Fatal("a downgrade to http was followed with the token attached")
	}
	var apiErr *Error
	if !errorAs(err, &apiErr) || apiErr.Code() != "insecure_redirect" {
		t.Errorf("error = %v, want insecure_redirect", err)
	}
}

// handshake is a registry that walks the three steps, aiming the upload leg at
// uploadURL. Only the redirect tests need to choose that host, so it lives here.
func handshake(uploadURL string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/artifacts", func(w http.ResponseWriter, r *http.Request) {
		var body beginRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(beginResponse{
			ID:          "abc1234",
			Uploads:     []UploadTarget{{Filename: body.Files[0].Filename, Method: http.MethodPut, URL: uploadURL}},
			FinalizeURL: "/v1/artifacts/abc1234/finalize",
		})
	})
	mux.HandleFunc("POST /v1/artifacts/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Artifact{ID: "abc1234", URL: "https://krowk.com/a/abc1234"})
	})
	return mux
}

func TestUploadURLMustBeHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(beginResponse{
			ID:          "abc1234",
			Uploads:     []UploadTarget{{URL: "file:///etc/passwd"}},
			FinalizeURL: "/v1/artifacts/abc1234/finalize",
		})
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "")
	client.Sleep = func(time.Duration) {}

	file := write(t, t.TempDir(), "shot.png", "one")
	_, err := client.CreateArtifact(context.Background(), []string{file}, nil)
	if err == nil || err.(*Error).Code() != "malformed_response" {
		t.Errorf("err = %v, want malformed_response for a file:// upload target", err)
	}
}

func TestAlreadyCompleteSkipsTheUpload(t *testing.T) {
	var puts int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/artifacts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(beginResponse{
			ID:       "abc1234",
			Complete: true,
			Artifact: &Artifact{ID: "abc1234", URL: "https://krowk.com/a/abc1234"},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { puts++ })

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := New(srv.URL+"/v1", "")
	file := write(t, t.TempDir(), "shot.png", "one")

	artifact, err := client.CreateArtifact(context.Background(), []string{file}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.URL != "https://krowk.com/a/abc1234" {
		t.Errorf("URL = %q", artifact.URL)
	}
	if puts != 0 {
		t.Errorf("a finished upload should not send bytes again, got %d request(s)", puts)
	}
}

func TestRetryAfterAcceptsBothSpellings(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"5", 5 * time.Second, true},
		{" 7 ", 7 * time.Second, true},
		// The spec allows an absolute date, which Atoi alone would drop.
		{now.Add(30 * time.Second).UTC().Format(http.TimeFormat), 30 * time.Second, true},
		// A registry must not be able to wedge the CLI for a week.
		{"604800", 60 * time.Second, true},
		{now.Add(72 * time.Hour).UTC().Format(http.TimeFormat), 60 * time.Second, true},
		// Already past, or nonsense: fall back to exponential backoff.
		{now.Add(-time.Hour).UTC().Format(http.TimeFormat), 0, false},
		{"0", 0, false},
		{"-3", 0, false},
		{"soon", 0, false},
		{"", 0, false},
	} {
		got, ok := retryAfter(tc.in, now)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("retryAfter(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestVerifyKeyReportsScopes(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("X-RateLimit-Remaining", "42")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Key{
			Valid:     true,
			KeyID:     "key_7f3a",
			Workspace: "acme",
			Scopes:    []string{"artifacts:read", ScopeWrite},
		})
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "krk_secret")
	key, err := client.VerifyKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer krk_secret" {
		t.Errorf("Authorization = %q", auth)
	}
	if key.KeyID != "key_7f3a" || key.Workspace != "acme" {
		t.Errorf("key = %+v", key)
	}
	if !key.HasScope(ScopeWrite) || key.HasScope("artifacts:delete") {
		t.Errorf("scopes = %v", key.Scopes)
	}
	if key.RateLimitRemaining != "42" {
		t.Errorf("remaining = %q, want it off the header", key.RateLimitRemaining)
	}
}

// A 200 is not the same as a yes.
func TestVerifyKeyTreatsValidFalseAsRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Key{Valid: false})
	}))
	defer srv.Close()

	_, err := New(srv.URL+"/v1", "krk_secret").VerifyKey(context.Background())
	if err == nil || err.(*Error).Code() != "invalid_key" {
		t.Errorf("err = %v, want invalid_key", err)
	}
}

func TestVerifyKeyPassesTheRegistrysRejectionThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"key_revoked","fix":"issue a new key","retryable":false}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL+"/v1", "krk_secret").VerifyKey(context.Background())
	var apiErr *Error
	if !errorAs(err, &apiErr) || apiErr.Code() != "key_revoked" || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want the registry's own key_revoked", err)
	}
	if apiErr.Fix() != "issue a new key" {
		t.Errorf("fix = %q", apiErr.Fix())
	}
}

func TestBaseURLPrecedence(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	for _, tc := range []struct {
		name string
		dev  bool
		env  map[string]string
		want string
	}{
		{"nothing set", false, nil, DefaultBaseURL},
		{"--dev", true, nil, DevBaseURL},
		{"KROWK_DEV", false, map[string]string{"KROWK_DEV": "1"}, DevBaseURL},
		{"KROWK_DEV off", false, map[string]string{"KROWK_DEV": "0"}, DefaultBaseURL},
		{"KROWK_API_URL", false, map[string]string{"KROWK_API_URL": "https://staging/v1"}, "https://staging/v1"},
		// A flag typed on the command line beats an ambient variable.
		{"--dev over KROWK_API_URL", true, map[string]string{"KROWK_API_URL": "https://staging/v1"}, DevBaseURL},
		// A named target beats a general "use dev".
		{"KROWK_API_URL over KROWK_DEV", false,
			map[string]string{"KROWK_API_URL": "https://staging/v1", "KROWK_DEV": "1"}, "https://staging/v1"},
	} {
		if got := BaseURLFor(tc.dev, env(tc.env)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTruthy(t *testing.T) {
	for _, yes := range []string{"1", "true", "TRUE", "yes", " on "} {
		if !Truthy(yes) {
			t.Errorf("Truthy(%q) = false", yes)
		}
	}
	for _, no := range []string{"", "0", "false", "off", "no", "maybe"} {
		if Truthy(no) {
			t.Errorf("Truthy(%q) = true", no)
		}
	}
}

func TestRetryableStepIsRetriedThenSucceeds(t *testing.T) {
	var attempts int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/artifacts", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"rate_limited","retryable":true}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(beginResponse{
			ID:       "abc1234",
			Complete: true,
			Artifact: &Artifact{ID: "abc1234", URL: "https://krowk.com/a/abc1234"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var slept int
	client := New(srv.URL+"/v1", "")
	client.Sleep = func(time.Duration) { slept++ }

	file := write(t, t.TempDir(), "shot.png", "one")
	if _, err := client.CreateArtifact(context.Background(), []string{file}, nil); err != nil {
		t.Fatalf("a 429 with retryable:true should be retried: %v", err)
	}
	if attempts != 2 || slept != 1 {
		t.Errorf("attempts = %d, sleeps = %d, want 2 and 1", attempts, slept)
	}
}
