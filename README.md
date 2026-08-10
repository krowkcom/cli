# krowk

Permalinks for agent output. Push a screenshot, get a URL that unfurls in
GitHub, Slack, Basecamp and Linear with the run metadata attached.

Proof of concept. The CLI speaks the registry's real protocol and is tested
against both a local stand-in and a running
[krowk-registry](../krowk-registry); what is still missing is a Slack app and a
first tag (see [What the POC still needs](#what-the-poc-still-needs)).

```bash
krowk push ../../tmp/screenshots/foobar.jpg \
  --pull-request="https://github.com/acme/storefront/pull/412" \
  --reference="https://linear.app/acme/issue/ENG-9" \
  --session="3fe6808d-088d-4a6f-a04c-cc9690bcf852"
```

```
✓ uploaded  foobar.jpg  412 KB
  https://krowk.com/a/art_2e1d
  run run_8Kd2wq · expires in 24h
```

That link is the **card page**: an HTML page carrying the artifact and its run
metadata in OpenGraph tags, which is what a paste destination unfurls into a
preview. The bytes themselves live on the CDN and come back as `file_url` — it
is what an image embed has to name, and it is not the link to hand to a person.

Go, standard library only. One static binary, no runtime to install — agent
containers rarely have Node, and the whole point is that the upload step never
fails for boring reasons.

## Install

```bash
go install github.com/krowkcom/cli/cmd/krowk@latest
```

The release pipeline is built but no tag has been cut yet, so nothing below
works today. Once one is:

```bash
# The installer: picks the platform, verifies the checksum, installs both
# binaries and drops the agent skill in ~/.claude/skills if that exists.
curl -fsSL https://krowk.com/install | bash

# Or the archive by hand. Both binaries are in one per platform.
curl -sSfL https://github.com/krowkcom/cli/releases/latest/download/krowk_0.1.0_linux_amd64.tar.gz \
  | tar -xz -C /usr/local/bin krowk krowk-mcp

# npm, for people already in Node. The bare `krowk` name is blocked by npm's
# typosquat filter ("too similar to growl"), so the wrapper is scoped; npx runs
# a package's only bin, and this one's is still called `krowk`.
npx @krowk/cli push screenshot.png
```

Linux and macOS on amd64 and arm64, Windows on amd64; `checksums.txt` on each
release covers every archive. A Homebrew tap comes off the same build and is not
wired up yet. See [Releasing](#releasing) for how a tag becomes those files, and
[What the POC still needs](#what-the-poc-still-needs) for what a human has to do
before the npm half of it can publish.

`https://krowk.com/install` is a redirect to
[`scripts/install.sh`](scripts/install.sh) on `main`, so the bytes come from
GitHub and the site never serves a stale installer. It reads `KROWK_BIN_DIR`,
`KROWK_VERSION` and `KROWK_SKIP_SKILL=1`, verifies the SHA-256 against the
release's `checksums.txt` before anything becomes executable, and asks nothing:
there is no wizard, because a keyless push works the moment the binary lands.
`scripts/install_test.sh` runs it end to end against a release built locally and
served over loopback.

## The agent skill

[`skills/krowk/SKILL.md`](skills/krowk/SKILL.md) is what an agent reads to know
which krowk command to reach for — the invariants (`--json` for anything parsed,
breadcrumbs are commands, a claim token is a one-shot secret that never goes in a
PR comment), a task-to-command table, the decision trees, and the exit codes.

The installer copies it into `~/.claude/skills/krowk/SKILL.md` when that
directory exists. Copy it yourself for any other agent.

It is prose about a command surface, so it is held to the surface:
`internal/skill` parses every `krowk …` in the file's code spans and blocks and
resolves each against the same catalog `krowk help --json` serialises, flags
included. A renamed flag fails the build rather than failing an agent.

## Commands

| Command | What it does |
| --- | --- |
| `krowk push <file...>` | Upload files, get a link for each |
| `krowk uploads create <file...>` | The same thing, spelled out |
| `krowk uploads list` | List uploads, newest first — the workspace's, or one run's with `--run` (`--limit`, `--before`) |
| `krowk uploads show <artifact>` | Read one artifact back |
| `krowk uploads attach <artifact> --run <run>` | Put an upload under a run after it was uploaded |
| `krowk uploads delete <artifact> [claim-token]` | Take an upload down — the bytes go at once, and it cannot be undone |
| `krowk runs start` | Open a run to group later uploads under |
| `krowk runs list` | List the workspace's runs, newest first (`--limit`, `--before`) |
| `krowk runs show <run>` | Read one run back, with everything recorded on it |
| `krowk runs finish <run>` | Close a run |
| `krowk claim <artifact> <claim-token>` | Keep an anonymous upload past its expiry (`--run` groups it while claiming) |
| `krowk auth login` | Approve this machine in a browser and store the key it mints, in `~/.config/krowk/credentials.json` (0600) — `--token <token>` stores one you already have, `--no-browser` prints the code and the page instead |
| `krowk auth token` | Print the stored token, for scripts |
| `krowk auth verify` | Ask the registry which key this is, and the workspace it acts in |
| `krowk doctor` | Report version, API reachability, auth and detected run context |
| `krowk registry serve` | Run the local stand-in registry to develop against |
| `krowk help [command]` | Show the help, or one command's own — `--json` for the machine-readable surface |

Upload flags: `--run`, `--pull-request`, `--reference` (repeatable), `--session`,
`--title`, plus `--repo` / `--commit` / `--agent` to override detection. Flags
may follow the filenames.

Global flags: `--dev`, `--format human|json|markdown|url`, `--json`, `--quiet`,
`--help`, `--version`. Output is human on a TTY and JSON when piped, so an agent
that captures stdout gets structured data without asking for it.

## The command surface, as data

`krowk help --json` answers with the whole surface — every command, its usage,
its arguments, its flags with their types and defaults, the global flags and the
environment variables — so an agent discovers what krowk can do without parsing
a help text written for a person:

```jsonc
{
  "name": "krowk",
  "version": "0.1.0",
  "commands": [
    { "name": "push",
      "usage": "krowk push <file...> [flags]",
      "summary": "Upload files, get a link for each",
      "args": [ { "name": "file", "required": true, "repeated": true } ],
      "flags": [ { "name": "run", "type": "string", "default": "",
                   "usage": "Attach to an existing run instead of opening one" } ] }
  ],
  "global_flags": [ ],
  "environment": [ { "name": "KROWK_TOKEN", "usage": "API token — wins over the credentials file" } ]
}
```

`krowk help <command> --json` narrows it to one entry — `krowk <command> --help
--json` is the same question and the same answer. Like every other command, help
is human on a terminal and JSON when piped, and `--json` or `--format json` asks
for it outright.

**It cannot lie about the real surface.** The command list in the human help is
*rendered from the same catalog* the JSON serialises, and tests hold that
catalog to the code rather than to itself: every command in it must have a case
in the routing switch and every routed command must be in it; every flag it
advertises must be one the flag set actually parses, with the same default, and
every flag that parses must be documented; every environment variable it
promises must be one something reads.

The whole surface — the CLI's and the MCP server's six tools — is also pinned to
`internal/cli/testdata/surface.json`. Changing it is a diff of what krowk
promises rather than a line buried in a change to how a command works:

```bash
go test ./internal/cli -run TestSurface -update   # regenerate, deliberately
```

Regenerating means updating the command table above, and the MCP tool table
below, in the same commit.

**One file, one artifact, one link.** Pushing three files creates three
artifacts with three URLs. What groups them is a run, not an artifact.

**Logging in goes through a browser, and the code cannot fetch the key.**
`auth login` asks the registry to open an authorization and gets back two things:
a slug and a short code. The slug is what the CLI polls, and it never appears in
a browser; the code is what a person confirms on the page, and approving is all
it can do. So the half that travels — printed in a terminal, pasted into a chat,
read out over a call — is not exchangeable for a key by whoever sees it. The key
is handed over exactly once: the registry keeps no plaintext after the poll that
collects it, so a second one answers `410 spent`, and a login that lapses
unanswered answers `410 expired`.

The page comes out of a response body and is handed to the desktop's URL handler,
which reaches much further than an HTTP client does — `file://` walks the local
disk, and desktops register schemes that start programs with arguments. So krowk
opens `http` and `https` and nothing else, and `http` only where the registry
itself is `http`: a local one, or a self-hosted one inside a private network.
Over SSH, or with no display, it opens nothing and prints the code and the page
instead — which is also what `--no-browser` asks for. Everything a person needs
while the command waits goes to stderr, so stdout stays the single document a
program parses.

**Logging in with a key asks first.** `--token` is unchanged, and it is how CI
logs in: nothing is opened and nothing is polled. It reads the key back from the
registry before writing it down, so a mistyped token fails while the real one is
still on the clipboard rather than at the next push. A key the registry rejects is not
stored, and never replaces a working one; a registry that cannot be reached is
not evidence about the key, so the token is stored unconfirmed and `auth verify`
settles it later. What came back — the key ID and the workspace — is kept
alongside the token, which is how `doctor` names the workspace an upload would
land in without a round trip. `KROWK_TOKEN` outranks that file, so when it is
set the recorded workspace describes a different key and is withheld rather than
reported.

## Exit codes

A script branching on the exit code should not have to parse anything, so the
number says what class of failure it was. The taxonomy is small on purpose —
what a caller *does* next is what separates the codes, not which of the
registry's two dozen error codes came back, and the `error` field in the JSON
envelope is still there when the exact one matters.

| Code | Means | What to do |
| --- | --- | --- |
| 0 | It worked | — |
| 1 | The command was wrong, or krowk failed on its own — a bad flag, an unknown command, an unreadable file, a port that will not bind. Also anything unclassified | Fix the command |
| 2 | Not found — no such artifact or run in this workspace, no such endpoint at this base URL, or a claim token the registry does not recognise, which it answers as no such record so that guessing learns nothing | Check the slug and the token, or `KROWK_API_URL` |
| 3 | Refused for want of credentials — no key where one is needed, a key the registry rejects, a browser login somebody denied, or no claim token where that is the only authority | `krowk auth login`, or pass the claim token |
| 4 | The registry understood the request and refused it — validation, an upload already finalized, a run that needs a key | Change something; retrying unchanged answers the same |
| 5 | Rate limited | Wait — the error body carries `retry_after` when the registry sent one |
| 6 | The bytes did not move — the registry or object storage could not be reached, or storage refused the transfer | Retry |
| 7 | The registry failed on its side (5xx), or answered a success this client could not read | Retry, and report it if it persists |
| 8 | Gone — the artifact expired or was taken down, or a browser login lapsed before anybody approved it | Upload again, or log in again; no retry brings any of them back |

1 stays the catch-all it always was, so anything checking `!= 0` keeps working
and a failure that gains a class can only ever move *out* of 1.

## Pasting the result

There is no single paste-ready string, because the surfaces disagree.

| Destination | Use | Why |
| --- | --- | --- |
| GitHub, Linear, Notion | `--format markdown` | GitHub builds preview cards only for its own resources, so a bare third-party link renders as a plain blue anchor no matter what OpenGraph tags the card page carries. It *does* render image URLs inline, so the image embed is what actually shows the artifact in a PR comment — which is why the embed names `file_url` and is wrapped in a link to the card, `[![name](file_url)](url)`: it renders *and* clicks through to the run metadata. A file that is not an image has nothing to embed, so it is a plain link to the card. |
| Slack, Basecamp | `--format url` | Both unfurl a bare URL into a card of their own. Slack renders no markdown image embeds at all — pasting the embed form there shows raw text. |

`--json` carries both forms under `paste`, one line per artifact:

```json
{
  "ok": true,
  "data": { },
  "paste": {
    "markdown": "[![foobar.jpg](https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg)](https://krowk.com/a/art_2e1d)",
    "url": "https://krowk.com/a/art_2e1d"
  }
}
```

Link labels — the title or the filename — are escaped for CommonMark, so
`frame[0].png` pastes as a working link rather than a broken one.

## The JSON envelope

Every command that succeeds answers in the same envelope, so an agent parses one
shape whichever command it ran:

```jsonc
{
  "ok": true,
  "data": { },                  // the command's own result
  "paste": { },                 // both paste forms, where there is a link to paste
  "summary": "1 artifact, 27 B, run run_7f3a",
  "breadcrumbs": [              // the calls left to make — omitted when there are none
    {
      "action": "keep past expiry",
      "cmd": "krowk claim art_2e1d krowk_claim_2b7f",
      "description": "this upload is anonymous and expires within the day; claiming it with a key keeps it and moves it into that key's workspace. The token is shown once and spent once"
    }
  ]
}
```

A failure is the same envelope with `"ok": false` and an `error` object carrying
the code, the `fix` line and `retryable`.

Breadcrumbs are commands, not hints. `cmd` is ready to run, with this result's
own slugs and tokens already in it — `action` is the short label, `description`
says what running it achieves and what happens if it is not run. All three
fields are always present.

A `<placeholder>` in a `cmd` is a value this side genuinely does not have: the
run to attach a claimed upload to, the file a freshly opened run is to be fed.
**Substitute it — never paste one into a shell as it stands.** `<` and `>` are
redirection there, so a verbatim paste runs something other than what was
suggested. Every placeholder is named in its own `description`.

The one `cmd` that is not a krowk command is `share`, which carries the link
itself: no OS agrees on a command for handing a link to a person, so a crumb
that ran `open` would fail everywhere but macOS.

What each command hands back, one line each — the wording lives in the code:

| After | Breadcrumbs |
| --- | --- |
| A keyless `push` | The claim that keeps each upload, **one per file**, plus the link to share; also printed for a person, since the token is shown exactly once |
| `runs start` | The push that feeds the new run, and the `runs finish` that closes it |
| `runs finish` | What the run made — never a second `runs finish` |
| `runs show` | What the run made, plus the finish if it is still open |
| `claim` without `--run` | The `uploads attach` that puts it under one — the only way a claimed upload ever gets a run; `claim --run` has already done it and says nothing |
| A page that came back full | The same listing with `--before`, keeping the `--run` and `--limit` it was given |
| `auth login` / `auth verify` | The push it just made possible, or the `auth verify` an unconfirmed login still needs |

`--quiet` is the raw record with no envelope, so it carries no breadcrumbs at
all — in human output either, where the claim and attach lines are suppressed;
`--format markdown` and `--format url` are the paste forms and are untouched.

## MCP server

`krowk-mcp` is the same thing for agents that cannot shell out — a thin client
over the same `/v1` API, speaking MCP over stdio. No logic lives there that is
not also in the CLI.

```jsonc
// Claude Code: .mcp.json — or `claude mcp add krowk -- krowk-mcp`
{
  "mcpServers": {
    "krowk": { "command": "krowk-mcp", "env": { "KROWK_TOKEN": "krowk_sk_..." } }
  }
}
```

| Tool | What it does |
| --- | --- |
| `krowk_push` | Upload files — one artifact per file, grouped under a run — and get both paste forms back |
| `krowk_list_artifacts` | List the workspace's artifacts, newest first (needs a key) |
| `krowk_get_artifact` | Look one up by its slug |
| `krowk_claim_artifact` | Spend a claim token to keep an anonymous upload, and group it under a run with `run` (needs a key) |
| `krowk_get_run` | Report the repo, commit, branch and agent that will be attached |
| `krowk_verify_key` | Whether a key is configured, and the workspace it acts in |

Every result carries the markdown embed and the bare URL, each labelled with the
surfaces it belongs to, so the agent's job is copy-paste rather than templating.
The machine-readable artifact rides along as `structuredContent`. A failed call
comes back as a tool result with `isError` and the registry's own `fix`, not as a
transport error, so the agent can read the reason and correct the call.

The declare/upload/finalize sequence is not exposed as separate tools: the `PUT`
step needs access to the local file, which the MCP client does not have, so the
sequence stays behind `krowk_push` where it belongs.

**`krowk_push` is confined to a root** — the working directory, or `--root` /
`KROWK_MCP_ROOT`. Paths are resolved with symlinks followed *before* the check,
and anything landing outside is refused. This matters more here than in the CLI:
there a person types the path, here a model picks it, and a model reads
repository files, web pages and issue bodies. An artifact is published at a URL
that needs no credential to read, so without the boundary an instruction hidden
in any of those would turn `krowk_push` into "read any file on this machine and
publish it somewhere I can fetch". Symlink order is the subtle half — a link
inside the repo pointing at `~/.ssh/id_rsa` sails through a prefix test done
before resolving.

Three things a plain prefix check gets wrong, all worth stating because they are
the common case rather than the exotic one:

- **The root may not be the home directory, or `/`.** It defaults to the working
  directory, so an agent started outside a checkout would otherwise take `$HOME`
  as its boundary — and `~/.ssh`, `~/.aws` and `~/.config/krowk` are all inside
  that. A root that broad is refused with a message saying to pass `--root`.
- **Credential files are refused inside the root too.** A checkout is not free of
  secrets: `.env` files live in them, and so do stray keys and service-account
  JSON. `.env*`, `.ssh`, `.aws`, `.gnupg`, `.kube`, `.docker`, `.netrc`, `.npmrc`,
  `.pypirc`, `.git-credentials`, `credentials.json` and `id_rsa` / `id_ed25519` /
  `id_ecdsa` are refused wherever they sit. Nobody publishes these on purpose, so
  the cost of refusing is an error message and the cost of allowing is a secret
  with a permalink.
- **A file with more than one name is refused.** Resolving symlinks catches a link
  pointing out of the root, but a hard link is not a link to anything — it is a
  second name for the same inode, so a path inside the root can be a key outside
  it with nothing to resolve. Nothing reports where the other names are, so a file
  with any is refused; copy it in and push the copy. In practice this rejects
  package-store files and nothing anyone wanted to publish.

The boundary constrains `krowk_push`; it is not a sandbox around the agent. An
agent that can also run a shell can read whatever it likes — this stops the tool
from being the thing that publishes it.

## Metadata

Flags win; everything else is detected so the agent never has to type it.

| Field | Source |
| --- | --- |
| `repo` | `GITHUB_REPOSITORY`, else `git remote get-url origin` |
| `commit` | `GITHUB_SHA`, else `git rev-parse HEAD` |
| `branch` | `git rev-parse --abbrev-ref HEAD` |
| `agent` | `KROWK_AGENT`, else `CLAUDECODE` → `claude-code`, `CURSOR_TRACE_ID` → `cursor`, `GITHUB_ACTIONS` → `github-actions` |
| `pull_request` | `--pull-request`, else derived from `GITHUB_REF` in a PR build |
| `references` (a list, from repeated `--reference`), `session`, `title` | Flags only |

Metadata is recorded on the **run**, because the registry keeps none on an
artifact. A run belongs to a workspace, so it needs an API key:

- **With a key** — `push` opens a run carrying the metadata, attaches every
  artifact to it, and closes it on the way out. `--run` attaches to a run you
  opened yourself instead, and leaves closing it to you.
- **Without a key** — the upload still works. It lands in the shared anonymous
  workspace, expires in 24 hours, and comes back with a claim token. There is no
  run, so metadata named by flag is *not* recorded, and the result says so in
  `notes` rather than dropping it silently.

An upload can join a run afterwards, which is the only way one that started out
anonymous ever gets a run at all: it could not name one when it was created, and
claiming it does not give it one. `krowk claim <artifact> <token> --run <run>`
does both in the order that works — the claim moves the upload into the key's
workspace, and only then can it be attached to a run there. `krowk uploads
attach <artifact> --run <run>` is the same attach on its own, for an upload
already owned. Both are idempotent, and neither moves the link.

## Wire contract

What the registry implements, and what this client talks to. Bytes never pass
through the registry: it hands out a presigned upload URL, the client uploads
straight to object storage, and a later call verifies what landed.

The API is resourceful all the way down, so what would be a verb hanging off an
artifact is a nested resource instead. The method follows from whether the call
can be repeated — finalizing, completing and setting an artifact's run are
idempotent, so they are `PUT`s; claiming spends a one-shot token, so it is a
`POST`. Reading a key is the same rule the other way round: asking what it may do
changes nothing about it, so the key a request is made with is a singular
resource reached with a `GET`:

```
GET        /                                  service descriptor
GET        /v1/key                            the key this request is made with
POST       /v1/cli/authorizations             open a browser login (no key, rate limited)
GET        /v1/cli/authorizations/:slug       poll it — hands the key over exactly once
GET        /v1/artifacts                      list, newest first (needs a key)
POST       /v1/artifacts                      declare an upload
GET        /v1/artifacts/:slug                read one back
DELETE     /v1/artifacts/:slug                take it down (needs a key or its claim token)
POST       /v1/artifacts/:slug/upload         mint its presigned PUT again (needs a key or its claim token)
PUT|PATCH  /v1/artifacts/:slug/finalization   confirm the bytes landed
POST       /v1/artifacts/:slug/claim          spend a claim token (needs a key)
PUT|PATCH  /v1/artifacts/:slug/run            put it under a run (needs a key)
GET        /v1/runs                           list runs, newest first (needs a key)
POST       /v1/runs                           open a run (needs a key)
GET        /v1/runs/:slug                     read one back, metadata and all (needs a key)
GET        /v1/runs/:slug/artifacts           what one run produced (needs a key)
PUT|PATCH  /v1/runs/:slug/completion          close a run (needs a key)
```

Everything but listing, claiming, attaching, the key and the run endpoints works
without a key: for a keyless request the slug *is* the capability, since slugs
are 24 random lowercase base36 characters — lowercase because a slug becomes a
DNS label at `art-{slug}.krowkusercontent.com` — and the bytes are public on the
CDN regardless.

Taking one down is the exception, and it is keyed differently than it looks. A
slug travels in whatever the link was pasted into, so a reader of a link must not
be able to destroy what they read — a keyless takedown is authorised by the claim
token instead. The client sends that token *instead of* the key rather than
alongside it: offered both, the registry reads the key and looks in its
workspace, where an upload still sitting in the anonymous one is simply not
found. Withholding the key is what lets a logged-in machine take down what a CI
job pushed anonymously.

Minting an upload URL again is the same exception, for the same reason. A
presigned `PUT` is not a read: it decides what the bytes behind the link *are*,
so a slug that is public the moment the link is pasted cannot be all it takes.
The claim token is sent the same way and withheld the same way, and it costs the
honest caller nothing — it came back in the same response as the slug and the URL
being replaced.

```
POST {KROWK_API_URL}/artifacts
Authorization: Bearer krowk_sk_...      # optional — anonymous uploads allowed
Content-Type: application/json

{ "artifact": {
    "filename": "foobar.jpg", "content_type": "image/jpeg",
    "byte_size": 421888, "checksum": "<sha-256 hex>", "run": "run_..." } }
```

```jsonc
// 201 Created
{
  "slug": "art_2e1d…", "state": "pending",
  "url": "https://krowk.com/a/art_2e1d",                          // the card page — the link to paste
  "file_url": "https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg", // the bytes on the CDN
  "markdown": "[![foobar.jpg](https://cdn.krowk.com/…)](https://krowk.com/a/art_2e1d)",
  "run": null,                                // or { "slug": "run_…", "metadata": {…}, "created_at": … }
  "expires_at": "2026-08-05T14:32:00Z",       // anonymous uploads only
  "upload": {
    "method": "PUT", "url": "https://…?X-Amz-Signature=…",
    "headers": { "Content-Type": "image/jpeg", "Content-Length": "421888" }
  },
  "next_step": "PUT the file to upload.url …, then PUT /v1/artifacts/art_2e1d/finalization",
  "claim_token": "krowk_claim_…"              // anonymous uploads only, shown once
}
```

Then the two calls `next_step` names:

```
PUT <upload.url>                                  # exactly the headers above, nothing more
PUT {KROWK_API_URL}/artifacts/{slug}/finalization
```

A browser login is two calls, and the split between them is the security
property rather than a shape:

```
POST {KROWK_API_URL}/cli/authorizations       # no Authorization header, no body
```

```jsonc
// 201 Created
{
  "slug": "aut_9k2f…", "state": "pending",
  "code": "7K4M-2QXP",                        // what a person confirms; approve or deny, nothing else
  "verification_url": "https://app.krowk.com/cli/authorizations/new?code=7K4M-2QXP",
  "interval": 5,                              // seconds between polls — the registry sets the pace
  "expires_at": "2026-08-10T14:47:00Z"
}
```

```
GET {KROWK_API_URL}/cli/authorizations/aut_9k2f…    # authorised by the slug, no key
```

```jsonc
// 200 OK, once approved — and only once
{ "slug": "aut_9k2f…", "state": "approved", "expires_at": "…",
  "token": "krowk_sk_…", "key_id": "key_9f3c2e1d", "workspace": "ws_acme" }
// 200 OK while nobody has answered: { "state": "pending" }, or "denied" once someone has
// 410 Gone afterwards: `spent` once the key has been collected, `expired` once the window closes
```

The slug is the capability that collects the key and is never put in front of a
browser; the code is the capability that approves, and knowing it yields nothing.
`POST /v1/cli/authorizations` is the one create in this API that carries no
`Idempotency-Key`: a lost response means the code was never seen, so the
authorization it belongs to can never be approved — it charges nothing, reserves
nothing, and lapses on its own.

Every failure carries the same shape, so an agent can branch on the code:

```json
{ "error": { "code": "invalid",
             "message": "Byte size must be at most 104857600 bytes",
             "details": { "byte_size": ["must be at most 104857600 bytes"] } } }
```

The client flattens that into one map and adds a `fix` line naming the next
thing to actually do, so `krowk`'s own errors and the registry's read alike.

- **Size and digest are declared up front** because they are signed into the
  upload URL. That is what lets object storage refuse an oversized or corrupted
  body at the edge instead of storing it and leaving us to notice later. It is
  also why the client reads the whole file to digest it before the first call.
- **Finalizing and completing are idempotent** — an agent that retries either gets
  the same success, and the record keeps the moment it first reached that state.
- **Listing is cursor-paged** — `next` carries the slug to pass back as `before`,
  and is null on the last page. It is present whenever a page came back full, so a
  total that is an exact multiple of `--limit` costs one extra empty page. Runs and
  a run's artifacts page the same way, off the same cursor.
- **A run's uploads are a collection of the run, not a filter** — `krowk uploads
  list --run <run>` reads `/v1/runs/:slug/artifacts`, so a run that does not exist
  is a `404` from the run itself. A filter would answer an empty page, which no
  caller can tell apart from a run that genuinely produced nothing.
- **Retries** — up to 3 attempts on a retryable failure (429, 5xx,
  `upload_missing`, `storage_unavailable`), honouring `Retry-After` in either
  spelling the HTTP spec allows, capped at 60 seconds so a header cannot wedge
  the CLI. The two calls that create something carry an `Idempotency-Key` naming
  the attempt, so retrying one is free rather than a second record — which is what
  lets opening a run be retried at all.
- **A lapsed signature is re-presigned, never re-declared.** A presigned URL is
  good for 15 minutes while the artifact it belongs to waits for its bytes far
  longer, so an upload can reach storage with a signature that has already gone
  stale — a large file to digest, a slow network, a retry after a crash. When the
  URL's own `expires_at` has passed, or storage answers the `403` it answers a
  signature it will not honour with, the client posts
  `/v1/artifacts/:slug/upload` for a fresh one and sends the bytes again: same
  slug, same storage key, same declared size and digest. Declaring the file again
  would also produce a working URL, and it is the wrong answer — a second declare
  is a second slug, and the first link is already pasted somewhere. It fires on
  failure only, so an upload that lands first time makes no such call. A finalized
  artifact refuses it with `409 already_finalized`: its bytes are a permalink, and
  the way to different bytes is a different artifact.
- **Expiry** — an anonymous artifact past its expiry answers `410 Gone`.
- **Takedown is immediate and unrecoverable** — the bytes leave storage at once
  and what stays behind is a tombstone, so the slug answers `410 taken_down`
  rather than `404`: the link is already pasted somewhere, and its reader deserves
  to know the artifact was removed rather than to go hunting for a typo. It is
  deliberately not routed through a trash or a recovery window, because the case
  it exists for is a secret uploaded by accident, and a secret that can be
  restored is still leaked. Taking down what is already down is a success.
- **Upload targets are storage, not anything reachable.** A presigned URL names a
  foreign host by design, but the client requires `https`, ignores `method` and
  always sends `PUT`, and refuses loopback, link-local, private and carrier-grade
  NAT addresses. Otherwise a response body would choose the method, host, path,
  headers and body of a request the CLI makes from its own network position —
  which in CI reaches a great deal the registry cannot. Two origins are exempt
  because the user chose them: a local registry, where local targets are the
  whole point, and the host `KROWK_API_URL` itself names — a self-hosted
  registry on a private network serves blobs on its own host, and that host is
  configuration, not the registry steering the client.

  The check is on the connection, not only on the URL, because a URL check alone
  has two ways past it. An upload target that answers with a redirect is refused
  outright — a presigned URL is where the bytes belong, and a `302` would change
  the host and arrive as a `GET`, taking the method restriction with it. The one
  exception is an upload URL on the API's own origin, which may redirect within
  it the way the API itself may; the https-downgrade check and the dial check
  still apply to the hop. And the address is judged as it is dialled rather than
  by resolving the name first, since whoever returned the name can answer the
  check and the dial differently.

  "Inside the network" means more than the obvious four shapes: carrier-grade NAT
  (`100.64.0.0/10`, where Tailscale lives), `240.0.0.0/4`, multicast, and the
  NAT64 prefix `64:ff9b::/96`, which on an IPv6-only runner is how you reach the
  metadata service without naming a link-local address at all.

  **The proxy a request goes through is exempt, and only at its own address.**
  Corporate proxies sit on private addresses, so refusing those outright would
  refuse every upload from the environments this tool is for — but the exemption
  is the address of the proxy the transport selected for that request, not any
  proxy configured in the environment and not a switch that turns the boundary
  off. A request that skipped the proxy — `NO_PROXY` covers its host, or its
  scheme names a different variable — is judged like any other, even when its
  dial lands on a configured proxy's address. One thing does stay out of reach:
  where a proxy sends the bytes is the proxy's business, so a name that resolves
  differently for it than for the URL check is not something this client can see.

### Retries dedupe, identical pushes do not

Pushing the same bytes twice creates **two artifacts with two links**. An
artifact's identity is a random slug, never a digest of its contents. That is
settled rather than pending: a digest slug would make an anonymous link guessable,
and merging on content alone would join two people who happened to upload the same
screenshot. The earlier draft of this CLI promised digest-derived IDs and one
stable link per file; it does not work that way and will not.

**A retry is the exception.** Each of the two calls that create something — opening
a run, declaring an artifact — carries an `Idempotency-Key` naming that attempt, and
every retry of the call repeats it, so the registry answers with the record the
first attempt already made. An agent whose push fails halfway no longer leaves the
first attempt's artifacts behind to expire, and eleven retries cost one upload
rather than eleven.

One key per call rather than per `push`: the registry matches a key against the
payload it was first used with, so one key spread across three files would have the
second file refused as a reuse. Re-running `krowk push` yourself mints fresh keys,
because that is a new attempt rather than a retry of the old one.

On a keyless push the key is a **credential**. With no API key there is nothing else
a retry can present to prove it made the original call, so the registry scopes keys
by the address they came from — and this client sends a random UUID for exactly that
reason. A predictable one would let anyone sharing that address, a NAT or a CI
runner pool, replay your declare and be handed a URL to write its bytes.

## Testing against a local registry

The CLI ships the registry it develops against, so trying it out needs neither
the network nor a key. In one terminal:

```bash
krowk registry serve                 # listens on 127.0.0.1:8787
```

And in another, `--dev` points the CLI at it:

```bash
krowk push screenshot.png --dev
krowk doctor --dev                   # says registry: local
```

`KROWK_DEV=1` does the same thing without the flag, which is how to point the MCP
server at it too. Precedence, most to least specific: `--dev`, then
`KROWK_API_URL`, then `KROWK_DEV`, then the public registry.

`registry serve` takes `--addr`, `--site` (the origin baked into returned links,
for demoing production-looking URLs) and `--limit-bytes` (to exercise the
too-large path). It serves the card page at `/a/{slug}` too — the same
OpenGraph tags production serves, unstyled — so a link it hands out unfurls
rather than 404ing when it is pasted somewhere.

It answers the browser login as well, so `krowk auth login --dev` works end to
end:

```bash
krowk auth login --dev                # opens the approval page, prints the code
```

The approval screen belongs to the app surface in production. What the stand-in
serves is an unmistakably local substitute — a code to compare and two buttons,
at `/_approve/cli/authorizations/new?code=…`, on a path that is neither the API
nor a pretence at the website. Pressing Approve mints a key derived exactly as
`GET /v1/key` derives one, so the login and the `auth verify` after it agree. A
script can press it without a browser:

```bash
curl -X POST http://localhost:8787/_approve/cli/authorizations/7K4M-2QXP/approval
curl -X POST http://localhost:8787/_approve/cli/authorizations/7K4M-2QXP/denial
```

It binds `127.0.0.1:8787` — **loopback by default**, because it accepts uploads
without a key and serves their bytes to anyone who can reach it. On a café or
office network a wider bind hands that to whoever is nearby. `--addr` can still
open it up, and the banner says so when you do.

## Development

```bash
make check          # go vet + go test ./...
make build          # → bin/krowk and bin/krowk-mcp, versions stamped from git describe
make mock           # the same registry as `krowk registry serve`, from the checkout
make install        # → $GOPATH/bin/{krowk,krowk-mcp}
make release-check  # validate .goreleaser.yaml and the npm launchers
make dist           # the whole release, locally, publishing nothing
```

`mise.toml` pins Go for the first four and GoReleaser and Node for the last two.
Nothing here needs a network beyond fetching those.

Against the real registry, with [krowk-registry](../krowk-registry) up on port
3000 — note the hostname, because that app routes by it:

```bash
export KROWK_API_URL=http://api.krowk.localhost:3000/v1
./bin/krowk doctor
./bin/krowk push screenshot.png                      # anonymous, expires in 24h

# For the keyed flow, mint a key in the registry checkout:
#   bin/rails runner 'puts Workspace.find_or_create_by!(name: "Local Dev") \
#     .keys.create!(name: "krowk-cli local").token'
export KROWK_TOKEN=krowk_sk_...
./bin/krowk push screenshot.png --pull-request=https://github.com/acme/x/pull/1
```

```
cmd/krowk               the binary
cmd/krowk-mcp           the MCP server
internal/cli            flag parsing, routing, commands
internal/api            the three-step upload, runs, retries, credentials
internal/runctx         git and CI metadata detection
internal/output         human / json / markdown / url rendering
internal/mcp            MCP over stdio, wrapping the same api client
internal/registry       the stand-in registry handler, shared with the tests
```

`internal/registry` is an in-memory stand-in for `api.krowk.com` that keeps the
test suite hermetic — no Postgres, no object storage, no Rails. It implements the
same sequence, the same error envelope and the same refusals the real registry
makes, including the ones that exist to catch a broken client: it will not
finalize bytes that never arrived, and it rejects a body whose length or digest
is not what was declared. The one thing it fakes is signing — object storage is
the same process on a `/_storage` path, with an opaque token in place of SigV4.

A stand-in can drift with the client and keep the suite green while the real
registry answers 404, so `TestWireShapeMatchesTheRegistrysRoutes` pins the method
and path of every call the CLI makes. Check it against `bin/rails routes` in the
registry checkout when either side moves.

Environment: `KROWK_TOKEN`, `KROWK_API_URL`, `KROWK_AGENT`.

## Releasing

A tag is the whole trigger.

```bash
git tag -a v0.1.0 -m "First release"
git push origin v0.1.0
```

`.github/workflows/release.yml` then runs `make check`, hands
[`.goreleaser.yaml`](.goreleaser.yaml) to GoReleaser, and publishes to npm. Out
of it come:

- **Five archives**, `krowk_0.1.0_{linux,darwin}_{amd64,arm64}.tar.gz` and
  `krowk_0.1.0_windows_amd64.zip`, each holding both `krowk` and `krowk-mcp` —
  static (`CGO_ENABLED=0`), trimmed, and stamped with the same
  `internal/cli.Version` ldflag the Makefile uses. Predictable names, because a
  script that constructs the URL should not have to scrape the release page.
- **`checksums.txt`**, sha256 over every archive.
- **A GitHub Release**, with the commit subjects since the last tag. A tag with a
  `-rc1` on it becomes a pre-release automatically.
- **Seven npm packages**: `@krowk/cli-{linux,darwin}-{x64,arm64}` and
  `@krowk/cli-win32-x64`, each carrying nothing but that platform's two binaries,
  and the two names people actually type — `@krowk/cli` and `@krowk/mcp` — which
  are launcher scripts depending on all five as `optionalDependencies`. The
  platform packages are named for the CLI even though `@krowk/mcp` depends on
  them too: one package per platform carries both binaries, rather than ten
  packages carrying one each.

The version the binaries report, the archive names and the npm version are one
string, taken from the tag with the `v` removed. `make build` strips it the same
way, so a checkout and a release never disagree about what version this is.

`npm/build.mjs` reads the binaries out of `dist/` rather than compiling its own,
so the bytes on npm are the bytes the checksums cover. It also holds the one
platform table the rest is checked against: add a platform to `.goreleaser.yaml`
and forget one of the two launchers, and the build stops rather than shipping a
launcher that cannot find its binary.

**Why optionalDependencies and not a postinstall download.** The binary has to
arrive through the npm registry the machine is already pointed at — one host, the
one already configured, already mirrored, already allowed through the proxy. A
postinstall script fetching from GitHub Releases adds a second host that CI
firewalls block, that `npm ci --ignore-scripts` disables outright, and whose
bytes npm's own `integrity` hashes do not cover. The cost is five more published
packages per release and about 6 MB per platform. Worth it: an upload tool whose
install fails behind a corporate proxy has failed for exactly the boring reason
the whole thing exists to avoid.

`.github/workflows/packaging.yml` runs the same build on any pull request that
touches the packaging — config check, snapshot build, npm assembly, then it
installs the packed tarballs and runs both binaries through the launchers. No
secrets, so it stays green whether or not anything is claimed on npm.

### What a human still has to do

1. **Claim the names on npm** — the `@krowk` scope, which covers `@krowk/cli`,
   `@krowk/mcp` and the five platform packages. The unscoped `krowk` is not
   available: npm's typosquat filter permanently refuses it as too similar to the
   existing `growl` package, so nobody should spend another release trying.
2. **Add `NPM_TOKEN`** as a repository secret: an automation token with publish
   rights on the scope. Until it exists, the publish step skips with a notice on the
   run and the GitHub Release happens anyway, so a tag pushed today still
   produces working binaries.

Nothing else is wired to a secret. `GITHUB_TOKEN` is the one Actions provides.

## What the POC still needs

Blocking, in order:

1. **A Slack app.** The card page is done: `url` is `krowk.com/a/{slug}`, which
   serves the OpenGraph tags and a rendered preview, and the stand-in registry
   serves the same page so a local `krowk registry serve` paste behaves like
   production. What is left of the unfurl layer is Slack's own path, which
   needs an app rather than tags on a page.
2. **Distribution.** The pipeline is built — see [Releasing](#releasing). What is
   left is not code: the `@krowk` scope has to carry `@krowk/cli`, `@krowk/mcp`
   and the five platform packages before the site's `npx @krowk/cli push`
   resolves. Binaries publish today.
3. **Getting a token.** The CLI half of the browser login is built — `krowk auth
   login` opens an authorization, prints the code, opens the page and collects the
   key, and the stand-in registry answers all of it. What is left is the registry
   half: `POST /v1/cli/authorizations`, `GET /v1/cli/authorizations/:slug` with
   its one-shot delivery and its sweep, and the approval page on the app surface.
   Until those ship, `--token` is still the only way in against
   `api.krowk.com`.

Non-blocking, in rough value order: a Homebrew tap, which comes off the archives
the release already produces; a Claude Code `PostToolUse` hook so screenshots
upload with no prompting; a GitHub Action. The `curl | bash` installer is
written — it has never run against a real release, because there is not one.

## Open questions

- ~~**Nothing dedupes.** Should the registry key artifacts by digest within a
  workspace, or is a link per push the intended behaviour?~~ **Answered:** a link
  per push, and retries named with an `Idempotency-Key` — not digests. See
  [above](#retries-dedupe-identical-pushes-do-not), and canon,
  `glossary.md` → Identical bytes do not dedupe.
- **A run per push.** With a key, every `push` that is not given `--run` opens
  and closes its own run, so ten screenshots from one agent session become ten
  runs. Should the CLI persist a run for the session — keyed on the agent's
  session ID — so they group without the caller threading `--run` through?
- **Oversized files are read before they are refused.** The upload cap lives in
  the registry, so the client digests a 2 GB file and only then hears it is too
  large. Should the API root publish `max_upload_bytes` so the client can refuse
  it locally?
- **Anonymous uploads have no key to rate-limit against.** IP, or a
  machine-scoped anonymous token minted on first push?

MIT.
