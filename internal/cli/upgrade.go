package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/runctx"
)

// Where releases live. Variables rather than constants so a test can point them
// at a local server; nothing outside tests writes them. The API answers what
// the latest version is, the download host serves its bytes — GitHub happens to
// be both, but they are two different questions.
var (
	releaseAPIURL      = "https://api.github.com/repos/krowkcom/cli/releases/latest"
	releaseDownloadURL = "https://github.com/krowkcom/cli/releases/download"
)

// How long each half of an upgrade is given. The version check is one small
// JSON response and gets seconds; the download is two binaries over whatever
// network this is, and gets minutes.
const (
	latestCheckTimeout = 10 * time.Second
	downloadTimeout    = 5 * time.Minute

	// notifyCheckTimeout bounds the once-a-day background check that ordinary
	// commands pay for. Two seconds, because a command must never visibly hang
	// on news that could as well arrive tomorrow.
	notifyCheckTimeout = 2 * time.Second

	// notifyInterval is how often the latest release is asked about. Between
	// checks the answer comes from the state file, which costs one read.
	notifyInterval = 24 * time.Hour
)

// releaseVersion is what a tagged build stamps: a bare semver triple.
// Everything else — "dev", a git describe like 0.3.0-5-gd88d552, a -dirty
// suffix — is a checkout, and a checkout upgrades with git, not with a
// downloaded archive over whatever is being worked on.
var releaseVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func isReleaseVersion(v string) bool { return releaseVersion.MatchString(v) }

// versionLess compares two release versions numerically, so 0.10.0 is newer
// than 0.9.0. Anything that is not a release version compares as not-less,
// which fails safe: no nag and no downgrade over a shape this cannot read.
func versionLess(a, b string) bool {
	if !isReleaseVersion(a) || !isReleaseVersion(b) {
		return false
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range as {
		an, _ := strconv.Atoi(as[i])
		bn, _ := strconv.Atoi(bs[i])
		if an != bn {
			return an < bn
		}
	}
	return false
}

// latestVersion asks the release API what the newest release is, and answers it
// without the leading v. It is the one fact both the upgrade and the daily
// notice need.
func latestVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseAPIURL, nil)
	if err != nil {
		return "", api.Fail("network_unreachable", "could not build the release request: "+err.Error())
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", api.Fail("network_unreachable", "could not reach "+releaseAPIURL+": "+err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", api.Fail("malformed_response",
			fmt.Sprintf("the release API answered HTTP %d — try again, or see %s", resp.StatusCode, releaseAPIURL))
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release); err != nil {
		return "", api.Fail("malformed_response", "the release API answered something unreadable: "+err.Error())
	}
	version := strings.TrimPrefix(release.TagName, "v")
	if !isReleaseVersion(version) {
		return "", api.Fail("malformed_response",
			"the release API named no readable version (got "+strconv.Quote(release.TagName)+")")
	}
	return version, nil
}

// underNPM reports whether this binary was put here by npm, which is the one
// installer that must stay the owner of its files: a binary swapped underneath
// it leaves package.json claiming a version that is not what runs.
func underNPM(exe string) bool {
	return strings.Contains(exe, string(filepath.Separator)+"node_modules"+string(filepath.Separator))
}

