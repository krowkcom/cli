package store

import (
	"fmt"
	"regexp"
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

	// Order across goroutines is whatever the scheduler decided, so ordering is
	// only asserted inside each goroutine — where issuance order is known —
	// while uniqueness is asserted over the union of all of them.
	seen := make(map[string]string, goroutines*each)
	for g, ids := range out {
		assertUniqueAndOrdered(t, ids)
		for i, id := range ids {
			where := fmt.Sprintf("goroutine %d index %d", g, i)
			if prev, dup := seen[id]; dup {
				t.Fatalf("id %q issued twice, at %s and %s", id, prev, where)
			}
			seen[id] = where
		}
	}
	if len(seen) != goroutines*each {
		t.Fatalf("expected %d distinct ids, got %d", goroutines*each, len(seen))
	}
}

// The counter seed is easy to leave unwired — a constant seed passes every
// ordering test in this file — so it is pinned on its own, and through the ids
// two independent minters produce at the same instant.
func TestSeedCounter(t *testing.T) {
	first := seedCounter()
	varied := false
	for i := 0; i < 10_000; i++ {
		got := seedCounter()
		if got > 0x7ff {
			t.Fatalf("seedCounter() = %#x, which does not leave the top bit of rand_a clear", got)
		}
		if got != first {
			varied = true
		}
	}
	if !varied {
		t.Fatalf("seedCounter() returned %#x on all 10001 draws, so it is not random", first)
	}
}

func TestIndependentMintersDiffer(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"same millisecond", time.UnixMilli(1_757_000_000_000)},
		{"unix epoch", time.UnixMilli(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Two processes syncing at once are two minters with the same
			// frozen clock: only the seed keeps their first ids apart.
			const minters = 64
			ids := make(map[string]bool, minters)
			randA := make(map[string]bool, minters)
			for i := 0; i < minters; i++ {
				id := NewMinter(func() time.Time { return tc.at }).NewID()
				if ids[id] {
					t.Fatalf("two independent minters at %v both issued %q", tc.at, id)
				}
				ids[id] = true
				randA[id[14:18]] = true
			}
			if len(randA) == 1 {
				t.Fatalf("all %d minters started rand_a at the same value, so the counter is not seeded", minters)
			}
		})
	}
}

func TestValidateID(t *testing.T) {
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
			err := ValidateID(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("ValidateID(%q) refused a valid id: %v", tc.in, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ValidateID(%q) accepted it", tc.in)
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

// NewMinter(nil) is the shape the package-level NewID and NowMS use, so the
// nil-clock branch is worth a test of its own rather than only being exercised
// in production.
func TestNewMinterNilClock(t *testing.T) {
	m := NewMinter(nil)

	if id := m.NewID(); !idShape.MatchString(id) {
		t.Fatalf("NewMinter(nil) minted %q, which does not match %v", id, idShape)
	}
	if got, want := m.NowMS(), time.Now().UnixMilli(); got < want-1000 || got > want+1000 {
		t.Fatalf("NewMinter(nil).NowMS() = %d, which is not within a second of %d", got, want)
	}
}

func TestNewIDPreEpochClock(t *testing.T) {
	// A negative millisecond truncated into the 48-bit timestamp field would
	// wrap: -1 becomes all ones, which reads back as the year 10889 and sorts
	// after every id krowk will ever mint. It is floored to the epoch instead.
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"one millisecond before the epoch", time.UnixMilli(-1)},
		{"a second before the epoch", time.UnixMilli(-1000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := NewMinter(func() time.Time { return tc.at }).NewID()
			if !idShape.MatchString(id) {
				t.Fatalf("minted %q, which does not match %v", id, idShape)
			}
			got, err := IDTime(id)
			if err != nil {
				t.Fatalf("IDTime(%q): %v", id, err)
			}
			if got.UnixMilli() != 0 {
				t.Fatalf("a clock at %v produced an id stamped %d (%v), want 0", tc.at, got.UnixMilli(), got)
			}
		})
	}
}

func TestNewIDRandBVaries(t *testing.T) {
	// rand_b is the only part of an id the timestamp and the counter cannot
	// reach, so it is checked where they have no say: the variant byte's low
	// nibble at index 19 and the nibble after it. A minter whose rand_b was
	// left zero still passes every ordering test in this file.
	m := NewMinter(func() time.Time { return time.UnixMilli(1_757_000_000_000) })

	variant := map[byte]bool{}
	next := map[byte]bool{}
	for i := 0; i < 300; i++ {
		id := m.NewID()
		variant[id[19]] = true
		next[id[20]] = true
	}
	if len(variant) < 2 {
		t.Fatalf("all 300 ids had the same variant nibble, so rand_b is not random: %v", variant)
	}
	if len(next) < 2 {
		t.Fatalf("all 300 ids had the same nibble at index 20, so rand_b is not random: %v", next)
	}
}
