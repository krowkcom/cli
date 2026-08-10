package cli

import (
	"net/url"
	"os/exec"
	"runtime"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/runctx"
)

// Opening a browser is the one thing `auth login` does that reaches outside the
// process, and it does it with a string the registry chose. Everything in this
// file exists because of that: what may be opened, when opening is even
// sensible, and how the handoff happens without the CLI's own streams or
// lifetime depending on whatever the desktop launches.

// browsableURL judges the page the registry named, and answers it back when it
// is safe to hand over.
//
// The URL arrives in a response body and its destination is a *URL handler*, not
// an HTTP client — so the scheme is the whole of the hazard. `file://` walks the
// local disk, `javascript:` runs in whatever is already open, and on a desktop
// with the usual handlers registered there are schemes that start a program with
// arguments. None of that is reachable over http or https, so the two are the
// only ones accepted, and the URL is passed as one argv element so nothing in it
// can become a second one.
//
// Which host is left to the registry on purpose. The app surface is a different
// origin from the API by design, and a self-hosted registry serves its own — so
// the host is configuration the caller already chose, the same reasoning that
// exempts `KROWK_API_URL`'s own origin from the upload boundary. https is
// required unless the API base URL is itself plain http, which is the caller
// having made that choice too: a local registry, or a self-hosted one behind a
// private network.
func browsableURL(raw, apiBaseURL string) (string, error) {
	if raw == "" {
		return "", api.Fail("malformed_response",
			"the registry opened a browser login without naming a page to approve it on — "+
				"check KROWK_API_URL points at the API host, not the website")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// The URL itself is deliberately not quoted back: it came from a response
		// body, and a failure message is the last place to echo an arbitrary string
		// of someone else's choosing at a terminal.
		return "", api.Fail("malformed_response",
			"the registry named a page for this login that is not a URL — "+
				"check KROWK_API_URL points at the API host, not the website")
	}
	switch u.Scheme {
	case "https":
	case "http":
		// An https registry may not talk its own login page down to http; a caller
		// who pointed krowk at an http registry already accepted that.
		if !insecureRegistry(apiBaseURL) {
			return "", api.Fail("refused_verification_url",
				"the registry named an http page for this login while the API itself is https — "+
					"krowk will not open it; check KROWK_API_URL")
		}
	default:
		return "", api.Fail("refused_verification_url",
			"the registry named a "+u.Scheme+": page for this login, and krowk only opens http and https")
	}
	return u.String(), nil
}

// insecureRegistry reports whether the registry being talked to is itself plain
// http, which is what makes an http login page the caller's own choice rather
// than the registry's.
func insecureRegistry(baseURL string) bool {
	u, err := url.Parse(baseURL)
	return err == nil && u.Scheme == "http"
}

// headless reports whether there is no browser here worth opening.
//
// Two answers, and they are different questions. A session reached over SSH has
// a browser at the other end of the connection rather than on the machine
// running krowk, so opening one locally either fails or — worse, on a shared
// box — opens a window somebody else is looking at. A unix session with no
// display server has nowhere to draw at all, which is a container and is the
// case this whole flow exists for; macOS and Windows have no such variable, and
// a graphical session there is the default rather than something to detect.
//
// It is a default, not a verdict: --no-browser forces this behaviour, and
// nothing forces the opposite because a browser that cannot open is not worth
// insisting on. The code and the URL are printed either way.
func headless(env runctx.Env) bool {
	if env("SSH_CONNECTION") != "" || env("SSH_CLIENT") != "" || env("SSH_TTY") != "" {
		return true
	}
	switch runtime.GOOS {
	case "darwin", "windows":
		return false
	}
	return env("DISPLAY") == "" && env("WAYLAND_DISPLAY") == ""
}

// openBrowser asks the desktop to open the page, and reports whether the
// handoff started. It is never the reason a login fails: the URL is printed
// before this runs, so a machine with no handler registered loses a convenience
// and nothing else.
//
// Started rather than waited on, and that is deliberate. `xdg-open` hands off
// and exits, but it is a shell script that may exec a browser in the
// foreground — waiting on it could hold the login open until the person closes
// their browser. The child is reaped in the background so it is not left a
// zombie for the minutes the poll can run.
func openBrowser(target string) bool {
	cmd := browserCommand(target)
	if cmd == nil {
		return false
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	go func() { _ = cmd.Wait() }()
	return true
}

// browserCommand is the platform's "open this URL" command. The URL is one argv
// element in every case — no shell is involved, so nothing in it is parsed as
// anything but a URL.
func browserCommand(target string) *exec.Cmd {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target)
	case "windows":
		// Not `cmd /c start`: start reads its first quoted argument as a window
		// title, and `&` in a query string ends the command. FileProtocolHandler
		// takes the URL and nothing else.
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		return exec.Command("xdg-open", target)
	}
}
