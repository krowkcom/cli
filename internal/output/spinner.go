package output

import (
	"io"
	"sync"
	"time"
)

// Spinner is the line a person watches while bytes move. An upload is the one
// thing krowk does that takes long enough to look hung, and a terminal that has
// printed nothing for four seconds is indistinguishable from one that has
// stopped.
//
// It is deliberately not progress: krowk hands the file to object storage in one
// request and is not told how much of it has landed, so a percentage would be a
// number krowk invented. What it can honestly say is that it is still working
// and on which file.
//
// Everything about it is transient. It writes to stderr, so the link on stdout
// stays the only thing a pipe sees, and it erases itself before the durable line
// is printed, so nothing about it survives into scrollback or into a file. The
// success or failure line that follows is the whole record of what happened.
type Spinner struct {
	// A nil Spinner is one that was never started — piped output, JSON, a
	// terminal that would only collect escape codes. Every method tolerates it,
	// so the caller writes the same three lines whether or not anybody is
	// watching.
	w    io.Writer
	stop chan struct{}
	done chan struct{}

	// once guards the close rather than a flag under mu: Stop waits for the
	// goroutine, and the goroutine takes mu to read the message, so holding mu
	// across that wait would deadlock the two against each other.
	once sync.Once

	mu  sync.Mutex
	say string
}

// frames are the braille cycle, which reads as motion at this speed and is one
// cell wide in every font that has it, so the line does not jitter.
var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	spinnerInterval = 80 * time.Millisecond
	// eraseLine returns to the start of the line and clears to its end. The
	// spinner never prints a newline, so this is always the line it wrote.
	eraseLine = "\r\x1b[K"
)

// Spin starts a spinner saying what is happening, and returns nil when it should
// not be shown at all — which is the common case, since agents pipe krowk and
// piped output must stay byte-for-byte what it always was.
func Spin(w io.Writer, show bool, say string) *Spinner {
	if !show || w == nil {
		return nil
	}
	s := &Spinner{w: w, say: say, stop: make(chan struct{}), done: make(chan struct{})}
	go s.run()
	return s
}

// Say changes what the line reads without restarting it, so a push of several
// files is one spinner naming each in turn rather than a line per file blinking
// in and out.
func (s *Spinner) Say(say string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.say = say
}

func (s *Spinner) run() {
	defer close(s.done)
	ticker := time.NewTicker(spinnerInterval)
	defer ticker.Stop()

	for i := 0; ; i++ {
		s.mu.Lock()
		say := s.say
		s.mu.Unlock()
		io.WriteString(s.w, eraseLine+paint(true, dim, frames[i%len(frames)]+" "+say))
		select {
		case <-s.stop:
			io.WriteString(s.w, eraseLine)
			return
		case <-ticker.C:
		}
	}
}

// Stop erases the line and waits for the goroutine to be done with the writer,
// so the caller can print to the same stream immediately afterwards without
// racing a frame onto the front of it. Calling it twice, or on a nil Spinner, is
// fine: the caller defers it and also calls it before printing.
func (s *Spinner) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
		<-s.done
	})
}
