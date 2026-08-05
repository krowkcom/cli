package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// noEnv is the environment of a machine with nothing exported, which is where
// the credentials file is supposed to be the only voice.
func noEnv(string) string { return "" }

// withEnv answers for KROWK_TOKEN alone, which is the only variable any of this
// consults.
func withEnv(token string) func(string) string {
	return func(k string) string {
		if k == "KROWK_TOKEN" {
			return token
		}
		return ""
	}
}

// isolate points the credentials path at a scratch directory, so a test never
// reads or writes the credentials of whoever is running it.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "krowk", "credentials.json")
}

func TestSaveCredentialsRecordsWhatTheRegistrySaid(t *testing.T) {
	path := isolate(t)

	got, err := SaveCredentials("krowk_sk_secret", Identity{KeyID: "key_7f3a", Workspace: "ws_acme"})
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Errorf("stored at %q, want %q", got, path)
	}

	if token := ReadToken(noEnv); token != "krowk_sk_secret" {
		t.Errorf("token = %q", token)
	}
	// The point of writing the identity down is that reading it back costs
	// nothing — no registry, no network.
	id, ok := ReadIdentity(noEnv)
	if !ok || id.KeyID != "key_7f3a" || id.Workspace != "ws_acme" {
		t.Errorf("identity = %+v, ok = %v", id, ok)
	}
}

// A token in a file is a secret, and the file mode is the only thing standing
// between it and every other account on the machine.
func TestSaveCredentialsIsOwnerOnly(t *testing.T) {
	path := isolate(t)

	if _, err := SaveCredentials("krowk_sk_secret", Identity{KeyID: "key_7f3a"}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials mode = %o, want 600", perm)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir mode = %o, want 700", perm)
	}
}

// The temporary file the atomic write goes through must not survive it. One
// left behind is a second copy of the token, at a name nothing will ever clean
// up.
func TestSaveCredentialsLeavesNoStrayCopyOfTheToken(t *testing.T) {
	path := isolate(t)

	if _, err := SaveCredentials("krowk_sk_secret", Identity{KeyID: "key_7f3a"}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "credentials.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("config dir holds %v, want only credentials.json", names)
	}
}

// Logging in with a key the registry could not confirm has to clear the last
// key's identity. Leaving it would have the file name a workspace belonging to
// a token that is no longer in it.
func TestSaveCredentialsClearsAnIdentityItCannotVouchFor(t *testing.T) {
	isolate(t)

	if _, err := SaveCredentials("krowk_sk_first", Identity{KeyID: "key_7f3a", Workspace: "ws_acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredentials("krowk_sk_second", Identity{}); err != nil {
		t.Fatal(err)
	}

	if token := ReadToken(noEnv); token != "krowk_sk_second" {
		t.Errorf("token = %q, want the key just stored", token)
	}
	if id, ok := ReadIdentity(noEnv); ok {
		t.Errorf("identity = %+v, want none — it described the previous key", id)
	}
}

// ReadToken prefers the environment, so the file's identity describes a key
// that is not the one doing the work. Reporting it would name a workspace
// uploads are not going to, which is worse than admitting to not knowing.
func TestReadIdentityStaysQuietWhenTheEnvironmentSuppliesTheToken(t *testing.T) {
	isolate(t)

	if _, err := SaveCredentials("krowk_sk_stored", Identity{KeyID: "key_7f3a", Workspace: "ws_acme"}); err != nil {
		t.Fatal(err)
	}

	env := withEnv("krowk_sk_from_ci")
	if token := ReadToken(env); token != "krowk_sk_from_ci" {
		t.Errorf("token = %q, want the environment's", token)
	}
	if id, ok := ReadIdentity(env); ok {
		t.Errorf("identity = %+v, want none while KROWK_TOKEN is set", id)
	}
	if src := TokenSource(env); src != TokenSourceEnv {
		t.Errorf("source = %q, want %q", src, TokenSourceEnv)
	}
}

func TestTokenSourceNamesWhereTheTokenCameFrom(t *testing.T) {
	isolate(t)

	if src := TokenSource(noEnv); src != TokenSourceNone {
		t.Errorf("source with nothing stored = %q, want %q", src, TokenSourceNone)
	}
	if _, err := SaveCredentials("krowk_sk_stored", Identity{KeyID: "key_7f3a"}); err != nil {
		t.Fatal(err)
	}
	if src := TokenSource(noEnv); src != TokenSourceFile {
		t.Errorf("source after login = %q, want %q", src, TokenSourceFile)
	}
}

// A corrupt file reads as "not logged in" rather than propagating a parse
// error, because the fix is the same either way and half a token is no token.
func TestUnreadableCredentialsReadAsNoKeyRatherThanFailing(t *testing.T) {
	path := isolate(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"token": "krowk_sk_trunc`), 0o600); err != nil {
		t.Fatal(err)
	}

	if token := ReadToken(noEnv); token != "" {
		t.Errorf("token = %q, want none from a truncated file", token)
	}
	if id, ok := ReadIdentity(noEnv); ok {
		t.Errorf("identity = %+v, want none from a truncated file", id)
	}
	if src := TokenSource(noEnv); src != TokenSourceNone {
		t.Errorf("source = %q, want %q", src, TokenSourceNone)
	}
}

// A file written before identities existed holds a token and nothing else. It
// must keep working — an upgrade is not a reason to make someone log in again.
func TestATokenOnlyFileStillAuthenticates(t *testing.T) {
	path := isolate(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"token":"krowk_sk_old"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if token := ReadToken(noEnv); token != "krowk_sk_old" {
		t.Errorf("token = %q, want the one already on disk", token)
	}
	if _, ok := ReadIdentity(noEnv); ok {
		t.Error("a token-only file records no identity, so none should be reported")
	}
}

// The token is the secret; the key ID is not, and is what a support ticket
// quotes. Both belong in the file, but only under the names the reader expects.
func TestCredentialsFileShapeIsStable(t *testing.T) {
	path := isolate(t)

	if _, err := SaveCredentials("krowk_sk_secret", Identity{KeyID: "key_7f3a", Workspace: "ws_acme"}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("credentials are not JSON: %v\n%s", err, data)
	}
	for field, want := range map[string]any{
		"token":     "krowk_sk_secret",
		"key_id":    "key_7f3a",
		"workspace": "ws_acme",
	} {
		if raw[field] != want {
			t.Errorf("%s = %v, want %v", field, raw[field], want)
		}
	}
}
