package store

import (
	"regexp"
	"sort"
	"sync"
	"testing"
	"time"
)

// The shape every id has to have, spelled out independently of the code that
// mints it: 8-4-4-4-12 lowercase hex, version 7, RFC 9562 variant.
var idShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// frozen returns a clock stuck at t, and a knob to move it.
func frozen(t time.Time) (Clock, func(time.Time)) {
	var mu sync.Mutex
	now := t
	return func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}, func(next time.Time) {
			mu.Lock()
			defer mu.Unlock()
			now = next
		}
}

func TestNewIDShape(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clock Clock
	}{
		{"frozen clock", func() time.Time { return time.UnixMilli(1_757_000_000_000) }},
		{"real clock", time.Now},
		{"epoch", func() time.Time { return time.UnixMilli(0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMinter(tc.clock)
			for i := 0; i < 100; i++ {
				id := m.NewID()
				if !idShape.MatchString(id) {
					t.Fatalf("id %q does not match %v", id, idShape)
				}
			}
		})
	}

	if id := NewID(); !idShape.MatchString(id) {
		t.Fatalf("package-level NewID gave %q, which does not match %v", id, idShape)
	}
}

// assertUniqueAndOrdered is the property every batch of ids has to hold: no
// repeats, and string order equals issuance order.
func assertUniqueAndOrdered(t *testing.T, ids []string) {
	t.Helper()
	seen := make(map[string]int, len(ids))
	for i, id := range ids {
		if !idShape.MatchString(id) {
			t.Fatalf("id %d %q does not match %v", i, id, idShape)
		}
		if prev, dup := seen[id]; dup {
			t.Fatalf("id %q issued twice, at %d and %d", id, prev, i)
		}
		seen[id] = i
		if i > 0 && ids[i-1] >= id {
			t.Fatalf("ids out of order: %d %q is not after %d %q", i, id, i-1, ids[i-1])
		}
	}
}

func TestNewIDSortsInIssuanceOrder(t *testing.T) {
	// 1000 within one frozen millisecond fits in the counter's headroom, so
	// this exercises the ordinary same-millisecond path.
	clock, _ := frozen(time.UnixMilli(1_757_000_000_000))
	m := NewMinter(clock)

	ids := make([]string, 1000)
	for i := range ids {
		ids[i] = m.NewID()
	}
	assertUniqueAndOrdered(t, ids)
}

func TestNewIDCounterOverflow(t *testing.T) {
	// More than 4096 in one millisecond has to exhaust rand_a at least once.
	// The timestamp moves instead, and the ordering survives.
	at := time.UnixMilli(1_757_000_000_000)
	clock, _ := frozen(at)
	m := NewMinter(clock)

	ids := make([]string, 10_000)
	for i := range ids {
		ids[i] = m.NewID()
	}
	assertUniqueAndOrdered(t, ids)

	first, err := IDTime(ids[0])
	if err != nil {
		t.Fatalf("IDTime(%q): %v", ids[0], err)
	}
	last, err := IDTime(ids[len(ids)-1])
	if err != nil {
		t.Fatalf("IDTime(%q): %v", ids[len(ids)-1], err)
	}
	if !last.After(first) {
		t.Fatalf("10000 ids in one millisecond should have advanced the timestamp past %v, got %v", first, last)
	}
	if first.UnixMilli() != at.UnixMilli() {
		t.Fatalf("first id should carry the clock's millisecond %d, got %d", at.UnixMilli(), first.UnixMilli())
	}
}

func TestNewIDClockStepsBackwards(t *testing.T) {
	at := time.UnixMilli(1_757_000_000_000)
	clock, set := frozen(at)
	m := NewMinter(clock)

	before := m.NewID()
	set(at.Add(-5 * time.Second))
	after := m.NewID()

	if after <= before {
		t.Fatalf("id minted after the clock stepped back sorts before its predecessor: %q then %q", before, after)
	}
	bt, err := IDTime(before)
	if err != nil {
		t.Fatalf("IDTime(%q): %v", before, err)
	}
	at2, err := IDTime(after)
	if err != nil {
		t.Fatalf("IDTime(%q): %v", after, err)
	}
	if at2.Before(bt) {
		t.Fatalf("timestamp went backwards: %v then %v", bt, at2)
	}
}