// upgradeCmd is `krowk upgrade`: find out what the latest release is, and when
// it is newer than this build, become it. Which of the three install methods put
// this binary here decides how — a source checkout and an npm install are told
// their own upgrade command rather than having files swapped under a manager
// that believes it owns them.
func upgradeCmd(w io.Writer, f flags, format output.Format, env runctx.Env, colour bool) error {
	if !isReleaseVersion(Version) {
		return api.Fail("not_upgradable",
			"this build came from source (version "+Version+") — upgrade with `git pull && make install`, "+
				"or `go install github.com/krowkcom/cli/cmd/...@latest`")
	}

	ctx, stop := context.WithTimeout(context.Background(), latestCheckTimeout)
	latest, err := latestVersion(ctx)
	stop()
	if err != nil {
		return err
	}
	// An explicit check is a check: the daily notice starts its clock here too,
	// so `krowk upgrade` is never followed the same day by a nag about the
	// version it just reported on.
	writeUpdateState(env, updateState{CheckedAt: time.Now(), Latest: latest})

	if !versionLess(Version, latest) {
		return upgradeReport(w, format, map[string]any{
			"upgraded": false, "version": Version, "latest": latest,
		}, "krowk "+Version+" is the latest release")
	}

	exe, err := os.Executable()
	if err == nil {
		// The symlink target is what has to be replaced — swapping the link
		// itself would orphan the real binary and break every other name for it.
		if resolved, resolveErr := filepath.EvalSymlinks(exe); resolveErr == nil {
			exe = resolved
		}
	}
	if err != nil {
		return api.Fail("not_upgradable", "could not find this binary to replace it: "+err.Error())
	}
	if underNPM(exe) {
		return api.Fail("not_upgradable",
			"this krowk was installed by npm, which owns its files — run `npm install -g @krowk/cli@latest`")
	}
	if runtime.GOOS == "windows" {
		// ponytail: no self-replace on Windows — the release ships a zip and the
		// running .exe cannot be written over. Windows installs come through npm
		// or a manual unpack, and both upgrade the way they installed.
		return api.Fail("not_upgradable",
			"krowk does not replace itself on Windows — run `npm install -g @krowk/cli@latest`, "+
				"or unpack the release archive over this binary: "+releaseDownloadURL+"/v"+latest)
	}

	replaced, err := selfUpgrade(context.Background(), filepath.Dir(exe), latest)
	if err != nil {
		return err
	}
	return upgradeReport(w, format, map[string]any{
		"upgraded": true, "version": latest, "from": Version, "binaries": replaced,
	}, "upgraded "+Version+" → "+latest+"\n  "+strings.Join(replaced, "\n  "))
}

// upgradeReport prints the outcome the way doctor does: lines for a person,
// JSON for everything else.
func upgradeReport(w io.Writer, format output.Format, report map[string]any, human string) error {
	if format != output.Human {
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Fprintln(w, string(b))
		return nil
	}
	fmt.Fprintln(w, human)
	return nil
}

// selfUpgrade downloads the release archive for this platform, verifies it
// against the release's checksums.txt, and writes the binaries it holds over
// the ones in destDir. It answers with the paths it replaced.
//
// The order is the safety: nothing in destDir is touched until the whole
// archive has arrived and its digest has matched, and each binary lands by
// rename — the one moment a running krowk could be half-written is the moment
// the filesystem swaps inodes, which is no moment at all.
func selfUpgrade(ctx context.Context, destDir, version string) ([]string, error) {
	ctx, stop := context.WithTimeout(ctx, downloadTimeout)
	defer stop()

	archive := fmt.Sprintf("krowk_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	base := releaseDownloadURL + "/v" + version

	sums, err := fetchRelease(ctx, base+"/checksums.txt", 1<<20)
	if err != nil {
		return nil, err
	}
	want, ok := checksumFor(string(sums), archive)
	if !ok {
		return nil, api.Fail("malformed_response",
			"checksums.txt on release v"+version+" does not cover "+archive+" — nothing was installed")
	}

	data, err := fetchRelease(ctx, base+"/"+archive, 1<<30)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return nil, api.Fail("malformed_response",
			archive+" does not match its published checksum — nothing was installed; try again")
	}

	replaced, err := extractBinaries(data, destDir)
	if err != nil {
		return nil, err
	}
	if len(replaced) == 0 {
		return nil, api.Fail("malformed_response",
			archive+" holds no krowk binary — nothing was installed")
	}
	return replaced, nil
}

// fetchRelease downloads one release asset whole, bounded so a lying server
// cannot fill memory.
func fetchRelease(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, api.Fail("network_unreachable", "could not build the download request: "+err.Error())
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, api.Fail("network_unreachable", "could not reach "+url+": "+err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, api.Fail("malformed_response",
			fmt.Sprintf("%s answered HTTP %d — nothing was installed", url, resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, api.Fail("network_unreachable", "the download from "+url+" broke off: "+err.Error())
	}
	return data, nil
}

// checksumFor finds one file's digest in a goreleaser checksums.txt, which is
// `<hex>  <name>` per line.
func checksumFor(sums, name string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			return fields[0], true
		}
	}
	return "", false
}

