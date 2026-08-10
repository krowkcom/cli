package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/output"
)

// Every case the taxonomy claims to answer, pinned. A change to any number here
// is a change to the contract a script depends on, so it should be a change
// somebody had to make on purpose.
func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil is success", nil, 0},

		// krowk's own failures, which never reached the wire.
		{"plain error", errors.New("something broke"), 1},
		{"bad flag", api.Fail("bad_flag", ""), 1},
		{"unknown command", api.Fail("unknown_command", ""), 1},
		{"unreadable file", api.Fail("file_unreadable", ""), 1},
		{"local registry will not bind", api.Fail("registry_unavailable", ""), 1},
		// A second positional that is not token-shaped is a mistyped command line,
		// caught on shape before any credential is weighed — so it is usage, where
		// the missing token below is authority.
		{"a stray second positional on a takedown", api.Fail("bad_claim_token", ""), 1},
		// The one code krowk shares with the registry: raised here it is krowk
		// failing to build a request, not the registry refusing one.
		{"krowk cannot encode the body", api.Fail("bad_request", ""), 1},

		// Missing credentials, known before anything is sent.
		{"no key stored", api.Fail("not_authenticated", ""), 3},
		{"anonymous takedown with no claim token", api.Fail("missing_claim", ""), 3},
		// A browser login somebody said no to leaves the machine as
		// credential-less as never having asked.
		{"browser login denied", api.Fail("authorization_denied", ""), 3},

		// Transport, on either leg.
		{"registry unreachable", registryErr(0, "network_unreachable"), 6},
		{"storage unreachable", registryErr(0, "storage_unreachable"), 6},
		{"storage refused the bytes", registryErr(403, "storage_rejected_upload"), 6},
		{"storage refused with a 500", registryErr(500, "storage_rejected_upload"), 6},
		{"registry says storage is down", registryErr(502, "storage_unavailable"), 6},

		// Not found, both spellings.
		{"no such artifact", registryErr(404, "not_found"), 2},
		{"no such endpoint", registryErr(404, "no_such_endpoint"), 2},

		// Auth.
		{"key rejected", registryErr(401, "unauthorized"), 3},

		// Refused: on state, and on content.
		{"already finalized", registryErr(409, "already_finalized"), 4},
		{"idempotency key reused", registryErr(409, "idempotency_key_reused"), 4},
		{"bytes never landed", registryErr(409, "upload_missing"), 4},
		{"run needs a key", registryErr(422, "run_needs_key"), 4},
		{"checksum mismatch", registryErr(422, "checksum_mismatch"), 4},
		{"empty upload", registryErr(422, "empty_upload"), 4},
		{"validation", registryErr(422, "invalid"), 4},
		{"parameter missing", registryErr(400, "parameter_missing"), 4},
		{"unreadable body", registryErr(400, "bad_request"), 4},

		{"rate limited", registryErr(429, "too_many_requests"), 5},

		{"expired", registryErr(410, "expired"), 8},
		{"taken down", registryErr(410, "taken_down"), 8},
		{"browser login's key already collected", registryErr(410, "spent"), 8},
		// krowk deciding the login's window closed rather than being told, which
		// must land on the same number as being told.
		{"browser login lapsed", api.Fail("authorization_expired", ""), 8},

		{"registry blew up", registryErr(500, "internal_server_error"), 7},
		{"registry answered something unreadable", registryErr(200, "malformed_response"), 7},

		// The registry's catch-all carries no class of its own, so the status it
		// arrived with is what decides.
		{"unexpected error on a 405", registryErr(405, "unexpected_error"), 4},
		{"unexpected error on a 503", registryErr(503, "unexpected_error"), 7},

		// A code minted after this table was written still lands somewhere sane.
		{"unknown code on a 4xx", registryErr(422, "brand_new_refusal"), 4},
		{"unknown code on a 5xx", registryErr(503, "brand_new_outage"), 7},
		{"unknown code on a 403", registryErr(403, "brand_new_refusal"), 3},
		{"unknown code on a 410", registryErr(410, "brand_new_gone"), 8},

		// A 3xx krowk declined to follow is krowk's decision, not a verdict.
		{"unexpected redirect", registryErr(302, "unexpected_redirect"), 1},

		// Nothing to go on at all.
		{"no code and no status", &api.Error{Body: map[string]any{}}, 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exitCodeFor(c.err); got != c.want {
				t.Errorf("exitCodeFor(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// The mapping has to see through a wrapped error, since that is how the CLI
// hands failures up through the command functions.
func TestExitCodeForWrapped(t *testing.T) {
	err := fmt.Errorf("pushing screenshot.png: %w", registryErr(410, "taken_down"))
	if got := exitCodeFor(err); got != 8 {
		t.Errorf("wrapped taken_down = %d, want 8", got)
	}
}

// The mapping is only worth anything if the process really exits with it, so
// each class is provoked through Run against a real registry rather than by
// handing exitCodeFor an error built by the test.
func TestExitCodesComeOutOfRun(t *testing.T) {
	t.Run("0 when it worked", func(t *testing.T) {
		h := newHarness(t, 0)
		if code, _, stderr := h.run("push", h.fixture); code != 0 {
			t.Fatalf("push exited %d, stderr:\n%s", code, stderr)
		}
	})

	t.Run("1 for a command that is not one", func(t *testing.T) {
		h := newHarness(t, 0)
		h.failsWith(1, "uploads", "frobnicate")
	})

	t.Run("2 for a slug the workspace does not hold", func(t *testing.T) {
		h := newHarness(t, 0)
		h.failsWith(2, "uploads", "show", "art_nosuchartifact00001")
	})

	t.Run("3 when there is no key to list with", func(t *testing.T) {
		h := newHarness(t, 0).anonymous()
		h.failsWith(3, "uploads", "list")
	})

	t.Run("4 when the registry refuses the request", func(t *testing.T) {
		h := newHarness(t, 10) // a 10-byte cap the fixture cannot meet
		h.failsWith(4, "push", h.fixture)
	})

	t.Run("6 when nothing answers", func(t *testing.T) {
		h := newHarness(t, 0)
		h.env["KROWK_API_URL"] = "http://127.0.0.1:1/v1"
		h.failsWith(6, "push", h.fixture)
	})

	t.Run("8 for an artifact that was taken down", func(t *testing.T) {
		h := newHarness(t, 0)
		pushed := only(t, h.ok("push", h.fixture))
		h.ok("uploads", "delete", pushed.Slug)
		h.failsWith(8, "uploads", "show", pushed.Slug)
	})
}

// The table is a contract, so the codes it uses have to be the ones documented.
// Anything outside the published range would be a number no caller can read.
func TestExitCodesStayInRange(t *testing.T) {
	for name, table := range map[string]map[string]int{
		"clientCodes":   clientCodes,
		"registryCodes": registryCodes,
	} {
		for code, exit := range table {
			if exit < exitNotFound || exit > exitGone {
				t.Errorf("%s[%q] = %d, outside the documented 2-8", name, code, exit)
			}
		}
	}
}

// A code may not sit in both tables: which one wins would then decide the
// answer, and that is not a thing a reader should have to work out.
func TestNoCodeIsInBothTables(t *testing.T) {
	for code := range clientCodes {
		if _, ok := registryCodes[code]; ok {
			t.Errorf("%q is in both clientCodes and registryCodes", code)
		}
	}
}

// The help text is where a person reads the taxonomy, so it has to name every
// code the mapping can actually produce. Rendered through showHelp rather than
// the template directly, so the test reads what a person reads and survives the
// template growing arguments.
func TestHelpDocumentsEveryExitCode(t *testing.T) {
	var help strings.Builder
	if err := showHelp(&help, nil, output.Human); err != nil {
		t.Fatalf("showHelp: %v", err)
	}
	for exit := exitOK; exit <= exitGone; exit++ {
		if !strings.Contains(help.String(), fmt.Sprintf("\n  %d  ", exit)) {
			t.Errorf("help text does not document exit code %d", exit)
		}
	}
}

func registryErr(status int, code string) *api.Error {
	return &api.Error{Status: status, Body: map[string]any{"error": code}}
}
