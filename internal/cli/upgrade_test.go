package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/krowkcom/cli/internal/output"
)

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.3.0", "0.4.0", true},
		{"0.9.0", "0.10.0", true}, // numeric, not lexical
		{"0.4.0", "0.4.0", false},
		{"0.4.1", "0.4.0", false},
		{"1.0.0", "0.9.9", false},
		{"dev", "0.4.0", false},              // a source build never compares
		{"0.3.0-5-gd88d552", "0.4.0", false}, // neither does a git describe
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestIsReleaseVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"0.3.0":            true,
		"12.0.5":           true,
		"dev":              false,
		"0.1.0-rc1":        false,
		"0.3.0-5-gd88d552": false,
		"0.3.0-dirty":      false,
		"v0.3.0":           false,
	} {
		if got := isReleaseVersion(v); got != want {
			t.Errorf("isReleaseVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

// envMap is an env func over a map, the way the other tests build one.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// releaseServer serves a fake latest release and its assets: the archive under
// the goreleaser name for this platform, and a checksums.txt that covers it.
func releaseServer(t *testing.T, version string, archive []byte) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(archive)
	name := fmt.Sprintf("krowk_%s_%s_%s.tar.gz", version, goosForTest(), goarchForTest())
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"tag_name": "v" + version})
	})
	mux.HandleFunc("/v"+version+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	})
	mux.HandleFunc("/v"+version+"/"+name, func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	oldAPI, oldDL := releaseAPIURL, releaseDownloadURL
	releaseAPIURL, releaseDownloadURL = srv.URL+"/latest", srv.URL
	t.Cleanup(func() { releaseAPIURL, releaseDownloadURL = oldAPI, oldDL })
	return srv
}

func goosForTest() string   { return runtime.GOOS }
func goarchForTest() string { return runtime.GOARCH }

// tarGz builds an archive of regular files, the shape goreleaser ships.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestSelfUpgradeReplacesBinaries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "krowk"), []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	archive := tarGz(t, map[string]string{
		"krowk":     "new krowk",
		"krowk-mcp": "new mcp",
		"README.md": "not a binary",
	})
	releaseServer(t, "9.9.9", archive)

	replaced, err := selfUpgrade(context.Background(), dir, "9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced) != 2 {
		t.Fatalf("replaced %v, want both binaries", replaced)
	}
	got, err := os.ReadFile(filepath.Join(dir, "krowk"))
	if err != nil || string(got) != "new krowk" {
		t.Errorf("krowk = %q, %v — want the new bytes", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); !os.IsNotExist(err) {
		t.Error("README.md landed in the bin dir — only the binaries may")
	}
	info, _ := os.Stat(filepath.Join(dir, "krowk-mcp"))
	if info.Mode().Perm() != 0o755 {
		t.Errorf("krowk-mcp mode = %v, want 0755", info.Mode().Perm())
	}
}

func TestSelfUpgradeRefusesBadChecksum(t *testing.T) {
	dir := t.TempDir()
	archive := tarGz(t, map[string]string{"krowk": "new"})
	srv := releaseServer(t, "9.9.9", archive)
	// Reroute the archive to different bytes than checksums.txt promises.
	name := fmt.Sprintf("krowk_9.9.9_%s_%s.tar.gz", goosForTest(), goarchForTest())
	releaseDownloadURL = srv.URL + "/tampered"
	mux := http.NewServeMux()
	mux.HandleFunc("/tampered/v9.9.9/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		sum := sha256.Sum256(archive)
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	})
	mux.HandleFunc("/tampered/v9.9.9/"+name, func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarGz(t, map[string]string{"krowk": "evil"}))
	})
	tampered := httptest.NewServer(mux)
	defer tampered.Close()
	releaseDownloadURL = tampered.URL + "/tampered"

	if _, err := selfUpgrade(context.Background(), dir, "9.9.9"); err == nil {
		t.Fatal("a checksum mismatch must refuse to install")
	}
	if _, err := os.Stat(filepath.Join(dir, "krowk")); !os.IsNotExist(err) {
		t.Error("nothing may be written on a checksum mismatch")
	}
}

func TestUpgradeCmdRefusesSourceBuild(t *testing.T) {
	old := Version
	Version = "dev"
	defer func() { Version = old }()

	var out bytes.Buffer
	err := upgradeCmd(&out, flags{}, output.Human, envMap(nil), false)
	if err == nil || !strings.Contains(err.Error(), "not_upgradable") {
		t.Fatalf("a source build must refuse to upgrade itself, got %v", err)
	}
}

func TestUpgradeCmdUpToDate(t *testing.T) {
	releaseServer(t, "1.2.3", tarGz(t, map[string]string{"krowk": "x"}))
	old := Version
	Version = "1.2.3"
	defer func() { Version = old }()

	env := envMap(map[string]string{"XDG_CONFIG_HOME": t.TempDir()})
	var out bytes.Buffer
	if err := upgradeCmd(&out, flags{}, output.Human, env, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "latest release") {
		t.Errorf("output = %q, want an up-to-date report", out.String())
	}
}

func TestMaybeNotifyUpdate(t *testing.T) {
	releaseServer(t, "9.9.9", nil)
	old := Version
	Version = "1.0.0"
	defer func() { Version = old }()

	cfg := t.TempDir()
	env := envMap(map[string]string{"XDG_CONFIG_HOME": cfg})

	var stderr bytes.Buffer
	maybeNotifyUpdate(&stderr, env)
	if !strings.Contains(stderr.String(), "9.9.9") || !strings.Contains(stderr.String(), "krowk upgrade") {
		t.Fatalf("stderr = %q, want the upgrade nudge", stderr.String())
	}

	// A fresh check within the interval must not hit the network: point the API
	// somewhere dead and rely on the state file alone.
	releaseAPIURL = "http://127.0.0.1:1/latest"
	stderr.Reset()
	maybeNotifyUpdate(&stderr, env)
	if !strings.Contains(stderr.String(), "9.9.9") {
		t.Errorf("stderr = %q, want the nudge from the cached answer", stderr.String())
	}
}

func TestMaybeNotifyUpdateStaysQuiet(t *testing.T) {
	old := Version
	Version = "1.0.0"
	defer func() { Version = old }()
	cfg := t.TempDir()
	writeUpdateState(envMap(map[string]string{"XDG_CONFIG_HOME": cfg}),
		updateState{CheckedAt: time.Now(), Latest: "9.9.9"})

	cases := map[string]map[string]string{
		"in CI":     {"XDG_CONFIG_HOME": cfg, "CI": "true"},
		"opted out": {"XDG_CONFIG_HOME": cfg, "KROWK_NO_UPDATE_CHECK": "1"},
	}
	for name, envm := range cases {
		var stderr bytes.Buffer
		maybeNotifyUpdate(&stderr, envMap(envm))
		if stderr.Len() != 0 {
			t.Errorf("%s: stderr = %q, want silence", name, stderr.String())
		}
	}

	// Up to date: a check happened, and nothing is said.
	Version = "9.9.9"
	var stderr bytes.Buffer
	maybeNotifyUpdate(&stderr, envMap(map[string]string{"XDG_CONFIG_HOME": cfg}))
	if stderr.Len() != 0 {
		t.Errorf("up to date: stderr = %q, want silence", stderr.String())
	}
}
