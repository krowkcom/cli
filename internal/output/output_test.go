package output

import (
	"testing"
	"time"

	"github.com/krowkcom/cli/internal/api"
)

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{27, "27 B"},
		{412 * 1024, "412 KB"},
		{5933, "5.8 KB"},
		{214958080, "205 MB"},
	} {
		if got := HumanBytes(tc.in); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRelativeExpiry(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want string
	}{
		{now.Add(47 * time.Hour).Format(time.RFC3339), "expires in 47h"},
		{now.Add(72 * time.Hour).Format(time.RFC3339), "expires in 3d"},
		{now.Add(-time.Second).Format(time.RFC3339), "expired"},
		{"", ""},
		{"not a timestamp", ""},
	} {
		if got := RelativeExpiry(tc.in, now); got != tc.want {
			t.Errorf("RelativeExpiry(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveFormat(t *testing.T) {
	// Piped output defaults to JSON so an agent gets structure for free.
	if got, _ := ResolveFormat("", false, false); got != JSON {
		t.Errorf("piped default = %q, want json", got)
	}
	if got, _ := ResolveFormat("", false, true); got != Human {
		t.Errorf("tty default = %q, want human", got)
	}
	if got, _ := ResolveFormat("markdown", false, true); got != Markdown {
		t.Errorf("explicit markdown = %q", got)
	}
	if _, err := ResolveFormat("yaml", false, true); err == nil {
		t.Error("--format yaml should be rejected")
	}
}

func TestErrorRendersLimitAndFix(t *testing.T) {
	err := &api.Error{Status: 413, Body: map[string]any{
		"error":       "artifact_too_large",
		"limit_bytes": 104857600,
		"got_bytes":   214958080,
		"fix":         "re-encode below 100 MB or push frames separately",
		"retryable":   false,
	}}

	got := Error(err, Human, false, false)
	want := "✗ artifact_too_large  (HTTP 413)\n" +
		"  got_bytes: 214958080\n" +
		"  limit_bytes: 104857600\n" +
		"  fix: re-encode below 100 MB or push frames separately"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
