package cli

import (
	"errors"
	"net/http"

	"github.com/krowkcom/cli/internal/api"
)

// The exit codes krowk answers with. They are a contract: a script branches on
// the number without parsing anything, so what a number means may never quietly
// change, and a failure must never be reported as success.
//
// The taxonomy is deliberately small, and it is derived from what the caller
// would do next rather than from the registry's codes one for one — there are
// two dozen of those, and a shell script cannot hold two dozen cases. Each code
// here answers a different question: retry now, retry with a key, fix the
// command, or give up on this artifact for good.
//
// 1 stays the catch-all it has always been, so everything that checks `!= 0`
// keeps working and every mapping added later can only move a failure *out* of
// the crowd, never into it.
const (
	exitOK = 0
	// exitUsage is the command being wrong, or krowk itself failing: a bad flag,
	// an unknown command, a file that cannot be read, a port that will not bind.
	// Nothing about the registry is implied.
	exitUsage = 1
	// exitNotFound is a 404: no such artifact or run in this workspace, or no
	// such endpoint at this base URL. Both are "what you named is not there".
	exitNotFound = 2
	// exitAuth is the request lacking authority: no key where one is needed, a
	// key the registry will not accept, or a claim token that authorises nothing.
	// The fix is credentials, and nothing else.
	exitAuth = 3
	// exitRefused is the registry understanding the request and refusing it on
	// the state of things: an upload already finalized, a run that needs a key, a
	// field that does not validate. Retrying unchanged gets the same answer.
	exitRefused = 4
	// exitRateLimited is 429 and nothing else. Alone among the failures, waiting
	// is the whole fix — which is why it does not sit inside exitRefused.
	exitRateLimited = 5
	// exitUnreachable is the bytes not moving: the registry not answering, object
	// storage not answering, or storage refusing the transfer. Nothing has been
	// learned about the request itself, so retrying is reasonable.
	exitUnreachable = 6
	// exitServer is the registry failing on its side: a 5xx, or a success status
	// carrying a body this client cannot read. Also worth retrying, but it is a
	// different thing to report than a network that is down.
	exitServer = 7
	// exitGone is 410: the artifact existed and does not any more, either expired
	// or taken down. Its own code because krowk's callers care — a link in a pull
	// request that is gone calls for a fresh upload, where a 404 calls for
	// checking the slug that was typed.
	exitGone = 8
)

// clientCodes are the failures krowk raises itself, before or around a response.
// They are matched whatever status came back — a 403 from object storage on an
// expired upload URL is a storage failure to retry, not the caller's key being
// refused — so this table is consulted before any status is read.
var clientCodes = map[string]int{
	// No answer at all from the registry or from storage, and a transfer storage
	// would not take. All three mean the same thing to the caller: the bytes did
	// not move, try again.
	"network_unreachable":     exitUnreachable,
	"storage_unreachable":     exitUnreachable,
	"storage_rejected_upload": exitUnreachable,

	// An answer arrived, but not one this client can act on. That is the
	// registry's side of the contract broken, which is what exitServer names.
	"malformed_response": exitServer,

	// krowk knows there is no authority to send before it sends anything: no key
	// stored, no claim token for an anonymous takedown or for `claim` itself. The
	// registry would answer 401 to the first; saying so without the round trip
	// must not report a different exit code than making it. The others are a
	// credential the caller has to produce, which is what exitAuth means — as
	// against a *malformed* second positional, which is `bad_claim_token` and
	// stays a usage mistake.
	"not_authenticated": exitAuth,
	"missing_claim":     exitAuth,
	// A workspace resolved by name — a flag, a variable, a config file — and
	// holds no key. The command was right and the registry was never asked;
	// the fix is a credential for that workspace, and nothing else.
	"no_key_for_workspace": exitAuth,

	// A page krowk will not hand to the desktop's URL handler: a scheme that is not
	// the web, or http where the API itself is https. It sits with
	// `malformed_response` because it is the same news — the registry answered
	// something this client will not act on — and the other refusals from that same
	// check already land there. A caller has nothing to fix in its own command,
	// which is what would make it a usage failure.
	"refused_verification_url": exitServer,

	// A browser login that ended without a key. Denied is a person saying no,
	// which leaves the machine exactly as credential-less as never having asked.
	// Lapsed is krowk concluding the window closed rather than the registry saying
	// so — and it reports the same exitGone the registry's own 410 would, because
	// deciding it locally must not classify differently than being told.
	"authorization_denied":  exitAuth,
	"authorization_expired": exitGone,
	// A browser login asked for where nothing can approve one. The same class as no
	// key being stored, and the same number the fast failure it replaces reported:
	// the fix is a credential, and nothing else.
	"no_one_to_approve": exitAuth,
}

