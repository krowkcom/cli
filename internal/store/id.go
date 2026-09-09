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
func NewMinter(clock Clock) *Minter {
	if clock == nil {
		clock = time.Now
	}
	return &Minter{clock: clock}
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
	// top two bytes dropped rather than assembled a byte at a time.
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(ms))
	copy(b[0:6], ts[2:8])

	// crypto/rand.Read is documented since Go 1.24 never to return an error —
	// it either fills the buffer or the program cannot continue — so there is
	// no error path to handle here. That matters: a CLI that must not fail for
	// a boring reason has no business panicking to name a row.
	_, _ = rand.Read(b[6:])

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
// The timestamp it returns is not always the clock's: it never goes backwards,
// because an id that sorts before one already issued would break the ordering
// the whole scheme is for. So a clock that steps back — an NTP correction, a
// suspended laptop — and a millisecond whose 4096 counter values are used up
// are handled the same way, by taking the millisecond after the last one used.
func (m *Minter) next() (int64, uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch now := m.clock().UnixMilli(); {
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
	_, _ = rand.Read(b[:]) // never errors; see NewID
	return binary.BigEndian.Uint16(b[:]) & 0x7ff
}

// ParseID returns s unchanged when it is an id this package would mint, and
// otherwise says what shape was expected. It is strict about all three of case,
// version and variant, because the ids it has to refuse are the ones that look
// closest: a v4 uuid from a Claude transcript, an uppercase copy of one of
// ours, an opencode `ses_…` or a registry slug `art_…` in a `foreign_id`
// column. Anything that gets through here can go into a `uuid` column as text.
func ParseID(s string) (string, error) {
	if len(s) != idLen {
		return "", fmt.Errorf("%q is not a krowk id: expected %d characters of lowercase hyphenated uuidv7, got %d", s, idLen, len(s))
	}
	for i := 0; i < idLen; i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return "", fmt.Errorf("%q is not a krowk id: expected a hyphen at position %d, as in 8-4-4-4-12", s, i)
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", fmt.Errorf("%q is not a krowk id: expected lowercase hex at position %d, got %q", s, i, string(c))
		}
	}
	if s[14] != '7' {
		return "", fmt.Errorf("%q is not a krowk id: expected uuid version 7, got version %q", s, string(s[14]))
	}
	switch s[19] {
	case '8', '9', 'a', 'b':
	default:
		return "", fmt.Errorf("%q is not a krowk id: expected the RFC 9562 variant, one of 8, 9, a or b, got %q", s, string(s[19]))
	}
	return s, nil
}

// IDTime returns the millisecond the id was minted for. Useful to a test and to
// `krowk doctor`: it means a row's age is readable from its primary key without
// a time column being trusted.
func IDTime(id string) (time.Time, error) {
	if _, err := ParseID(id); err != nil {
		return time.Time{}, err
	}
	hex := strings.ReplaceAll(id[:18], "-", "")[:12]
	var ms int64
	for i := 0; i < len(hex); i++ {
		ms = ms<<4 | int64(unhex(hex[i]))
	}
	return time.UnixMilli(ms).UTC(), nil
}

// unhex is only ever reached through ParseID, so the byte is known to be a
// lowercase hex digit.
func unhex(c byte) byte {
	if c <= '9' {
		return c - '0'
	}
	return c - 'a' + 10
}
