package output

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer the spinner's goroutine and the test can both
// touch. Without it the race detector reports the spinner writing while the
// assertion reads, which is a fact about the test and not about the spinner.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The spinner is a thing being done, not a thing that happened, so nothing it
// wrote may outlive it. Whatever is printed next — the link, or the failure —
// has to start on a clean line, and a transcript of the session has to read as
// though the wait never occurred.
func TestTheSpinnerLeavesTheLineAsItFoundIt(t *testing.T) {
	var w syncBuffer
	s := Spin(&w, true, "Uploading shot.png")
	waitFor(t, &w, "Uploading shot.png")
	s.Stop()

	out := w.String()
	if !strings.HasSuffix(out, eraseLine) {
		t.Errorf("the spinner did not erase its line before stopping: %q", out)
	}
	// It never advanced the line either, so it cannot have pushed anything up
	// the scrollback on its way.
	if strings.Contains(out, "\n") {
		t.Errorf("the spinner wrote a newline, so its frames are in the scrollback: %q", out)
	}

	// And it is done with the writer by the time Stop returns, so the caller can
	// print to the same stream without racing a frame onto the front of it.
	before := len(out)
	time.Sleep(4 * spinnerInterval)
	if after := len(w.String()); after != before {
		t.Errorf("the spinner wrote %d more bytes after Stop returned", after-before)
	}
}

// Agents pipe krowk, and a spinner on a piped stream is escape codes in
// somebody's file. The caller decides by passing show, and a spinner that was
// never shown still has to behave like one so the call site stays three
// unconditional lines.
func TestASpinnerNobodyCanSeeWritesNothing(t *testing.T) {
	var w syncBuffer
	s := Spin(&w, false, "Uploading shot.png")
	s.Say("Uploading other.png")
	s.Stop()
	s.Stop()

	if s != nil {
		t.Error("Spin returned a spinner for output nobody is watching")
	}
	if out := w.String(); out != "" {
		t.Errorf("a spinner that was not shown wrote %q", out)
	}
}

// A push of several files is one line naming each in turn, rather than a line
// per file blinking in and out.
func TestTheSpinnerFollowsWhichFileIsMoving(t *testing.T) {
	var w syncBuffer
	s := Spin(&w, true, "Uploading first.png (1/2)")
	defer s.Stop()

	waitFor(t, &w, "Uploading first.png (1/2)")
	s.Say("Uploading second.png (2/2)")
	waitFor(t, &w, "Uploading second.png (2/2)")

	// Still one line: the first name was erased rather than scrolled past.
	if strings.Contains(w.String(), "\n") {
		t.Error("the spinner started a second line for the second file")
	}
}

func waitFor(t *testing.T, w *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(w.String(), want) {
			return
		}
		time.Sleep(spinnerInterval / 4)
	}
	t.Fatalf("the spinner never said %q; it said %q", want, w.String())
}
