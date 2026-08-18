package api

import "strings"

// Slug kinds, spelled as the registry prefixes them. A kind is what a command
// is asking for: `uploads show` wants an artifact and `runs show` wants a run,
// and a link that carries the wrong one is worth saying so about rather than
// sending on to be answered as a 404.
const (
	KindArtifact = "art"
	KindRun      = "run"
)

// slugNouns names each kind the way the help and the errors do, so a message
// about a run does not say "run kind" and one about an artifact does not say
// "art".
var slugNouns = map[string]string{
	KindArtifact: "artifact",
	KindRun:      "run",
}

// slugLength is how much random tail a token needs before a link is read as
// carrying a slug. Canon mints exactly 24 base36 characters after the prefix,
// and the website refuses anything else before it ever calls the registry, so
// nothing krowk made is turned away by this — and everything a URL is full of
// is: `run_id=4821` in a CI link, `art_report.png` beside a checkout, `run_7`
// in a job path. None of those is a slug, and `uploads delete` is immediate and
// has no undo.
//
// A floor rather than a fixed length, so a registry that one day mints a longer
// slug still has its links read. A shorter one would stop being read out of
// links and go on working when passed bare, which is the path this function
// leaves untouched.
const slugLength = 24

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
		// read as absent would quietly widen `--run " "` from one run to a new one
		// and `uploads list --run " "` to the whole workspace.
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

	// Lowercased because a slug is lowercase base36 and a pasted URL need not be.
	tokens := strings.FieldsFunc(strings.ToLower(trimmed), isSlugBoundary)

	found := slugsIn(kind, tokens)
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		// Naming the kind that *is* there turns a dead end into the next command:
		// a card link handed to `runs show` is the artifact's link, and the run it
		// belongs to is one `uploads show` away.
		for _, other := range []string{KindArtifact, KindRun} {
			if other == kind {
				continue
			}
			if elsewhere := slugsIn(other, tokens); len(elsewhere) > 0 {
				return "", Fail(slugFailure(kind),
					"that link names "+article(slugNoun(other))+" — `"+elsewhere[0]+"` — and this takes "+
						article(slugNoun(kind))+"; pass the "+slugNoun(kind)+
						" slug, or a link that carries one")
			}
		}
		// What was pasted is not quoted back. A URL is where credentials travel —
		// a presigned link carries its signature in the query — and the caller has
		// the string in front of them either way, where stderr and the JSON
		// envelope are what CI keeps.
		return "", Fail(slugFailure(kind),
			"that carries no "+slugNoun(kind)+" slug — pass "+slugExample(kind))
	default:
		// Two different records in one string and no way to know which was meant.
		// `uploads delete` is immediate and has no undo, so guessing here is how a
		// paste of two links takes down something nobody named.
		return "", Fail(slugFailure(kind),
			"that carries more than one "+slugNoun(kind)+" — `"+strings.Join(found, "`, `")+
				"` — so which one is meant is a guess; pass the one you want")
	}
}

// slugsIn collects the distinct slugs of one kind, in the order they appear.
// Distinct, because the same slug twice is not an ambiguity: the markdown krowk
// hands back names one artifact in both halves of `[![name](file)](card)`, and
// pasting that whole line is a reasonable thing to do.
func slugsIn(kind string, tokens []string) []string {
	var found []string
	for _, token := range tokens {
		rest, ok := strings.CutPrefix(token, kind+"_")
		if !ok || len(rest) < slugLength || !isBase36(rest) {
			continue
		}
		if slug := kind + "_" + rest; !contains(found, slug) {
			found = append(found, slug)
		}
	}
	return found
}

func contains(haystack []string, needle string) bool {
	for _, straw := range haystack {
		if straw == needle {
			return true
		}
	}
	return false
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
// public page to point at — `krowk.com/a/{slug}` is the card — so a run is told
// to pass the slug krowk printed rather than a URL shape that does not exist.
func slugExample(kind string) string {
	if kind == KindArtifact {
		return "the artifact slug, like art_…, or the link krowk handed back, " +
			"like https://krowk.com/a/art_…"
	}
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
// segments, a query and a fragment all break on one of these. `_` does not,
// because that is how a slug is spelled.
func isSlugBoundary(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return false
	case r == '_':
		return false
	}
	return true
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
