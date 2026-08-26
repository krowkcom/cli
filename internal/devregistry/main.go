// Command devregistry runs a local stand-in for api.krowk.com, so developing
// against a registry needs neither the network nor a checkout of the registry
// itself. It lives under internal/ and is built by no release: run it straight
// from a checkout with `go run ./internal/devregistry`, then talk to it with
// `krowk push screenshot.png --dev`.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/registry"
)

// defaultAddr is where the stand-in listens, matching api.DevBaseURL so --dev
// finds it with no configuration.
//
// Loopback, not ":8787". This registry takes uploads without a key and serves
// their bytes to anyone who can reach it. On a café or office network, binding
// every interface hands that to whoever is nearby. A wider bind stays possible,
// but it has to be asked for.
const defaultAddr = "127.0.0.1:8787"

func main() {
	var (
		addr       string
		site       string
		limitBytes int64
	)
	flag.StringVar(&addr, "addr", defaultAddr, "listen address (loopback only by default)")
	flag.StringVar(&site, "site", "", "origin for the links returned (default: the request host)")
	flag.Int64Var(&limitBytes, "limit-bytes", 0, "reject uploads above this size")
	flag.Parse()

	// registry.Handler treats <= 0 as "use the default", so a negative limit
	// would silently mean 100 MiB. Reject it instead of guessing.
	if limitBytes < 0 {
		fmt.Fprintln(os.Stderr, "--limit-bytes must not be negative — omit it or pass 0 for the default")
		os.Exit(1)
	}
	if err := usableAddr(addr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Bind before announcing anything, so a script keying off the banner never
	// proceeds against a port that failed to open.
	if err := run(os.Stdout, addr, site, limitBytes); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run binds addr, announces itself on w, and serves until the listener closes.
// Split from main so a test can hold the outcome without a process. The flag
// default is where the default address is decided; run takes the address as
// given, and Go would happily treat an empty one as "every interface", which
// is exactly what this command must never do by accident — main rejects it
// with usableAddr before getting here.
func run(w io.Writer, addr, site string, limitBytes int64) error {
	if addr == "" {
		return &bindError{addr: addr, err: errors.New("no address given")}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return &bindError{addr: addr, err: err}
	}
	return serve(w, ln, addr, site, limitBytes)
}

// bindError is a listen that never happened. Typed so the banner-before-error
// guarantee is checkable: nothing may print until the bind succeeds.
type bindError struct {
	addr string
	err  error
}

func (e *bindError) Error() string {
	return fmt.Sprintf("could not listen on %s: %v", e.addr, e.err)
}

// serve announces the already-bound listener on w and runs until it closes.
// Split from the bind in main so a test can hold the listener's fate without a
// process. asked is the address as given, which the listener flattens to an IP;
// the banner keeps the name for the URL it prints.
func serve(w io.Writer, ln net.Listener, asked, site string, limitBytes int64) error {
	fmt.Fprint(w, banner(ln.Addr().String(), asked))

	server := &http.Server{
		Handler: registry.Handler(limitBytes, site),
		// Uploads can be slow and large, so only the header read is bounded.
		ReadHeaderTimeout: 20 * time.Second,
	}
	if err := server.Serve(ln); err != nil {
		return fmt.Errorf("registry on %s stopped: %w", asked, err)
	}
	return nil
}

// banner is what the stand-in prints once the listener is bound: where the
// registry is, how to point a push at it, and — bound wider than this machine —
// that it is open. Kept separate so it can be checked without a bind.
func banner(bound, asked string) string {
	base := localBase(bound, asked)

	lines := []string{"krowk registry listening on " + base}
	if reachableByDev(asked) {
		lines = append(lines, "  krowk push screenshot.png --dev")
	} else {
		// --dev only knows the default address, so say what to use instead.
		lines = append(lines, "  KROWK_API_URL="+base+"/v1 krowk push screenshot.png")
	}
	// Bound wider than this machine, deliberately or not, it is worth saying out
	// loud: this registry takes uploads without a key and serves their bytes to
	// anyone who can reach it.
	if host := listenHost(asked); !isLoopbackHost(host) {
		lines = append(lines,
			"  ! reachable from the network on "+bindDescription(host)+" — it needs no key to accept uploads")
	}
	return strings.Join(lines, "\n") + "\n"
}

func bindDescription(host string) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "every interface"
	}
	return host
}

// localBase turns a listen address into a URL a client can call. bound is what
// the listener reports, which resolves ":0" to the real port; asked keeps the
// hostname the user typed, since the listener flattens it to an IP.
func localBase(bound, asked string) string {
	host, port, err := net.SplitHostPort(asked)
	if err != nil {
		return "http://" + asked
	}
	// An asked port of 0 means "any port", and only the bound address knows
	// which one it became.
	if port == "0" {
		if _, boundPort, splitErr := net.SplitHostPort(bound); splitErr == nil {
			port = boundPort
		}
	}
	// Wildcard binds listen everywhere but dial nowhere; localhost is the
	// loopback name that reaches them on either stack. Loopback IPs fold to
	// the same name, so the default bind stays the address --dev dials.
	switch host {
	case "", "0.0.0.0", "::", "127.0.0.1", "::1":
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// reachableByDev reports whether --dev, which knows only one address, will find
// a registry listening here.
func reachableByDev(addr string) bool {
	dev, err := url.Parse(api.DevBaseURL)
	if err != nil {
		return false
	}
	if listenPort(addr) != dev.Port() {
		return false
	}
	// --dev dials localhost, which lands on 127.0.0.1 or ::1 — so only those
	// hosts, their name, and every-interface binds qualify. The rest of
	// 127.0.0.0/8 is loopback too, but localhost does not reach it, so the
	// advice for 127.0.0.2 is KROWK_API_URL, not a --dev that cannot connect.
	switch listenHost(addr) {
	case "", "0.0.0.0", "::", "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func listenHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return host
}

func listenPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return port
}

// usableAddr rejects what net.Listen would reject anyway, so the banner does not
// print "http://localhost:" and a network warning for an address that never
// binds. --addr 8787 is the easy mistake.
// An empty port is accepted by SplitHostPort but binds a kernel-picked port the
// banner has no name for — ":0" is the explicit spelling of that request, and it
// is welcome: the bind happens before the banner, which then reports the port
// the kernel picked. An empty host is fine: that is what ":8787" means.
func usableAddr(addr string) error {
	if _, port, err := net.SplitHostPort(addr); err != nil || !bindablePort(port) {
		return fmt.Errorf("--addr %q needs a host and a numeric port, like 127.0.0.1:8787 or :8787", addr)
	}
	return nil
}

// bindablePort reports whether port is one the listener can take as given: all
// digits (net.Listen also resolves signs and service names like "http", which
// the banner would print verbatim), within 0-65535 — 99999 announces itself and
// then fails to bind — and spelled the way it binds, since ":08787" binds 8787.
// Port 0 asks the kernel to pick, and the banner reports what it picked.
func bindablePort(port string) bool {
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n <= 65535 && strconv.Itoa(n) == port
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
