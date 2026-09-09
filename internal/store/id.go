package store

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Clock is where this package reads the current time, injected rather than
// called directly so a test can freeze it. Production passes time.Now.
type Clock func() time.Time

// Minter issues ids and reads the clock. It is the pair of them on purpose: an
// id carries a millisecond timestamp, a row carries millisecond time columns,
// and if those two came from different clocks a frozen-clock test would see
// them disagree.
//
// The counter is the monotonic-random half of RFC 9562 §6.2 method 3. Two ids
// minted in the same millisecond differ only in random bits, which says nothing
// about which came first; a counter in rand_a makes the string order the
// issuance order, and that is what makes an id usable as a sort key.
type Minter struct {
	clock Clock

	mu      sync.Mutex
	lastMS  int64  // the highest millisecond any id has been issued for
	counter uint16 // 12 bits of rand_a, monotonic within lastMS
}

// NewMinter returns a Minter reading the given clock. A nil clock means
// time.Now, so a zero-value-ish call site still gets a working minter.
//
// lastMS starts at -1 rather than 0 because 0 is a millisecond a clock can
// actually read: a test frozen at the Unix epoch would otherwise look like a
// second id inside a millisecond already used, take the increment path, and
// count up from 0 identically in every process.
func NewMinter(clock Clock) *Minter {
	if clock == nil {
		clock = time.Now
	}
	return &Minter{clock: clock, lastMS: -1}
}

// defaultMinter serves the package-level NewID and NowMS, which is what
// everything outside a test uses.
var defaultMinter = NewMinter(time.Now)

// NewID mints an id from the default minter.
func NewID() string { return defaultMinter.NewID() }

// NowMS reads the default minter's clock.
func NowMS() int64 { return defaultMinter.NowMS() }

// idLen is the canonical hyphenated form: 32 hex digits and 4 hyphens.
const idLen = 36

// counterMax is the largest value rand_a can hold; past it the millisecond is
// full and the timestamp has to move.
const counterMax = 0xfff

// NewID returns a canonical lowercase UUIDv7: 48 bits of big-endian Unix
// milliseconds, version 7, 12 bits of monotonic counter, variant 10, and 62
// random bits.
func (m *Minter) NewID() string {
	ms, counter := m.next()

	var b [16]byte
	// The timestamp is 48 bits, so it goes in as a big-endian uint64 with the
	// top two bytes dropped rather than assembled a byte at a time. next()
	// guarantees ms is non-negative, so the conversion cannot wrap.
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(ms))
	copy(b[0:6], ts[2:8])

	// Only bytes 8 onwards are random: 6 and 7 are the version nibble and the
	// counter, written below, and byte 8 keeps its low 6 bits.
	//
	// There is no error to receive here. crypto/rand.Read is documented never
	// to return one — if the operating system's random source is unreadable the
	// runtime aborts the program irrecoverably inside the call, so no caller of
	// this function ever observes the failure and there is no path to write for
	// it.
	_, _ = rand.Read(b[8:])

	b[6] = 0x70 | byte(counter>>8) // version 7, then the top 4 bits of rand_a
	b[7] = byte(counter)           // the low 8 bits of rand_a
	b[8] = (b[8] & 0x3f) | 0x80    // variant 10, leaving 62 random bits of rand_b

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NowMS is the clock this store writes to every time column: milliseconds since
// the Unix epoch, UTC, truncated rather than rounded.
func (m *Minter) NowMS() int64 {
	return m.clock().UnixMilli()
}

// next reserves the (millisecond, counter) pair for one id.
//
// The timestamp it returns is not always the clock's, and the invariant is that
// it never decreases and is never negative. An id that sorted before one
// already issued would break the ordering the whole scheme is for, so a clock
// that steps back — an NTP correction, a suspended laptop — and a millisecond
// whose 4096 counter values are used up are handled the same way, by taking the
// millisecond after the last one used. The cost is that the timestamp in an id
// can run ahead of the clock; it is a sort key, not a measurement.
func (m *Minter) next() (int64, uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock().UnixMilli()
	// A pre-epoch clock is nobody's real clock, but a negative millisecond
	// would wrap when it was truncated into the 48-bit field and land far in
	// the future, so it is floored instead of encoded.
	if now < 0 {
		now = 0
	}

	switch {
	case now > m.lastMS:
		m.lastMS = now
		m.counter = seedCounter()
	case now == m.lastMS && m.counter < counterMax:
		m.counter++
	default:
		// Either the clock read earlier than an id already issued, or this
		// millisecond is full. Both mean: move on by one and start again.
		m.lastMS++
		m.counter = seedCounter()
	}
	return m.lastMS, m.counter
}

// seedCounter starts a millisecond's counter at a random value with the top bit
// clear, as RFC 9562 §6.2 suggests: random so two processes minting in the same
// millisecond do not produce the same id, and bounded so there are at least
// 2048 increments of headroom before the millisecond fills.
func seedCounter() uint16 {
	var b [2]byte
	_, _ = rand.Read(b[:]) // no error to receive; see NewID
	return binary.BigEndian.Uint16(b[:]) & 0x7ff
}

// ValidateID reports whether s is an id this package would mint, and otherwise
// says what shape was expected. It is the store's boundary check, and it is
// strict about all three of case, version and variant, because the ids it has
// to refuse are the ones that look closest: a v4 uuid from a Claude transcript,
// an uppercase copy of one of ours, an opencode `ses_…` or a registry slug
// `art_…` on their way to a `foreign_id` column. Anything that gets through
// here can go into a `uuid` column as text.
func ValidateID(s string) error {
	if len(s) != idLen {
		return fmt.Errorf("%q is not a krowk id: expected %d characters of lowercase hyphenated uuidv7, got %d", s, idLen, len(s))
	}
	for i := 0; i < idLen; i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return fmt.Errorf("%q is not a krowk id: expected a hyphen at position %d, as in 8-4-4-4-12", s, i)
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return fmt.Errorf("%q is not a krowk id: expected lowercase hex at position %d, got %q", s, i, string(c))
		}
	}
	if s[14] != '7' {
		return fmt.Errorf("%q is not a krowk id: expected uuid version 7, got version %q", s, string(s[14]))
	}
	switch s[19] {
	case '8', '9', 'a', 'b':
	default:
		return fmt.Errorf("%q is not a krowk id: expected the RFC 9562 variant, one of 8, 9, a or b, got %q", s, string(s[19]))
	}
	return nil
}

// IDTime returns the millisecond an id was issued for. That is a monotonic
// stamp, not a reading of the clock at the moment of issue — after a counter
// overflow or a backwards clock step it runs ahead — so it answers "which of
// these came first", not "when exactly did this happen". Its callers today are
// this package's own tests, which use it to assert exactly that ordering.
func IDTime(id string) (time.Time, error) {
	if err := ValidateID(id); err != nil {
		return time.Time{}, err
	}
	hex := strings.ReplaceAll(id[:18], "-", "")[:12]
	var ms int64
	for i := 0; i < len(hex); i++ {
		ms = ms<<4 | int64(unhex(hex[i]))
	}
	return time.UnixMilli(ms).UTC(), nil
}

// unhex is only ever reached through ValidateID, so the byte is known to be a
// lowercase hex digit.
func unhex(c byte) byte {
	if c <= '9' {
		return c - '0'
	}
	return c - 'a' + 10
}
