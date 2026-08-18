package api

import "strings"

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

	tokens := strings.FieldsFunc(strings.ToLower(trimmed), isSlugBoundary)
	for _, token := range tokens {
		if slug, ok := slugOfKind(kind, token); ok {
			return slug, nil
		}
	}

	// Naming the kind that *is* in the link turns a dead end into the next
	// command: a card link handed to `runs show` is the artifact's link, and the
	// run it belongs to is one `uploads show` away.
	for _, other := range []string{KindArtifact, KindRun, KindWorkspace} {
		if other == kind {
			continue
		}
		for _, token := range tokens {
			if slug, ok := slugOfKind(other, token); ok {
				return "", Fail(slugFailure(kind),
					"that link names "+article(slugNoun(other))+" — `"+slug+"` — and this takes "+
						article(slugNoun(kind))+"; pass the "+slugNoun(kind)+
						" slug, or a link that carries one")
			}
		}
	}

	return "", Fail(slugFailure(kind),
		"`"+trimmed+"` carries no "+slugNoun(kind)+" slug — paste "+slugExample(kind)+
			", or the "+slugNoun(kind)+" slug itself")
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
		return "the link krowk handed back, like https://krowk.com/a/art_…"
	}
	return "a link krowk printed that carries the " + slugNoun(kind) + " slug"
}

// looksLikeLink is what separates a slug from a URL that holds one. A slug is
// one word: no scheme, no path, no host. So anything carrying a separator is
// treated as a link, and everything else is left exactly as it was typed.
func looksLikeLink(s string) bool {
	return strings.ContainsAny(s, "/:") || strings.Contains(s, ".")
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

// slugOfKind reads one token as a slug of the given kind, in either spelling:
// `art_…` as the registry mints it, and `art-…` as it appears in a DNS label
// where an underscore is not legal.
func slugOfKind(kind, token string) (string, bool) {
	for _, separator := range []string{"_", "-"} {
		rest, ok := strings.CutPrefix(token, kind+separator)
		if ok && len(rest) >= slugFloor && isBase36(rest) {
			// Handed back in the underscore spelling, because that is the identity
			// every endpoint is addressed by; the hyphen is only how DNS spells it.
			return kind + "_" + rest, true
		}
	}
	return "", false
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
