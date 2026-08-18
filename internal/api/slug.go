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

// dnsSlugFloor is how long the random half of a slug has to be before the
// hyphen spelling is read as one. A slug is 24 random characters, and the
// hyphen form only exists as a DNS label — `art-{slug}.krowkusercontent.com` —
// so a short word on the other side of a hyphen, like `run-fast` in a path, is
// not a slug and must not be mistaken for one. The underscore spelling needs no
// such floor: nothing else is spelled that way.
const dnsSlugFloor = 16

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
				return "", Fail("bad_"+slugNouns[kind],
					"that link names "+article(slugNouns[other])+" — `"+slug+"` — and this takes "+
						article(slugNouns[kind])+"; pass the "+slugNouns[kind]+" slug, or a link that carries one")
			}
		}
	}

	return "", Fail("bad_"+slugNouns[kind],
		"`"+trimmed+"` carries no "+slugNouns[kind]+" slug — paste the link krowk handed back, "+
			"like https://krowk.com/a/"+kind+"_…, or the "+slugNouns[kind]+" slug itself")
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
	if rest, ok := strings.CutPrefix(token, kind+"_"); ok && isBase36(rest) {
		return kind + "_" + rest, true
	}
	if rest, ok := strings.CutPrefix(token, kind+"-"); ok && isBase36(rest) && len(rest) >= dnsSlugFloor {
		// Handed back in the underscore spelling, because that is the identity
		// every endpoint is addressed by; the hyphen is only how DNS spells it.
		return kind + "_" + rest, true
	}
	return "", false
}

// isBase36 is the slug alphabet: lowercase letters and digits, at least one.
func isBase36(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// article keeps the error sentences readable — "an artifact", "a run".
func article(noun string) string {
	if strings.ContainsRune("aeiou", rune(noun[0])) {
		return "an " + noun
	}
	return "a " + noun
}
