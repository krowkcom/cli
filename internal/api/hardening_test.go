package api

import (
	"context"
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

func write(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
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

// A presigned URL legitimately names a foreign host, but a compromised registry
// must not be able to aim the client at the machine it runs on or the network
// around it — a CI runner can reach plenty the registry cannot.
func TestUploadTargetsInsideTheNetworkAreRefused(t *testing.T) {
	production := New("https://api.krowk.com/v1", "krowk_sk_secret")

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

	// Neither does it get read off the local disk.
	if _, err := production.storageOrigin("file:///etc/passwd"); err == nil {
		t.Error("a file:// upload URL was accepted")
	} else if code := err.(*Error).Code(); code != "bad_upload_url" {
		t.Errorf("error = %q, want bad_upload_url", code)
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
		"http://localhost:8787/_storage/ws/art/shot.png",
		"http://127.0.0.1:8787/_storage/ws/art/shot.png",
	} {
		if _, err := local.storageOrigin(raw); err != nil {
			t.Errorf("%s was refused against a local registry: %v", raw, err)
		}
	}
}

// The contract documents one method. Honouring upload.method would let a
// response body choose what request this process makes. And the token stays
// off this leg entirely: a presigned URL carries its own authorisation and may
// point at any host the registry names, so Authorization must never ride along.
func TestUploadAlwaysUsesPUTAndKeepsTheTokenOffStorage(t *testing.T) {
	var mu sync.Mutex
	var methods, auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// testClient holds a token, which is the point: it must not be forwarded.
	c, _ := testClient(srv)
	path := write(t, t.TempDir(), "shot.png", "bytes")

	// A registry asking for something else entirely.
	up := &Upload{Method: http.MethodDelete, URL: srv.URL + "/_storage/ws/art/shot.png"}
	if err := c.PutBytes(context.Background(), up, Spec{Path: path, ByteSize: 5}); err != nil {
		t.Fatalf("PutBytes = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 1 || methods[0] != http.MethodPut {
		t.Errorf("methods = %v, want a single PUT regardless of what the registry asked for", methods)
	}
	for _, auth := range auths {
		if auth != "" {
			t.Errorf("Authorization = %q reached the storage leg, want it kept off", auth)
		}
	}
}

// storageOrigin can only vet the URL the registry returned, not where that URL
// leads. Without this the guard above costs one redirect to get past: the host
// changes, and a 302 arrives at the next one as a GET, so the method restriction
// goes with it.
func TestAnUploadTargetMayNotRedirect(t *testing.T) {
	var mu sync.Mutex
	var internalHits int
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		internalHits++
		mu.Unlock()
	}))
	t.Cleanup(internal.Close)

	// A storage host distinct from the registry's own, as a presigned URL is.
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/latest/meta-data/iam/", http.StatusFound)
	}))
	t.Cleanup(storage.Close)

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the registry itself should not be called by PutBytes")
	}))
	t.Cleanup(registry.Close)

	c, _ := testClient(registry)
	path := write(t, t.TempDir(), "shot.png", "bytes")

	err := c.PutBytes(context.Background(), &Upload{URL: storage.URL + "/blobs/tok"},
		Spec{Path: path, ByteSize: 5})
	if err == nil {
		t.Fatal("a redirect away from the upload target was followed, and the upload reported success")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "upload_redirected" {
		t.Errorf("error = %v, want upload_redirected", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if internalHits != 0 {
		t.Errorf("the redirect target was contacted %d time(s)", internalHits)
	}
}

// An upload URL on the API's own origin is the shape of a registry that serves
// its own blobs — this stack's does — and the same-origin allowance in
// checkRedirect means redirects on that leg are followed, not refused. Pinned
// here because it is an exception to "an upload target may not redirect": the
// downgrade check and the dial hook still apply, and the hop stays on the origin
// the user configured.
func TestAnUploadTargetOnTheAPIHostMayRedirect(t *testing.T) {
	var mu sync.Mutex
	var landed int
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /blobs/tok", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/blobs/moved", http.StatusFound)
	})
	mux.HandleFunc("/blobs/moved", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		landed++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, _ := testClient(srv)
	path := write(t, t.TempDir(), "shot.png", "bytes")

	if err := c.PutBytes(context.Background(), &Upload{URL: srv.URL + "/blobs/tok"},
		Spec{Path: path, ByteSize: 5}); err != nil {
		t.Fatalf("a same-origin redirect on the upload leg was refused: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if landed != 1 {
		t.Errorf("the redirect target on the API's own origin was hit %d time(s), want 1", landed)
	}
}

// The API's own host is allowed to redirect — http -> https in front of a
// self-hosted registry is ordinary — and permitDial still covers where it lands.
func TestTheAPIHostMayRedirect(t *testing.T) {
	client := New("https://api.krowk.com/v1", "krowk_sk_secret")

	from, _ := http.NewRequest(http.MethodPost, "https://api.krowk.com/v1/artifacts", nil)
	to, _ := http.NewRequest(http.MethodPost, "https://api.krowk.com/v1/artifacts/", nil)
	if err := client.checkRedirect(to, []*http.Request{from}); err != nil {
		t.Errorf("the API host was refused a redirect: %v", err)
	}
}

// The same-origin allowance is the origin, not a first hop that launders the
// rest: a request that started on the API's own host must stay there, or an
// upload URL on that origin could aim a GET anywhere with one redirect.
func TestASameOriginRedirectMayNotLeaveTheHost(t *testing.T) {
	client := New("https://api.krowk.com/v1", "krowk_sk_secret")

	from, _ := http.NewRequest(http.MethodPost, "https://api.krowk.com/v1/artifacts", nil)
	to, _ := http.NewRequest(http.MethodPost, "https://internal.example/latest/meta-data/", nil)

	err := client.checkRedirect(to, []*http.Request{from})
	if err == nil {
		t.Fatal("a redirect off the API's own host was followed")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "untrusted_redirect" {
		t.Errorf("error = %v, want untrusted_redirect", err)
	}
}

// Go decides whether to forward Authorization on a redirect by host alone, so
// https -> http on one host would carry the token in the clear.
func TestARedirectMayNotDowngradeTheScheme(t *testing.T) {
	client := New("https://api.krowk.com/v1", "krowk_sk_secret")

	from, _ := http.NewRequest(http.MethodPost, "https://api.krowk.com/v1/artifacts", nil)
	to, _ := http.NewRequest(http.MethodPost, "http://api.krowk.com/v1/artifacts", nil)

	err := client.checkRedirect(to, []*http.Request{from})
	if err == nil {
		t.Fatal("a downgrade to http was followed with the token attached")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "insecure_redirect" {
		t.Errorf("error = %v, want insecure_redirect", err)
	}
}

// KROWK_API_URL is documented as an arbitrary base URL, and a self-hosted
// registry on a private network is the ordinary reason to set one. The host the
// user typed is trusted the way isLocal trusts loopback: its own address may be
// dialled, and upload targets on its own origin are accepted — this stack's
// registry serves blobs on its own host. Everything else stays refused.
func TestASelfHostedRegistryOnAPrivateNetworkIsTrusted(t *testing.T) {
	noProxy(t)
	client := New("https://10.1.2.3/v1", "krowk_sk_secret")

	if err := client.permitDial(context.Background(), "10.1.2.3:443"); err != nil {
		t.Errorf("the API host's own address was refused: %v", err)
	}
	// The trust is the host the user typed, not the network around it.
	for _, address := range []string{"10.1.2.4:443", "10.1.2.3:8080", "169.254.169.254:80"} {
		if err := client.permitDial(context.Background(), address); err == nil {
			t.Errorf("%s was permitted because the API lives on a private network", address)
		}
	}

	if _, err := client.storageOrigin("https://10.1.2.3/blobs/tok"); err != nil {
		t.Errorf("an upload target on the API's own origin was refused: %v", err)
	}
	// An explicit default port and an implicit one spell the same origin, in
	// both directions — the dial layer already treats them as one address.
	if _, err := client.storageOrigin("https://10.1.2.3:443/blobs/tok"); err != nil {
		t.Errorf("the API's origin with its default port spelled out was refused: %v", err)
	}
	if _, err := New("https://10.1.2.3:443/v1", "krowk_sk_secret").
		storageOrigin("https://10.1.2.3/blobs/tok"); err != nil {
		t.Errorf("the API's origin with the default port left implicit was refused: %v", err)
	}
	// A different private host, or the same host on plaintext http, earns nothing.
	if _, err := client.storageOrigin("https://10.9.9.9/blobs/tok"); err == nil {
		t.Error("an upload target on another private host was accepted")
	}
	if _, err := client.storageOrigin("http://10.1.2.3/blobs/tok"); err == nil {
		t.Error("a plaintext upload target was accepted on an https API's host")
	}

	// The dial hook sees resolved addresses, so a name-based base URL has to be
	// recognised by where it resolves to, not by its spelling.
	named := New("https://localhost:8443/v1", "krowk_sk_secret")
	if !named.isAPIAddress("127.0.0.1:8443") {
		t.Error("the API host's resolved address was not recognised as the API's own")
	}
	if named.isAPIAddress("127.0.0.1:9999") {
		t.Error("a different port on the API's resolved address was recognised as the API's own")
	}
}

// checkRedirect permits an API-origin request to upgrade http -> https, and the
// dial that hop then makes goes to port 443, not the base's — so the API-host
// exemption must cover it, or a private self-hosted registry behind an https
// front fails on every request. The exemption is that one extra port in the
// upgrade direction, not the host wholesale and not a downgrade.
func TestTheAPIHostsHTTPSUpgradeMayBeDialled(t *testing.T) {
	noProxy(t)
	client := New("http://10.1.2.3/v1", "krowk_sk_secret")

	from, _ := http.NewRequest(http.MethodPost, "http://10.1.2.3/v1/artifacts", nil)
	to, _ := http.NewRequest(http.MethodPost, "https://10.1.2.3/v1/artifacts", nil)
	if err := client.checkRedirect(to, []*http.Request{from}); err != nil {
		t.Fatalf("the upgrade hop was refused at the redirect: %v", err)
	}
	if err := client.permitDial(context.Background(), "10.1.2.3:443"); err != nil {
		t.Errorf("the upgrade hop's dial was refused: %v", err)
	}
	for _, address := range []string{"10.1.2.3:8443", "10.1.2.4:443"} {
		if err := client.permitDial(context.Background(), address); err == nil {
			t.Errorf("%s was permitted alongside the upgrade port", address)
		}
	}

	// The upload leg agrees with the dial: a blob URL on the upgraded origin is
	// accepted — same host, default https port, nothing wider.
	if _, err := client.storageOrigin("https://10.1.2.3/v1/blobs/tok"); err != nil {
		t.Errorf("an upload target on the upgraded API origin was refused: %v", err)
	}
	if _, err := client.storageOrigin("https://10.1.2.3:8443/v1/blobs/tok"); err == nil {
		t.Error("an upload target on another port of the API host was accepted")
	}
	if _, err := client.storageOrigin("https://10.1.2.4/v1/blobs/tok"); err == nil {
		t.Error("an upload target on another private host was accepted")
	}

	// And the redirect gate agrees with both: a leg that started on the upgraded
	// origin may redirect within its host, and nowhere else.
	ufrom, _ := http.NewRequest(http.MethodPut, "https://10.1.2.3/v1/blobs/tok", nil)
	uto, _ := http.NewRequest(http.MethodGet, "https://10.1.2.3/v1/blobs/moved", nil)
	if err := client.checkRedirect(uto, []*http.Request{ufrom}); err != nil {
		t.Errorf("a same-host redirect on the upgraded leg was refused: %v", err)
	}
	away, _ := http.NewRequest(http.MethodGet, "https://10.1.2.4/v1/blobs/tok", nil)
	if err := client.checkRedirect(away, []*http.Request{ufrom}); err == nil {
		t.Error("a redirect off the upgraded origin was followed")
	}

	// An https base earns no port 80: the redirect there is a downgrade, and
	// checkRedirect refuses it before any dial.
	if err := New("https://10.1.2.3/v1", "krowk_sk_secret").
		permitDial(context.Background(), "10.1.2.3:80"); err == nil {
		t.Error("an https API's host was permitted on port 80")
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
	client := New("https://api.krowk.com/v1", "krowk_sk_secret")

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.HTTP.Do(req); err == nil {
		t.Fatal("the client connected to a loopback address")
	} else if !strings.Contains(err.Error(), "untrusted_endpoint") {
		t.Errorf("error = %v, want it to name untrusted_endpoint", err)
	}

	// The hook judges the address after resolution, so a name is judged by where
	// it lands: the same server reached as "localhost" is refused the same way
	// its literal address is. This is the property a URL-level check cannot have,
	// and the one a hook that runs before resolution silently loses.
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	named, err := http.NewRequest(http.MethodGet, "http://localhost:"+u.Port(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.HTTP.Do(named); err == nil {
		t.Fatal("the client connected to a loopback address reached by name")
	} else if !strings.Contains(err.Error(), "untrusted_endpoint") {
		t.Errorf("error = %v, want it to name untrusted_endpoint", err)
	}

	// A private address is refused the same way a loopback one is.
	if err := client.permitDial(context.Background(), "10.0.0.7:3128"); err == nil {
		t.Error("a private address was permitted with no proxy configured")
	}

	// A local registry means local addresses throughout, so the hook stands down.
	local := New(DevBaseURL, "")
	if err := local.permitDial(context.Background(), "127.0.0.1:8787"); err != nil {
		t.Errorf("a local registry was refused its own address: %v", err)
	}
}

// A zone makes an address unparseable, and permitDial judges what it can parse —
// so without stripping it, "fe80::1%eth0" is waved through at the dial.
func TestTheDialerStripsAnIPv6Zone(t *testing.T) {
	noProxy(t)
	client := New("https://api.krowk.com/v1", "krowk_sk_secret")

	for _, address := range []string{"[fe80::1%eth0]:443", "[fe80::1]:443", "[64:ff9b::a9fe:a9fe]:443"} {
		if err := client.permitDial(context.Background(), address); err == nil {
			t.Errorf("%s was permitted", address)
		}
	}
}

// Behind a proxy every connection goes to the proxy, and corporate proxies sit on
// private addresses — so judging the dialled address there refuses every upload
// from the environments this tool exists for.
func TestUploadsWorkBehindAProxyOnAPrivateAddress(t *testing.T) {
	var mu sync.Mutex
	var proxied int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxied++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	client := New("https://api.krowk.com/v1", "krowk_sk_secret")
	// The proxy is on loopback here, which is the shape of a proxy on 10.0.0.0/8.
	// The request is plain http so it is forwarded rather than tunnelled with
	// CONNECT, which a bare test server cannot answer.
	client.HTTP.Transport.(*proxyStamp).base.Proxy = func(*http.Request) (*url.URL, error) {
		return url.Parse(proxy.URL)
	}

	req, err := http.NewRequest(http.MethodGet, "http://storage.example.com/blob", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.HTTP.Do(req); err != nil {
		t.Fatalf("an upload behind a proxy was refused: %v", err)
	}
	mu.Lock()
	if proxied != 1 {
		t.Errorf("the proxy saw %d requests, want 1", proxied)
	}
	mu.Unlock()

	// Only the proxy this request goes through earns the exemption. A dial with
	// no proxy stamped on it — the transport chose to go direct — is judged like
	// any other, so configuring a proxy must not switch the boundary off.
	for _, address := range []string{"169.254.169.254:80", "10.0.0.99:443", "127.0.0.1:9999"} {
		if err := client.permitDial(context.Background(), address); err == nil {
			t.Errorf("%s was permitted because a proxy is configured elsewhere", address)
		}
	}
}

// The exemption is for the proxy the transport selected for this request, not
// for any proxy configured in the environment. HTTP_PROXY is never consulted
// for an https request, so such a request going direct must be judged like any
// other, even when its dial lands on the proxy's own address — the registry
// controls the port, and rebinding controls the IP.
func TestAProxyForAnotherSchemeEarnsNoExemption(t *testing.T) {
	noProxy(t)
	client := New("https://api.krowk.com/v1", "krowk_sk_secret")
	// The shape HTTP_PROXY=http://127.0.0.1:9999 gives the transport: a proxy for
	// http requests, direct for everything else.
	client.HTTP.Transport.(*proxyStamp).base.Proxy = func(req *http.Request) (*url.URL, error) {
		if req.URL.Scheme == "http" {
			return url.Parse("http://127.0.0.1:9999")
		}
		return nil, nil
	}

	req, err := http.NewRequest(http.MethodGet, "https://127.0.0.1:9999/blob", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.HTTP.Do(req); err == nil {
		t.Fatal("a direct dial to the http proxy's address was permitted for an https request")
	} else if !strings.Contains(err.Error(), "untrusted_endpoint") {
		t.Errorf("error = %v, want it to name untrusted_endpoint", err)
	}
}

func TestHostAddressesDerivesTheSchemesDefaultPort(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"http://10.0.0.7:3128", "10.0.0.7:3128"},
		{"http://10.0.0.7", "10.0.0.7:80"},
		{"https://10.0.0.7", "10.0.0.7:443"},
		{"socks5://10.0.0.7", "10.0.0.7:1080"},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := hostAddresses(u.Hostname(), u.Port(), u.Scheme); !slices.Contains(got, tc.want) {
			t.Errorf("hostAddresses(%q) = %v, want it to include %q", tc.raw, got, tc.want)
		}
	}
}

// The client vets where a request starts, and only CheckRedirect covers where
// a 302 sends it next: Go forwards Authorization on any same-host hop, so a
// registry answering with a redirect to another port on its own host would
// otherwise deliver the token there.
func TestRedirectsAreRefusedWithoutLeakingTheToken(t *testing.T) {
	var mu sync.Mutex
	var hijacked []string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hijacked = append(hijacked, r.Header.Get("Authorization"))
		mu.Unlock()
	}))
	defer elsewhere.Close()

	// Same host as the attacker's server — both 127.0.0.1 — different port,
	// which is exactly the hop the default client follows with the token on.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()

	c, _ := testClient(srv)
	_, err := c.ShowArtifact(context.Background(), "art_x")
	if err == nil {
		t.Fatal("a redirected API call should fail, not be followed")
	}
	if code := err.(*Error).Code(); code != "unexpected_redirect" {
		t.Errorf("error = %q, want unexpected_redirect", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hijacked) != 0 {
		t.Errorf("the redirect was followed %d time(s), carrying Authorization %q", len(hijacked), hijacked)
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
		// Nor defeat the cap outright: this value overflows time.Duration if
		// multiplied first, wrapping negative and slipping under the min.
		{"9223372036854775807", 60 * time.Second, true},
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
	var mu sync.Mutex
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Key{
			Valid:     true,
			KeyID:     "key_7f3a",
			Workspace: "acme",
			Scopes:    []string{"artifacts:read", ScopeWrite},
		})
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "krowk_sk_secret")
	key, err := client.VerifyKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if auth != "Bearer krowk_sk_secret" {
		t.Errorf("Authorization = %q", auth)
	}
	mu.Unlock()
	if key.KeyID != "key_7f3a" || key.Workspace != "acme" {
		t.Errorf("key = %+v", key)
	}
	if !key.HasScope(ScopeWrite) || key.HasScope("artifacts:delete") {
		t.Errorf("scopes = %v", key.Scopes)
	}
	if key.Status != http.StatusOK {
		t.Errorf("status = %d, want the 200 the answer arrived with", key.Status)
	}
}

// A 200 is not the same as a yes.
func TestVerifyKeyTreatsValidFalseAsRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Key{Valid: false})
	}))
	defer srv.Close()

	_, err := New(srv.URL+"/v1", "krowk_sk_secret").VerifyKey(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "invalid_key" {
		t.Fatalf("err = %v, want invalid_key", err)
	}
	// The verdict rode in on a 200; carrying that status is what lets doctor
	// tell "the registry said no" apart from "nothing answered at all".
	if apiErr.Status != http.StatusOK {
		t.Errorf("status = %d, want the 200 the verdict arrived with", apiErr.Status)
	}
}

func TestVerifyKeyPassesTheRegistrysRejectionThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"key_revoked","message":"this key was revoked — issue a new one"}}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL+"/v1", "krowk_sk_secret").VerifyKey(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "key_revoked" || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want the registry's own key_revoked", err)
	}
	if msg, _ := apiErr.Body["message"].(string); !strings.Contains(msg, "revoked") {
		t.Errorf("message = %q, want the registry's own explanation", msg)
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

// A cancelled context stops the loop before any request goes out, so an agent
// that gives up does not keep hammering the registry from a helper goroutine.
func TestCancelledContextStopsBeforeAnyRequest(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
	}))
	defer srv.Close()

	c, _ := testClient(srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.ShowArtifact(ctx, "art_x")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "cancelled" {
		t.Fatalf("err = %v, want cancelled", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Errorf("calls = %d, want none after cancellation", calls)
	}
}
