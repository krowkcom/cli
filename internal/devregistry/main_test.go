package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

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
		{"127.0.0.1:8787", defaultAddr, "http://localhost:8787"},
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
	host := listenHost(defaultAddr)
	if !isLoopbackHost(host) {
		t.Errorf("default listen host = %q, want loopback", host)
	}
	// And still on the port --dev looks for, or the shorthand breaks.
	if !reachableByDev(defaultAddr) {
		t.Errorf("--dev cannot reach the default address %q", defaultAddr)
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
		defaultAddr,
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
	go func() { done <- serve(io.Discard, ln, "127.0.0.1:0", "", 0) }()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	bannerText := banner(ln.Addr().String(), "127.0.0.1:0")
	if !strings.Contains(bannerText, "krowk registry listening on http://localhost:"+port+"\n") {
		t.Errorf("banner = %q, want it to carry the bound port %s", bannerText, port)
	}
	if !strings.Contains(bannerText, "KROWK_API_URL=http://localhost:"+port+"/v1") {
		t.Errorf("banner = %q, want KROWK_API_URL advice naming the bound port", bannerText)
	}

	// And a registry really answers on the port the kernel picked.
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("nothing answered on the bound port: %v", err)
	}
	resp.Body.Close()

	ln.Close()
	if serveErr := <-done; serveErr == nil || !strings.Contains(serveErr.Error(), "stopped") {
		t.Errorf("serve after the listener closed = %v, want the stop reported", serveErr)
	}
}

func TestTheBannerWarnsOnlyWhenBoundOffBox(t *testing.T) {
	// The default: loopback on the dev port, so --dev is the advice and there is
	// nothing to warn about.
	def := banner("127.0.0.1:8787", defaultAddr)
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
		got := banner(addr, addr)
		if !strings.Contains(got, "reachable from the network") {
			t.Errorf("%s should warn:\n%s", addr, got)
		}
		if !strings.Contains(got, "needs no key") {
			t.Errorf("%s: the warning should say why it matters:\n%s", addr, got)
		}
	}

	// A non-default port cannot be reached by --dev, so the banner says what can.
	other := banner("127.0.0.1:9000", "127.0.0.1:9000")
	if !strings.Contains(other, "KROWK_API_URL=http://localhost:9000/v1") {
		t.Errorf("banner should give the env var for a non-default port:\n%s", other)
	}
}

// The banner only appears after a successful bind, so a script keying off
// "listening" never proceeds against a port that failed to open.
func TestRunFailsToBindWithoutPrintingTheBanner(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	var out bytes.Buffer
	serveErr := run(&out, taken.Addr().String(), "", 0)

	var bindErr *bindError
	if !errors.As(serveErr, &bindErr) {
		t.Fatalf("run on a taken port = %v, want a bindError", serveErr)
	}
	if !strings.Contains(bindErr.Error(), taken.Addr().String()) {
		t.Errorf("error = %q, want it to name the address", bindErr.Error())
	}
	if out.Len() != 0 {
		t.Errorf("banner printed despite the failed bind: %q", out.String())
	}
}

// An empty --addr is rejected up front by usableAddr in main, so it never
// reaches run — but a caller of run that passes one deserves the bind failure,
// not a silent fallback to a default decided in two places.
func TestRunWithAnEmptyAddrFailsToBind(t *testing.T) {
	var out bytes.Buffer
	serveErr := run(&out, "", "", 0)

	var bindErr *bindError
	if !errors.As(serveErr, &bindErr) || out.Len() != 0 {
		t.Fatalf("run with an empty addr = %v (out %q), want a quiet bind failure", serveErr, out.String())
	}
}
