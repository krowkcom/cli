// Package termclean makes caller-controlled text safe for a terminal row:
// whitespace folds to single spaces and the characters that would move the
// cursor, recolour the row, or reorder what is drawn after it are dropped.
// Dropping ESC outright also neutralises ANSI sequences (only their inert
// letters remain).
//
// One copy lives here. The CLI listing/show, the store's ambiguous-prefix
// error, and the output renderer all call it — security logic must not drift
// by copy.
package termclean

import (
	"strings"
	"unicode"
)

// Cell folds s to one terminal-safe row.
func Cell(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.TrimSpace(s) {
		switch {
		case unicode.IsSpace(r):
			space = true
		case unicode.IsControl(r), Reordering(r):
			// Dropped outright rather than folded to a space: an escape
			// sequence arrives as ESC plus ordinary letters, and spacing
			// it out would leave the letters behind as text.
		default:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Reordering reports the characters that move or hide text while occupying
// no space of their own: bidi overrides and isolates, zero-width spaces,
// and the byte order mark. U+200D ZERO WIDTH JOINER is kept: it holds
// multi-part emoji together.
//
// Spelled as code points rather than literals, since written literally they
// would be invisible here too — in the one function whose job is knowing
// they exist.
func Reordering(r rune) bool {
	switch {
	case r == '\u200d': // ZERO WIDTH JOINER
		return false
	case r == '\ufeff', // BYTE ORDER MARK
		r >= '\u200b' && r <= '\u200f', // zero-width spaces, LRM, RLM
		r >= '\u202a' && r <= '\u202e', // bidi embeddings and overrides
		r >= '\u2066' && r <= '\u2069': // bidi isolates
		return true
	}
	return false
}