// upgradeBinaries is what an upgrade takes out of the archive — the same pair
// the installer puts down, because they version together.
var upgradeBinaries = map[string]bool{"krowk": true, "krowk-mcp": true}

// extractBinaries writes the archive's binaries into destDir, each through a
// temp file and a rename so a binary is either the old one or the new one and
// never a torn middle.
func extractBinaries(archive []byte, destDir string) ([]string, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, api.Fail("malformed_response", "the release archive is not a gzip file: "+err.Error())
	}
	tr := tar.NewReader(gz)

	var replaced []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return replaced, nil
		}
		if err != nil {
			return replaced, api.Fail("malformed_response", "the release archive is unreadable: "+err.Error())
		}
		// filepath.Base rather than trusting the entry's path: the name decides
		// where this writes, and an archive must not get to spell "../".
		name := filepath.Base(hdr.Name)
		if hdr.Typeflag != tar.TypeReg || !upgradeBinaries[name] {
			continue
		}
		dest := filepath.Join(destDir, name)
		if err := writeBinary(tr, dest); err != nil {
			return replaced, api.Fail("not_upgradable",
				"could not write "+dest+": "+err.Error()+" — anything already replaced stays replaced")
		}
		replaced = append(replaced, dest)
	}
}

// writeBinary lands one binary next to its destination and renames it into
// place, executable.
func writeBinary(r io.Reader, dest string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".new-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // gone already on the happy path; the rename consumed it
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

// --- the daily notice ---

// updateState is what the state file remembers between commands: when the
// release API was last asked, and what it said. CheckedAt moves on every
// attempt, answered or not, so an offline day costs one two-second try rather
// than one per command.
type updateState struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest,omitempty"`
}

// updateStatePath sits next to the config: ~/.config/krowk/update-check.json,
// XDG_CONFIG_HOME honoured, same reasoning as config.GlobalPath.
func updateStatePath(env runctx.Env) string {
	dir := env("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(".krowk", "update-check.json")
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "krowk", "update-check.json")
}

// readUpdateState is lenient the way the credentials reader is: a missing,
// unreadable or torn file all mean "never checked", and the fix for all three
// is the check that is about to happen anyway.
func readUpdateState(env runctx.Env) updateState {
	var st updateState
	data, err := os.ReadFile(updateStatePath(env))
	if err != nil {
		return updateState{}
	}
	if json.Unmarshal(data, &st) != nil {
		return updateState{}
	}
	return st
}

// writeUpdateState records a check. Failing to write is failing to remember,
// which costs one extra check tomorrow and is not worth a word to the caller.
func writeUpdateState(env runctx.Env, st updateState) {
	path := updateStatePath(env)
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	// ponytail: plain write, no temp-and-rename — a torn file reads as "never
	// checked" and heals on the next command.
	_ = os.WriteFile(path, data, 0o644)
}

// maybeNotifyUpdate is the once-a-day nudge: after a command has done its work,
// say on stderr when a newer release exists. stderr because stdout is the
// document a program parses, and this is an aside to whoever is driving.
//
// It stays silent for a source build (git is its upgrade), in CI (nobody there
// can act on it), and when KROWK_NO_UPDATE_CHECK asks it to — which also skips
// the network check entirely, for machines that must not phone GitHub.
//
// The check itself runs at most once per notifyInterval and is bounded by
// notifyCheckTimeout, so the worst any command ever pays is two seconds, once a
// day, after its output is already printed.
func maybeNotifyUpdate(stderr io.Writer, env runctx.Env) {
	if !isReleaseVersion(Version) || inCI(env) || api.Truthy(env("KROWK_NO_UPDATE_CHECK")) {
		return
	}
	st := readUpdateState(env)
	if time.Since(st.CheckedAt) >= notifyInterval {
		ctx, stop := context.WithTimeout(context.Background(), notifyCheckTimeout)
		latest, err := latestVersion(ctx)
		stop()
		if err == nil {
			st.Latest = latest
		}
		st.CheckedAt = time.Now()
		writeUpdateState(env, st)
	}
	if st.Latest != "" && versionLess(Version, st.Latest) {
		fmt.Fprintf(stderr, "krowk %s is available (this is %s) — run `krowk upgrade`\n", st.Latest, Version)
	}
}
