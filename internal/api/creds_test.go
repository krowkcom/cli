package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// writeRaw puts bytes at the credentials path without going through
// SaveCredentials, which is the only way to test reading a file this version of
// the code would never have written — a legacy one, or a broken one.
func writeRaw(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSaveCredentialsRecordsWhatTheRegistrySaid(t *testing.T) {
	path := isolate(t)

	got, err := SaveCredentials("krowk_sk_secret", Identity{KeyID: "key_7f3a", Workspace: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Errorf("stored at %q, want %q", got, path)
	}

	if token := ReadToken(noEnv, ""); token != "krowk_sk_secret" {
		t.Errorf("token = %q", token)
	}
	// The point of writing the identity down is that reading it back costs
	// nothing — no registry, no network.
	id, ok := ReadIdentity(noEnv, "")
	if !ok || id.KeyID != "key_7f3a" || id.Workspace != "acme" {
		t.Errorf("identity = %+v, ok = %v", id, ok)
	}
}

// The one thing the whole rewrite exists for: a second login must not be a way
// to lose the first key.
func TestSaveCredentialsAddsAWorkspaceWithoutDisturbingTheOthers(t *testing.T) {
	isolate(t)

	if _, err := SaveCredentials("krowk_sk_acme", Identity{KeyID: "key_1", Workspace: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredentials("krowk_sk_personal", Identity{KeyID: "key_2", Workspace: "personal"}); err != nil {
		t.Fatal(err)
	}

	if token := ReadToken(noEnv, "acme"); token != "krowk_sk_acme" {
		t.Errorf("acme token = %q, want the key stored before the second login", token)
	}
	if token := ReadToken(noEnv, "personal"); token != "krowk_sk_personal" {
		t.Errorf("personal token = %q", token)
	}
	// Logging in is how someone says which workspace they mean to work in now,
	// so the newest key is the one an unqualified command reaches for.
	if token := ReadToken(noEnv, ""); token != "krowk_sk_personal" {
		t.Errorf("default token = %q, want the workspace just logged in to", token)
	}
	id, ok := ReadIdentity(noEnv, "acme")
	if !ok || id.KeyID != "key_1" || id.Workspace != "acme" {
		t.Errorf("acme identity = %+v, ok = %v", id, ok)
	}
}

// A second key for the same workspace is a re-login. The token it replaces was
// very likely revoked to make it, and keeping it would leave a dead key behind.
func TestLoggingInAgainReplacesThatWorkspacesKey(t *testing.T) {
	isolate(t)

	if _, err := SaveCredentials("krowk_sk_old", Identity{KeyID: "key_1", Workspace: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredentials("krowk_sk_new", Identity{KeyID: "key_9", Workspace: "acme"}); err != nil {
		t.Fatal(err)
	}

	if token := ReadToken(noEnv, "acme"); token != "krowk_sk_new" {
		t.Errorf("token = %q, want the key from the newer login", token)
	}
	if id, _ := ReadIdentity(noEnv, "acme"); id.KeyID != "key_9" {
		t.Errorf("key_id = %q, want the newer key's", id.KeyID)
	}
	if stored := StoredWorkspaces(); len(stored) != 1 {
		t.Errorf("stored %+v, want one entry — a re-login is not a second key", stored)
	}
}

// A key the registry could not confirm has no workspace to be filed under, so
// it goes under "default" with its recorded workspace left empty. The emptiness
// is the record of what the registry said, which was nothing.
func TestAnUnconfirmedKeyFilesUnderDefaultAndClaimsNoWorkspace(t *testing.T) {
	isolate(t)

	if _, err := SaveCredentials("krowk_sk_offline", Identity{KeyID: "key_7f3a"}); err != nil {
		t.Fatal(err)
	}

	if token := ReadToken(noEnv, ""); token != "krowk_sk_offline" {
		t.Errorf("default token = %q", token)
	}
	if token := ReadToken(noEnv, "default"); token != "krowk_sk_offline" {
		t.Errorf("token under \"default\" = %q, want the unconfirmed key", token)
	}
	id, ok := ReadIdentity(noEnv, "")
	if !ok || id.KeyID != "key_7f3a" {
		t.Fatalf("identity = %+v, ok = %v", id, ok)
	}
	if id.Workspace != "" {
		t.Errorf("workspace = %q, want none — the registry never named one", id.Workspace)
	}
}

// Asking for a workspace nothing is stored under is an ordinary "no key", not a
// failure: the caller finds out exactly as it would have with an empty store.
func TestANameWithNoKeyStoredUnderItReadsAsNoKey(t *testing.T) {
	isolate(t)

	if _, err := SaveCredentials("krowk_sk_acme", Identity{KeyID: "key_1", Workspace: "acme"}); err != nil {
		t.Fatal(err)
	}

	if token := ReadToken(noEnv, "nowhere"); token != "" {
		t.Errorf("token = %q, want none for a workspace never logged in to", token)
	}
	if id, ok := ReadIdentity(noEnv, "nowhere"); ok {
		t.Errorf("identity = %+v, want none", id)
	}
	if src := TokenSource(noEnv, "nowhere"); src != TokenSourceNone {
		t.Errorf("source = %q, want %q", src, TokenSourceNone)
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
	if _, err := SetDefaultWorkspace("default"); err != nil {
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

// ReadToken prefers the environment, so the file's identity describes a key
// that is not the one doing the work. Reporting it would name a workspace
// uploads are not going to, which is worse than admitting to not knowing.
func TestReadIdentityStaysQuietWhenTheEnvironmentSuppliesTheToken(t *testing.T) {
	isolate(t)

	if _, err := SaveCredentials("krowk_sk_stored", Identity{KeyID: "key_7f3a", Workspace: "acme"}); err != nil {
		t.Fatal(err)
	}

	env := withEnv("krowk_sk_from_ci")
	// Naming a workspace explicitly does not change the answer: the environment
	// holds the key that will do the work whichever entry was asked for.
	for _, ws := range []string{"", "acme"} {
		if token := ReadToken(env, ws); token != "krowk_sk_from_ci" {
			t.Errorf("token for %q = %q, want the environment's", ws, token)
		}
		if id, ok := ReadIdentity(env, ws); ok {
			t.Errorf("identity for %q = %+v, want none while KROWK_TOKEN is set", ws, id)
		}
		if src := TokenSource(env, ws); src != TokenSourceEnv {
			t.Errorf("source for %q = %q, want %q", ws, src, TokenSourceEnv)
		}
	}
}

func TestTokenSourceNamesWhereTheTokenCameFrom(t *testing.T) {
	isolate(t)

	if src := TokenSource(noEnv, ""); src != TokenSourceNone {
		t.Errorf("source with nothing stored = %q, want %q", src, TokenSourceNone)
	}
	if _, err := SaveCredentials("krowk_sk_stored", Identity{KeyID: "key_1", Workspace: "acme"}); err != nil {
		t.Fatal(err)
	}
	if src := TokenSource(noEnv, ""); src != TokenSourceFile {
		t.Errorf("source after login = %q, want %q", src, TokenSourceFile)
	}
	if src := TokenSource(noEnv, "acme"); src != TokenSourceFile {
		t.Errorf("source for the named workspace = %q, want %q", src, TokenSourceFile)
	}
}

// A corrupt file reads as "not logged in" rather than propagating a parse
// error, because the fix is the same either way and half a token is no token.
func TestUnreadableCredentialsReadAsNoKeyRatherThanFailing(t *testing.T) {
	path := isolate(t)
	writeRaw(t, path, `{"token": "krowk_sk_trunc`)

	if token := ReadToken(noEnv, ""); token != "" {
		t.Errorf("token = %q, want none from a truncated file", token)
	}
	if id, ok := ReadIdentity(noEnv, ""); ok {
		t.Errorf("identity = %+v, want none from a truncated file", id)
	}
	if src := TokenSource(noEnv, ""); src != TokenSourceNone {
		t.Errorf("source = %q, want %q", src, TokenSourceNone)
	}
	if stored := StoredWorkspaces(); len(stored) != 0 {
		t.Errorf("stored %+v, want nothing from a truncated file", stored)
	}
}

// A file written before identities existed holds a token and nothing else. It
// must keep working — an upgrade is not a reason to make someone log in again.
func TestATokenOnlyFileStillAuthenticates(t *testing.T) {
	path := isolate(t)
	writeRaw(t, path, `{"token":"krowk_sk_old"}`)

	if token := ReadToken(noEnv, ""); token != "krowk_sk_old" {
		t.Errorf("token = %q, want the one already on disk", token)
	}
	if _, ok := ReadIdentity(noEnv, ""); ok {
		t.Error("a token-only file records no identity, so none should be reported")
	}
	// With no workspace recorded it can only be the unconfirmed-key entry, and
	// it has to be the default, because there is nothing else to point at.
	if token := ReadToken(noEnv, "default"); token != "krowk_sk_old" {
		t.Errorf("token under \"default\" = %q, want the migrated key", token)
	}
	if src := TokenSource(noEnv, ""); src != TokenSourceFile {
		t.Errorf("source = %q, want %q", src, TokenSourceFile)
	}
}

// A file from the one-token era that did record a workspace files under that
// workspace, so a name that used to be implicit keeps working by that name.
func TestALegacyFileWithAWorkspaceReadsUnderThatName(t *testing.T) {
	path := isolate(t)
	writeRaw(t, path, `{"token":"krowk_sk_old","key_id":"key_7f3a","workspace":"acme"}`)

	if token := ReadToken(noEnv, ""); token != "krowk_sk_old" {
		t.Errorf("default token = %q", token)
	}
	if token := ReadToken(noEnv, "acme"); token != "krowk_sk_old" {
		t.Errorf("acme token = %q, want the migrated key", token)
	}
	id, ok := ReadIdentity(noEnv, "acme")
	if !ok || id.KeyID != "key_7f3a" || id.Workspace != "acme" {
		t.Errorf("identity = %+v, ok = %v", id, ok)
	}
	stored := StoredWorkspaces()
	if len(stored) != 1 || stored[0].Name != "acme" || !stored[0].Default {
		t.Errorf("stored = %+v, want one default entry named acme", stored)
	}
}

// Migration happens on read, so the first login after an upgrade has to carry
// the old key into the new file rather than write over it.
func TestLoggingInOverALegacyFileKeepsTheKeyThatWasAlreadyThere(t *testing.T) {
	path := isolate(t)
	writeRaw(t, path, `{"token":"krowk_sk_old","key_id":"key_1","workspace":"acme"}`)

	if _, err := SaveCredentials("krowk_sk_new", Identity{KeyID: "key_2", Workspace: "personal"}); err != nil {
		t.Fatal(err)
	}

	if token := ReadToken(noEnv, "acme"); token != "krowk_sk_old" {
		t.Errorf("acme token = %q, want the key the legacy file held", token)
	}
	if token := ReadToken(noEnv, "personal"); token != "krowk_sk_new" {
		t.Errorf("personal token = %q", token)
	}
	if token := ReadToken(noEnv, ""); token != "krowk_sk_new" {
		t.Errorf("default token = %q, want the workspace just logged in to", token)
	}

	// The file it left behind is the new shape, whatever shape it read.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("credentials are not JSON: %v\n%s", err, data)
	}
	if _, ok := raw["workspaces"]; !ok {
		t.Errorf("saved file is still the legacy shape:\n%s", data)
	}
	if _, ok := raw["token"]; ok {
		t.Errorf("saved file still holds a top-level token:\n%s", data)
	}
}

// A legacy key whose workspace matches the one being logged in to is the same
// workspace, so it is replaced rather than kept twice under one name.
func TestLoggingInToTheLegacyFilesOwnWorkspaceReplacesIt(t *testing.T) {
	path := isolate(t)
	writeRaw(t, path, `{"token":"krowk_sk_old","key_id":"key_1","workspace":"acme"}`)

	if _, err := SaveCredentials("krowk_sk_new", Identity{KeyID: "key_2", Workspace: "acme"}); err != nil {
		t.Fatal(err)
	}

	if token := ReadToken(noEnv, "acme"); token != "krowk_sk_new" {
		t.Errorf("token = %q, want the key from the newer login", token)
	}
	if stored := StoredWorkspaces(); len(stored) != 1 {
		t.Errorf("stored %+v, want one entry", stored)
	}
}

func TestStoredWorkspacesSortsAndMarksTheDefault(t *testing.T) {
	isolate(t)

	if got := StoredWorkspaces(); len(got) != 0 {
		t.Errorf("stored = %+v, want nothing before any login", got)
	}

	for _, ws := range []struct{ name, key string }{
		{"zeta", "key_z"}, {"acme", "key_a"}, {"middle", "key_m"},
	} {
		if _, err := SaveCredentials("krowk_sk_"+ws.name, Identity{KeyID: ws.key, Workspace: ws.name}); err != nil {
			t.Fatal(err)
		}
	}

	stored := StoredWorkspaces()
	wantNames := []string{"acme", "middle", "zeta"}
	if len(stored) != len(wantNames) {
		t.Fatalf("stored %d keys, want %d: %+v", len(stored), len(wantNames), stored)
	}
	for i, want := range wantNames {
		if stored[i].Name != want {
			t.Errorf("stored[%d].Name = %q, want %q — the listing has to be sorted", i, stored[i].Name, want)
		}
		// Only the last login is the default, and it is not the first by name,
		// so this would catch a marker that just followed the ordering.
		if isDefault := stored[i].Name == "middle"; stored[i].Default != isDefault {
			t.Errorf("stored[%d] (%s) default = %v, want %v", i, stored[i].Name, stored[i].Default, isDefault)
		}
	}
	if stored[0].KeyID != "key_a" || stored[0].Workspace != "acme" {
		t.Errorf("stored[0] = %+v, want what the registry said about acme", stored[0])
	}
}

func TestSetDefaultWorkspacePointsCommandsAtAnotherStoredKey(t *testing.T) {
	path := isolate(t)

	if _, err := SaveCredentials("krowk_sk_acme", Identity{KeyID: "key_1", Workspace: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredentials("krowk_sk_personal", Identity{KeyID: "key_2", Workspace: "personal"}); err != nil {
		t.Fatal(err)
	}

	got, err := SetDefaultWorkspace("acme")
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Errorf("wrote %q, want %q", got, path)
	}
	if token := ReadToken(noEnv, ""); token != "krowk_sk_acme" {
		t.Errorf("default token = %q, want acme's", token)
	}
	// Repointing is not a way to lose the key that used to be the default.
	if token := ReadToken(noEnv, "personal"); token != "krowk_sk_personal" {
		t.Errorf("personal token = %q, want it untouched", token)
	}
	id, ok := ReadIdentity(noEnv, "")
	if !ok || id.Workspace != "acme" {
		t.Errorf("identity = %+v, ok = %v", id, ok)
	}
}

// Pointing the default at nothing would have every later command report "not
// logged in" while the keys sit untouched in the file — a failure the user
// could not connect back to the command that caused it.
func TestSetDefaultWorkspaceRefusesANameNothingIsStoredUnder(t *testing.T) {
	isolate(t)

	if _, err := SaveCredentials("krowk_sk_acme", Identity{KeyID: "key_1", Workspace: "acme"}); err != nil {
		t.Fatal(err)
	}

	_, err := SetDefaultWorkspace("acmee")
	if err == nil {
		t.Fatal("naming a workspace with no stored key succeeded, want an error")
	}
	// The usual cause is a typo, so the message has to show what is actually
	// there rather than leave the user guessing.
	if msg := err.Error(); !strings.Contains(msg, "acme") {
		t.Errorf("error = %q, want it to list the stored names", msg)
	}
	if token := ReadToken(noEnv, ""); token != "krowk_sk_acme" {
		t.Errorf("default token = %q, want the pointer left where it was", token)
	}
}

// With nothing stored there is no list to offer, so the message has to say what
// to do instead.
func TestSetDefaultWorkspaceOnAnEmptyStoreSaysToLogIn(t *testing.T) {
	isolate(t)

	_, err := SetDefaultWorkspace("acme")
	if err == nil {
		t.Fatal("setting a default with nothing stored succeeded, want an error")
	}
	if msg := err.Error(); !strings.Contains(msg, "krowk auth login") {
		t.Errorf("error = %q, want it to point at `krowk auth login`", msg)
	}
}

// The token is the secret; the key ID is not, and is what a support ticket
// quotes. Both belong in the file, but only under the names the reader expects.
func TestCredentialsFileShapeIsStable(t *testing.T) {
	path := isolate(t)

	if _, err := SaveCredentials("krowk_sk_secret", Identity{KeyID: "key_7f3a", Workspace: "acme"}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Default    string `json:"default"`
		Workspaces map[string]struct {
			Token     string `json:"token"`
			KeyID     string `json:"key_id"`
			Workspace string `json:"workspace"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("credentials are not JSON: %v\n%s", err, data)
	}
	if raw.Default != "acme" {
		t.Errorf("default = %q, want acme", raw.Default)
	}
	entry, ok := raw.Workspaces["acme"]
	if !ok {
		t.Fatalf("no entry filed under acme:\n%s", data)
	}
	if entry.Token != "krowk_sk_secret" || entry.KeyID != "key_7f3a" || entry.Workspace != "acme" {
		t.Errorf("entry = %+v, want the token and what the registry said about it", entry)
	}
}
