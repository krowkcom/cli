package api

import (
	"net/url"
	"strings"
)

// Slug kinds, spelled as the registry prefixes them. A kind is what a command
// is asking for: `uploads show` wants an artifact and `runs show` wants a run,
// and a link that carries the wrong one is worth saying so about rather than
// sending on to be answered as a 404.
const (
	KindArtifact  = "art"
	KindRun       = "run"
	KindWorkspace = "ws"
)

// slugNouns names each kind the way the help and the errors do, so a message
// about a run does not say "run kind" and one about an artifact does not say
// "art".
var slugNouns = map[string]string{
	KindArtifact:  "artifact",
	KindRun:       "run",
	KindWorkspace: "workspace",
}

// slugFloor is how much random tail a token needs before a link is read as
// carrying a slug. Canon mints exactly 24 base36 characters after the prefix —
// the website validates that shape before it calls the registry at all — so
// nothing real is turned away by a floor, and everything a URL is full of is:
// `run_id=4821` in a CI link, `art_report.png` beside a checkout, `run_7` in a
// job path. Each of those is `<kind>_<base36>` and none of them is a slug.
//
// A floor rather than 24 exactly, because this is a reader and not a validator:
// the registry stays the authority on what it mints, and a slug shorter than
// canon's would still resolve when passed bare, which is the path this function
// leaves untouched.
const slugFloor = 16

// ParseSlug takes what somebody typed where a slug belongs — the slug itself,
// or a link that carries one — and answers with the slug.
//
// The pitch is the pasted link: what an agent and a person both hold after an
// upload is `krowk.com/a/art_…`, or the CDN URL under it, not a bare slug they
// would have to cut out by hand. Taking the link is the whole leniency, and it
// is additive: anything that is not link-shaped comes back untouched, so a slug
// the registry would have accepted before is still passed on verbatim — this
// function is not a validator, and the registry stays the authority on what a
// real slug is.
//
// A link that carries no slug of the wanted kind is refused here rather than
// sent on, because the registry can only answer "no such record" about it,
// which reads as a slug that expired rather than as a URL in the wrong place.
func ParseSlug(kind, input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		// Nothing at all is not a mistake — an optional run is absent this way,
		// and the caller decides what that means. Whitespace is a mistake: it was
		// typed, or a shell expanded an empty variable into it, and letting it
		// read as absent would quietly widen `--run " "` from one run to a new
		// one and `uploads list --run " "` to the whole workspace.
		if input == "" {
			return "", nil
		}
		return "", Fail(slugFailure(kind),
			"a blank "+slugNoun(kind)+" is not one — pass the "+slugNoun(kind)+
				" slug, or a link that carries it")
	}
	if !looksLikeLink(trimmed) {
		return trimmed, nil
	}

	host, rest := splitLink(trimmed)
	found := slugsIn(kind, host, rest)
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		break
	default:
		// Two different records in one string and no way to know which was meant.
		// `uploads delete` is immediate and has no undo, so guessing here is how a
		// paste of two links, or of a page that mentions a second artifact, takes
		// down something nobody named.
		return "", Fail(slugFailure(kind),
			"that carries more than one "+slugNoun(kind)+" — `"+strings.Join(found, "`, `")+
				"` — so which one is meant is a guess; pass the one you want")
	}

	// Naming the kind that *is* there turns a dead end into the next command: a
	// card link handed to `runs show` is the artifact's link, and the run it
	// belongs to is one `uploads show` away.
	for _, other := range []string{KindArtifact, KindRun, KindWorkspace} {
		if other == kind {
			continue
		}
		if elsewhere := slugsIn(other, host, rest); len(elsewhere) > 0 {
			return "", Fail(slugFailure(kind),
				"that link names "+article(slugNoun(other))+" — `"+elsewhere[0]+"` — and this takes "+
					article(slugNoun(kind))+"; pass the "+slugNoun(kind)+
					" slug, or a link that carries one")
		}
	}

	return "", Fail(slugFailure(kind),
		"`"+trimmed+"` carries no "+slugNoun(kind)+" slug — pass "+slugExample(kind))
}

