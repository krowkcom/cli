// Package harness knows which coding agents are installed on this machine and
// whether krowk is actually wired into them. An agent that has krowk's MCP
// server registered and its skill on disk can use krowk without being told how;
// one that has neither will improvise, badly. The point of this package is to
// be able to say which of those two a checkout is in, in one vocabulary, so
// that `krowk setup` verifying its own work and `krowk doctor` reporting on
// somebody else's ask exactly the same questions and get exactly the same
// answers.
//
// Nothing here reads the process environment. Every check takes the lookup it
// needs — an Env, and a working directory where the answer depends on one — so
// a test can hand it a temporary home instead of the developer's real one, and
// so a check is a function of its inputs rather than of the machine it ran on.
package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Env is a lookup function, the same shape internal/runctx uses, so tests do
// not have to touch the process environment to move a home directory.
type Env = func(string) string

// Status values, ordered by how much they should worry the person reading.
const (
	StatusPass = "pass"
	StatusWarn = "warn"
	StatusFail = "fail"
)

// StatusCheck is one health question and its answer. Message says what is,
// Hint says what to do about it — so it is present only when there is
// something to do, which is why it is omitempty rather than empty.
type StatusCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// Pass reports a check that found what it was looking for. A passing check
// carries no hint: there is nothing left to suggest.
func Pass(name, message string) StatusCheck {
	return StatusCheck{Name: name, Status: StatusPass, Message: message}
}

// Warn reports a check that could not reach an answer — the file it needed was
// unreadable, not absent. It is not a failure, because nothing was disproved.
func Warn(name, message, hint string) StatusCheck {
	return StatusCheck{Name: name, Status: StatusWarn, Message: message, Hint: hint}
}

// Fail reports a check that reached an answer and the answer is no.
func Fail(name, message, hint string) StatusCheck {
	return StatusCheck{Name: name, Status: StatusFail, Message: message, Hint: hint}
}

// Worst is the most severe status in checks: fail beats warn beats pass. An
// empty set passes — no question asked is no bad news. Every consumer needs
// this single-word summary, so it lives here rather than being reinvented.
func Worst(checks []StatusCheck) string {
	worst := StatusPass
	for _, c := range checks {
		switch c.Status {
		case StatusFail:
			return StatusFail
		case StatusWarn:
			worst = StatusWarn
		}
	}
	return worst
}

// HomeDir is the home directory as env describes it, or "" when env describes
// none. Windows spells it USERPROFILE and only falls back there, so a Unix
// test that deliberately empties HOME is not rescued by a stray variable.
func HomeDir(env Env) string {
	if env == nil {
		return ""
	}
	if home := env("HOME"); home != "" {
		return filepath.Clean(home)
	}
	if runtime.GOOS == "windows" {
		if home := env("USERPROFILE"); home != "" {
			return filepath.Clean(home)
		}
	}
	return ""
}

// LookPath finds name in the PATH env describes, without consulting the real
// one. Only executable regular files count on Unix; on Windows, where the
// execute bit does not exist, presence is the whole test.
func LookPath(env Env, name string) string {
	if env == nil {
		return ""
	}
	for _, dir := range filepath.SplitList(env("PATH")) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// isRegularFile reports whether path's final component is a regular file.
// Lstat, so a symlink in the name of the thing is not the thing: its target
// was never inspected, and a check must not vouch for what it did not read.
func isRegularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// isDir reports whether path is a directory, resolving links.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// commandIsKrowkMCP reports whether an MCP server's command line launches
// krowk's MCP server. The base name is what identifies it: an absolute path
// out of a version manager, a bare name off PATH and a Windows .exe are all
// the same server.
func commandIsKrowkMCP(command string) bool {
	base := filepath.Base(strings.TrimSpace(command))
	base = strings.TrimSuffix(base, ".exe")
	return base == "krowk-mcp"
}
