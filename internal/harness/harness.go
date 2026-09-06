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
//
// Nothing here trusts a file either. A project-scoped config sits in the
// checkout, which means whoever wrote the checkout wrote it, so every read is
// bounded and every path is inspected before it is opened.
package harness

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Env is a lookup function, the same shape internal/runctx uses, so tests do
// not have to touch the process environment to move a home directory.
type Env func(string) string

// Status values, ordered by how much they should worry the person reading.
const (
	StatusPass = "pass"
	StatusWarn = "warn"
	StatusFail = "fail"
)

// maxConfigBytes bounds every config read. A megabyte is orders of magnitude
// more than any of these files legitimately holds, and the bound is what stops
// a symlink to /dev/zero in a cloned checkout from eating the machine.
const maxConfigBytes = 1 << 20

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

// HomeDir is the home directory as env describes it. HOME wins on every
// platform, including Windows, where Go's own runtime sets it and where a
// deliberately emptied HOME must not be quietly rescued; USERPROFILE is
// consulted only when HOME is empty, and only on Windows, which is the only
// place it means anything. No home in env is an empty string, never a guess.
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

// defaultPathExt is what Windows uses when PATHEXT says nothing.
const defaultPathExt = ".COM;.EXE;.BAT;.CMD"

// LookPath finds name in the PATH env describes, without consulting the real
// one. On Windows it tries name and then each PATHEXT suffix, since that is
// where the actual executable lives (`claude.cmd`, not `claude`), and presence
// is the whole test because there is no execute bit. Everywhere else the file
// must be executable.
//
// Only absolute PATH entries count. A relative entry — "." above all — makes
// the answer depend on which directory the process happens to be in, and an
// untrusted checkout carrying a file called `claude` must not be able to
// report itself as an installed agent.
func LookPath(env Env, name string) string {
	if env == nil {
		return ""
	}
	for _, dir := range filepath.SplitList(env("PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		for _, candidate := range executableNames(env, name) {
			path := filepath.Join(dir, candidate)
			if isExecutableFile(path) {
				return path
			}
		}
	}
	return ""
}

// executableNames is the file names a command could be spelled as: just the
// name on Unix, the name plus every PATHEXT suffix on Windows.
func executableNames(env Env, name string) []string {
	if runtime.GOOS != "windows" {
		return []string{name}
	}
	names := []string{name}
	exts := env("PATHEXT")
	if strings.TrimSpace(exts) == "" {
		exts = defaultPathExt
	}
	for _, ext := range strings.Split(exts, string(os.PathListSeparator)) {
		if ext = strings.TrimSpace(ext); ext != "" {
			names = append(names, name+ext)
		}
	}
	return names
}

// isExecutableFile reports whether path is a runnable file. Stat, so links
// resolve: the official Claude Code installer drops a symlink into
// ~/.local/bin, and a check that refused to follow it would call a working
// install missing. What matters here is what runs, and what runs is the
// target. (The skill check takes the opposite line, for the opposite reason —
// see CheckClaudeSkill.)
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0
}

// isRegularFile reports whether path's final component is a regular file.
// Lstat, so a symlink in the name of the thing is not the thing: its target
// was never inspected, and a check must not vouch for what it did not read.
func isRegularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// isDir reports whether path is a directory. Stat, so a symlinked config or
// skills directory counts — people do move those onto another volume, and the
// directory only has to exist, not to be trusted.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// readConfigFile reads a small JSON configuration file safely enough to read
// one an attacker wrote. It refuses anything that is not a regular file — a
// FIFO in the name of .mcp.json would otherwise block the check forever — and
// stops at maxConfigBytes rather than parsing the prefix of something huge,
// because half a config is not a config.
//
// A missing file returns fs.ErrNotExist, which callers read as "this scope
// says nothing" rather than as a problem.
func readConfigFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := os.Open(path) //nolint:gosec // G304: a path the caller named
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	// One byte past the limit is read on purpose: it is how "exactly at the
	// limit" is told apart from "truncated here".
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigBytes {
		return nil, errors.New("larger than 1 MiB")
	}
	return data, nil
}

// isNotExist reports whether err is the filesystem saying nothing is there.
func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