// slugsIn collects the distinct slugs of one kind a link carries, in the order
// they appear. Distinct, because the same slug twice is not an ambiguity: the
// markdown krowk hands back names one artifact in both halves of
// `[![name](file_url)](card_url)`, and pasting that whole line is a reasonable
// thing to do.
//
// The two spellings are read in the two places they exist. `art_…` is the
// identity, and it can turn up in any segment of a URL. `art-…` is only ever a
// DNS label — `art-{slug}.krowkusercontent.com` — where an underscore is not
// legal, so it is read in the host and nowhere else: a path like
// `/j/run-abcdefghij0123456789/log` on somebody else's CI belongs to them, and
// reading a slug out of it would invent a record krowk then asks about.
func slugsIn(kind, host, rest string) []string {
	var found []string
	add := func(slug string) {
		for _, seen := range found {
			if seen == slug {
				return
			}
		}
		found = append(found, slug)
	}

	for _, label := range strings.FieldsFunc(host, isSlugBoundary) {
		if slug, ok := slugSpelled(kind, "-", label); ok {
			add(slug)
		}
		if slug, ok := slugSpelled(kind, "_", label); ok {
			add(slug)
		}
	}
	for _, token := range strings.FieldsFunc(rest, isSlugBoundary) {
		if slug, ok := slugSpelled(kind, "_", token); ok {
			add(slug)
		}
	}
	return found
}

// splitLink separates the host from everything after it, so the hyphen spelling
// can be read where it is legal and not where it is not. Both halves come back
// lowercased and percent-decoded: a slug is lowercase base36, and a link that
// has been through an encoder on its way to being pasted still carries the same
// slug — `art%5F9f3c…` is one, spelled by a machine.
func splitLink(link string) (host, rest string) {
	decoded := link
	if unescaped, err := url.QueryUnescape(link); err == nil {
		decoded = unescaped
	}
	decoded = strings.ToLower(decoded)

	if scheme := strings.Index(decoded, "://"); scheme >= 0 {
		decoded = decoded[scheme+3:]
	}
	// A path, query or fragment ends the host; anything before the first of them
	// is the authority, userinfo and port included — all of which break on a slug
	// boundary anyway.
	if end := strings.IndexAny(decoded, "/?#"); end >= 0 {
		return decoded[:end], decoded[end:]
	}
	return decoded, ""
}

// slugNoun and slugFailure keep an unknown kind from being worse than useless:
// ParseSlug takes a plain string, so a caller can name a kind this file has
// never heard of, and the answer to that is a readable error rather than a
// panic on an empty noun.
func slugNoun(kind string) string {
	if noun, ok := slugNouns[kind]; ok {
		return noun
	}
	return kind
}

func slugFailure(kind string) string { return "bad_" + slugNoun(kind) }

// slugExample shows the shape that would have worked. Only the artifact has a
// public page to point at — `krowk.com/a/{slug}` is the card — so the other
// kinds are told to paste whatever krowk printed rather than a URL shape that
// does not exist.
func slugExample(kind string) string {
	if kind == KindArtifact {
		return "the artifact slug, like art_…, or the link krowk handed back, " +
			"like https://krowk.com/a/art_…"
	}
	// No URL shape is named for the others, because krowk prints none: a run
	// comes back as a slug, and inventing `krowk.com/a/run_…` here would send a
	// caller to the artifact card's path, where no run has ever lived.
	return "the " + slugNoun(kind) + " slug, like " + kind + "_…, or a link carrying it"
}

// looksLikeLink is what separates a slug from a URL that holds one. A slug is
// one word: no scheme, no path, no host. So anything carrying a separator is
// treated as a link, and everything else is left exactly as it was typed.
func looksLikeLink(s string) bool {
	return strings.ContainsAny(s, "/:.")
}

// isSlugBoundary is every character a slug cannot contain, which is what makes
// the surrounding link fall away: the scheme, the host's labels, the path's
// segments, a query and a fragment all break on one of these. `_` and `-` do
// not, because both spellings of a slug are built with them.
func isSlugBoundary(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return false
	case r == '_', r == '-':
		return false
	}
	return true
}

// slugSpelled reads one token as a slug of the given kind in one spelling, and
// answers in the underscore one — that is the identity every endpoint is
// addressed by; the hyphen is only how DNS spells it.
func slugSpelled(kind, separator, token string) (string, bool) {
	rest, ok := strings.CutPrefix(token, kind+separator)
	if !ok || len(rest) < slugFloor || !isBase36(rest) {
		return "", false
	}
	return kind + "_" + rest, true
}

// isBase36 is the slug alphabet: lowercase letters and digits.
func isBase36(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// article keeps the error sentences readable — "an artifact", "a run".
func article(noun string) string {
	if noun == "" {
		return "one"
	}
	if strings.ContainsRune("aeiou", rune(noun[0])) {
		return "an " + noun
	}
	return "a " + noun
}
