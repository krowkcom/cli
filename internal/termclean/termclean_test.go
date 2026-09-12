package termclean

import (
	"strings"
	"testing"
)

func TestCell(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain untouched", "hello world", "hello world"},
		{"ansi stripped", "\x1b[31mred\x1b[0m", "[31mred[0m"},
		{"esc dropped", "a\x1bb", "ab"},
		{"bidi override dropped", "a\u202eb", "ab"},
		{"bidi isolate dropped", "a\u2066b\u2069c", "abc"},
		{"zwj preserved", "a\u200db", "a\u200db"},
		{"bom stripped", "\ufeffhi", "hi"},
		{"c0 stripped", "a\x01\x7fb", "ab"},
		{"whitespace folded", "a  b\n\tc", "a b c"},
		{"del is control", "a\x7fb", "ab"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Cell(c.in); got != c.want {
				t.Errorf("Cell(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	if got := Cell("normal text 123"); strings.Contains(got, "\x1b") {
		t.Errorf("plain text gained an escape: %q", got)
	}
}