func TestNewIDRealClock(t *testing.T) {
	m := NewMinter(time.Now)
	ids := make([]string, 1000)
	for i := range ids {
		ids[i] = m.NewID()
	}
	assertUniqueAndOrdered(t, ids)
}

func TestNewIDConcurrent(t *testing.T) {
	const goroutines, each = 8, 500
	m := NewMinter(time.Now)

	var wg sync.WaitGroup
	out := make([][]string, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ids := make([]string, each)
			for i := range ids {
				ids[i] = m.NewID()
			}
			out[g] = ids
		}(g)
	}
	wg.Wait()

	// Order across goroutines is whatever the scheduler decided, so only
	// uniqueness is asserted globally — sorting first lets the same check run.
	all := make([]string, 0, goroutines*each)
	for _, ids := range out {
		all = append(all, ids...)
	}
	sort.Strings(all)
	assertUniqueAndOrdered(t, all)
	if len(all) != goroutines*each {
		t.Fatalf("expected %d ids, got %d", goroutines*each, len(all))
	}
}

func TestParseID(t *testing.T) {
	minted := NewMinter(func() time.Time { return time.UnixMilli(1_757_000_000_000) }).NewID()

	for _, tc := range []struct {
		name string
		in   string
		ok   bool
	}{
		{"minted here", minted, true},
		{"lowest variant", "01994b5f-2a00-7000-8000-000000000000", true},
		{"highest variant", "ffffffff-ffff-7fff-bfff-ffffffffffff", true},
		{"claude line uuid v4", "9a490369-b6d3-4df0-a2cc-5fe6bb82353e", false},
		{"opencode session id", "ses_fdc11bee3ffe0000abcdefghijklmn", false},
		{"registry slug", "art_k3f9q0zt7m2x8bw4nr6vhc1s", false},
		{"uppercase", "01994B5F-2A00-7000-8000-000000000000", false},
		{"hyphenless", "01994b5f2a0070008000000000000000", false},
		{"empty", "", false},
		{"braced", "{01994b5f-2a00-7000-8000-000000000000}", false},
		{"urn form", "urn:uuid:01994b5f-2a00-7000-8000-000000000000", false},
		{"nil uuid", "00000000-0000-0000-0000-000000000000", false},
		{"bad variant", "01994b5f-2a00-7000-c000-000000000000", false},
		{"hyphen in the wrong place", "01994b5f2-a00-7000-8000-00000000000", false},
		{"non-hex digit", "01994b5g-2a00-7000-8000-000000000000", false},
		{"one short", "1994b5f-2a00-7000-8000-000000000000", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseID(tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("ParseID(%q) refused a valid id: %v", tc.in, err)
				}
				if got != tc.in {
					t.Fatalf("ParseID(%q) returned %q", tc.in, got)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseID(%q) accepted %q", tc.in, got)
			}
		})
	}
}

func TestNowMS(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
		want int64
	}{
		{"exact millisecond", time.UnixMilli(1_757_000_000_000), 1_757_000_000_000},
		{"sub-millisecond is truncated", time.Unix(1_757_000_000, 999_999), 1_757_000_000_000},
		{"epoch", time.UnixMilli(0), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMinter(func() time.Time { return tc.at })
			if got := m.NowMS(); got != tc.want {
				t.Fatalf("NowMS() = %d, want %d", got, tc.want)
			}
		})
	}

	if got := NowMS(); got < 1_757_000_000_000 {
		t.Fatalf("package-level NowMS() = %d, which is before this code was written", got)
	}
}

func TestIDTimeRoundTrip(t *testing.T) {
	at := time.UnixMilli(1_757_000_000_000)
	m := NewMinter(func() time.Time { return at })

	got, err := IDTime(m.NewID())
	if err != nil {
		t.Fatalf("IDTime: %v", err)
	}
	if got.UnixMilli() != at.UnixMilli() {
		t.Fatalf("IDTime round-tripped to %d, want %d", got.UnixMilli(), at.UnixMilli())
	}
	if got.Location() != time.UTC {
		t.Fatalf("IDTime returned %v, want a UTC time", got.Location())
	}

	if _, err := IDTime("art_k3f9q0zt7m2x8bw4nr6vhc1s"); err == nil {
		t.Fatal("IDTime accepted a registry slug")
	}
}
