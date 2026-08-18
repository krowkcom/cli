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
// carrying a slug. Canon mints exactly 24 base36 characters after the prefix,
// and the website refuses anything else before it ever calls the registry, so
// nothing krowk made is turned away by this — and everything a URL is full of
// is: `run_id=4821` in a CI link, `art_report.png` beside a checkout, `run_7`
// in a job path, a 16-character token in a signed URL. None of those is a slug,
// and `uploads delete` is immediate and has no undo.
//
// A floor rather than 24 exactly, so a registry that one day mints a longer
// slug still has its links read. Shorter would stop being read out of links and
// go on working when passed bare, which is the path this function leaves
// untouched.
const slugFloor = 24

// bytesHost is where a slug is spelled as a DNS label — `art-{slug}` is a host
// under it, and an underscore is not legal in one. Nowhere else spells a slug
// that way, so nowhere else is read that way: `art-9f3c….evil.example.com` is
// somebody else's subdomain, and reading a record out of it would point a
// takedown at an artifact the pasted link never named.
const bytesHost = "krowkusercontent.com"

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

	link := readLink(trimmed)
	found := link.slugs(kind)
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
		if elsewhere := link.slugs(other); len(elsewhere) > 0 {
			return "", Fail(slugFailure(kind),
				"that link names "+article(slugNoun(other))+" — `"+elsewhere[0]+"` — and this takes "+
					article(slugNoun(kind))+"; pass the "+slugNoun(kind)+
					" slug, or a link that carries one")
		}
	}

	// What was pasted is not quoted back. A URL is where credentials travel —
	// a presigned link carries its signature in the query — and the caller has
	// the string in front of them either way, where stderr and the JSON envelope
	// are what CI keeps.
	return "", Fail(slugFailure(kind),
		"that carries no "+slugNoun(kind)+" slug — pass "+slugExample(kind))
}

// link is one pasted string, taken apart once: the host's labels and everything
// after them, each already tokenized, so a refusal that asks about three kinds
// walks the string no more times than an answer that asks about one.
type link struct {
	hostLabels []string
	rest       []string
	// dnsSpelling says the host is one that spells slugs as labels, which is the
	// only place the hyphen form is real.
	dnsSpelling bool
}

// slugs collects the distinct slugs of one kind, in the order they appear.
// Distinct, because the same slug twice is not an ambiguity: the markdown krowk
// hands back names one artifact in both halves of `[![name](file)](card)`, and
// pasting that whole line is a reasonable thing to do.
func (l link) slugs(kind string) []string {
	var found []string
	add := func(slug string, ok bool) {
		if !ok {
			return
		}
		for _, seen := range found {
			if seen == slug {
				return
			}
		}
		found = append(found, slug)
	}

	for _, label := range l.hostLabels {
		if l.dnsSpelling {
			add(slugSpelled(kind, "-", label))
		}
		add(slugSpelled(kind, "_", label))
	}
	for _, token := range l.rest {
		add(slugSpelled(kind, "_", token))
	}
	return found
}

// readLink separates the host from everything after it and tokenizes both. The
// string is lowercased and percent-decoded first: a slug is lowercase base36,
// and a link that went through an encoder on its way to being pasted carries
// the same slug — `art%5F9f3c…` is one, spelled by a machine.
func readLink(pasted string) link {
	decoded := strings.ToLower(decodePercent(pasted))

	if scheme := strings.Index(decoded, "://"); scheme >= 0 {
		decoded = decoded[scheme+3:]
	}
	// A path, query or fragment ends the host; what is before the first of them
	// is the authority, userinfo and port included — all of which break on a slug
	// boundary anyway.
	host, rest := decoded, ""
	if end := strings.IndexAny(decoded, "/?#"); end >= 0 {
		host, rest = decoded[:end], decoded[end:]
	}
	return link{
		hostLabels:  strings.FieldsFunc(host, isSlugBoundary),
		rest:        strings.FieldsFunc(rest, isSlugBoundary),
		dnsSpelling: host == bytesHost || strings.HasSuffix(host, "."+bytesHost),
	}
}

// decodePercent replaces the %XX escapes it can read and leaves everything else
// exactly as it found it.
//
// Not url.QueryUnescape: that fails the whole string on one stray `%` — a
// filename like `100%.png` in the path — and a failure there would hide a slug
// spelled correctly somewhere else in the same link. It also reads `+` as a
// space, which is the query's rule and not the path's.
func decodePercent(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if hi, ok := unhex(s[i+1]); ok {
				if lo, ok := unhex(s[i+2]); ok {
					b.WriteByte(hi<<4 | lo)
					i += 2
					continue
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
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