// registryCodes map the registry's error codes, and are only consulted for an
// error that actually arrived on a response. The distinction matters: krowk
// raises `bad_request` itself for a body it could not encode, which is a bug in
// krowk (exitUsage) rather than a request the registry refused (exitRefused).
// A code means what the registry means by it only when the registry said it.
var registryCodes = map[string]int{
	// Two different 404s, one class of answer: what was named is not there. The
	// fixes differ — a slug is checked against its workspace, an endpoint against
	// the base URL — and the `fix` line already says which, so the exit code does
	// not have to split them.
	"not_found":        exitNotFound,
	"no_such_endpoint": exitNotFound,

	"unauthorized": exitAuth,

	// Refusals on the state of things, and refusals on the content of the
	// request. They are one exit code because the caller does the same thing with
	// both: read the message, change something, and only then send it again.
	"already_finalized":      exitRefused,
	"idempotency_key_reused": exitRefused,
	"upload_missing":         exitRefused,
	"run_needs_key":          exitRefused,
	"checksum_mismatch":      exitRefused,
	"empty_upload":           exitRefused,
	"invalid":                exitRefused,
	"parameter_missing":      exitRefused,
	"bad_request":            exitRefused,

	"too_many_requests": exitRateLimited,

	// The registry saying object storage is unreachable is the same news as
	// krowk failing to reach it — the bytes did not move — so it reads as
	// unreachable rather than as the 502 it arrives as.
	"storage_unavailable": exitUnreachable,

	"expired":    exitGone,
	"taken_down": exitGone,
	// A one-shot secret the registry has already handed over and keeps no second
	// copy of: a browser login whose key was collected. Gone for the same reason a
	// lapsed artifact is — it existed, it does not now, and no retry brings it
	// back. Asking again means asking for a new one.
	"spent": exitGone,

	"internal_server_error": exitServer,

	// `unexpected_error` is deliberately absent. The registry mints it for any
	// status it has nothing specific to say about — a 405 and a 503 both arrive
	// under it — so mapping the code would flatten statuses that mean different
	// things. It falls through to the status instead, which is the only thing
	// that response carries.
}

// exitCodeFor is the whole mapping from a failure to the process exit code. It
// is total: anything it does not recognise is exitUsage, which is where every
// failure lived before this table existed.
func exitCodeFor(err error) int {
	if err == nil {
		return exitOK
	}

	// Anything that is not an *api.Error never crossed the wire — it is krowk's
	// own plumbing failing, and there is no status or code to reason about.
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return exitUsage
	}

	code := apiErr.Code()
	if exit, ok := clientCodes[code]; ok {
		return exit
	}
	// A status of zero means nothing answered, so no registry code was involved:
	// this is one of krowk's own api.Fail codes wearing a name the registry also
	// uses. Skipping the table here is what keeps those apart.
	if apiErr.Status != 0 {
		if exit, ok := registryCodes[code]; ok {
			return exit
		}
	}
	return exitCodeForStatus(apiErr.Status)
}

// exitCodeForStatus is the fallback for a response krowk has no code for: a
// status the registry answered under `unexpected_error`, or a code minted after
// this table was written. Reading the status keeps a new registry code landing
// in roughly the right class instead of in the catch-all.
func exitCodeForStatus(status int) int {
	switch {
	case status == 0:
		// Nothing answered and no code matched, so there is nothing to classify.
		return exitUsage
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return exitAuth
	case status == http.StatusNotFound:
		return exitNotFound
	case status == http.StatusGone:
		return exitGone
	case status == http.StatusTooManyRequests:
		return exitRateLimited
	case status >= 500:
		return exitServer
	case status >= 400:
		// Every other refusal the registry can answer with. It understood the
		// request and would not do it, which is what exitRefused says.
		return exitRefused
	}
	// What is left is a failure reported against a status below 400, which in
	// practice is `unexpected_redirect`: a 3xx krowk refused to follow. That is a
	// decision krowk made about a URL rather than a verdict from the registry, so
	// it belongs with krowk's own failures.
	return exitUsage
}
