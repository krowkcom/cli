# krowk

Permalinks for agent output. Push a screenshot, get a URL that unfurls in
GitHub, Slack, Basecamp and Linear with the run metadata attached.

Proof of concept. The CLI speaks the registry's real protocol and is tested
against both a local stand-in and a running
[krowk-registry](../krowk-registry); what is still missing is the unfurl layer
and distribution (see [What the POC still needs](#what-the-poc-still-needs)).

```bash
krowk push ../../tmp/screenshots/foobar.jpg \
  --pull-request="https://github.com/acme/storefront/pull/412" \
  --reference="https://linear.app/acme/issue/ENG-9" \
  --session="3fe6808d-088d-4a6f-a04c-cc9690bcf852"
```

```
✓ uploaded  foobar.jpg  412 KB
  https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg
  run run_8Kd2wq · expires in 24h
```

Go, standard library only. One static binary, no runtime to install — agent
containers rarely have Node, and the whole point is that the upload step never
fails for boring reasons.

## Install

```bash
go install github.com/krowkcom/cli/cmd/krowk@latest
```

Release binaries, a Homebrew tap and the `npx krowk` wrapper the website
advertises all land with the first tagged release — see the prerequisites below.

## Commands

| Command | What it does |
| --- | --- |
| `krowk push <file...>` | Upload files, get a link for each |
| `krowk uploads create <file...>` | The same thing, spelled out |
| `krowk uploads list` | List the workspace's uploads, newest first (`--limit`, `--before`) |
| `krowk uploads show <artifact>` | Read one artifact back |
| `krowk uploads attach <artifact> --run <run>` | Put an upload under a run after it was uploaded |
| `krowk runs start` | Open a run to group later uploads under |
| `krowk runs finish <run>` | Close a run |
| `krowk claim <artifact> <claim-token>` | Keep an anonymous upload past its expiry (`--run` groups it while claiming) |
| `krowk auth login --token <token>` | Check a token against the registry, then store it in `~/.config/krowk/credentials.json` (0600) |
| `krowk auth token` | Print the stored token, for scripts |
| `krowk auth verify` | Ask the registry which key this is, and the workspace it acts in |
| `krowk doctor` | Report version, API reachability, auth and detected run context |
| `krowk registry serve` | Run the local stand-in registry to develop against |

Upload flags: `--run`, `--pull-request`, `--reference` (repeatable), `--session`,
`--title`, plus `--repo` / `--commit` / `--agent` to override detection. Flags
may follow the filenames.

Global flags: `--dev`, `--format human|json|markdown|url`, `--json`, `--quiet`,
`--help`, `--version`. Output is human on a TTY and JSON when piped, so an agent
that captures stdout gets structured data without asking for it.

**One file, one artifact, one link.** Pushing three files creates three
artifacts with three URLs. What groups them is a run, not an artifact.

**Logging in asks first.** `auth login` reads the key back from the registry
before writing it down, so a mistyped token fails while the real one is still on
the clipboard rather than at the next push. A key the registry rejects is not
stored, and never replaces a working one; a registry that cannot be reached is
not evidence about the key, so the token is stored unconfirmed and `auth verify`
settles it later. What came back — the key ID and the workspace — is kept
alongside the token, which is how `doctor` names the workspace an upload would
land in without a round trip. `KROWK_TOKEN` outranks that file, so when it is
set the recorded workspace describes a different key and is withheld rather than
reported.

## Pasting the result

There is no single paste-ready string, because the surfaces disagree.

| Destination | Use | Why |
| --- | --- | --- |
| GitHub, Linear, Notion | `--format markdown` | GitHub builds preview cards only for its own resources, so a bare third-party link renders as a plain blue anchor no matter what OpenGraph tags the page carries. It *does* render image URLs inline, so the image embed is what actually shows the artifact in a PR comment. |
| Slack, Basecamp | `--format url` | Both unfurl a bare URL into a card of their own. Slack renders no markdown image embeds at all — pasting the embed form there shows raw text. |

`--json` carries both forms under `paste`, one line per artifact:

```json
{
  "ok": true,
  "data": { },
  "paste": {
    "markdown": "![foobar.jpg](https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg)",
    "url": "https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg"
  }
}
```

Link labels — the title or the filename — are escaped for CommonMark, so
`frame[0].png` pastes as a working link rather than a broken one.

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
| `reference`, `session`, `title` | Flags only |

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
can be repeated — finalizing, completing and setting the run an artifact belongs
to are idempotent, so they are `PUT`s;
claiming spends a one-shot token, so it is a `POST`. Reading a key is the same
rule the other way round: asking what it may do changes nothing about it, so the
key a request is made with is a singular resource reached with a `GET`:

```
GET        /                                  service descriptor
GET        /v1/key                            the key this request is made with
GET        /v1/artifacts                      list, newest first (needs a key)
POST       /v1/artifacts                      declare an upload
GET        /v1/artifacts/:slug                read one back
PUT|PATCH  /v1/artifacts/:slug/finalization   confirm the bytes landed
POST       /v1/artifacts/:slug/claim          spend a claim token (needs a key)
PUT|PATCH  /v1/artifacts/:slug/run            put it under a run (needs a key)
POST       /v1/runs                           open a run (needs a key)
PUT|PATCH  /v1/runs/:slug/completion          close a run (needs a key)
```

Everything but listing, claiming, attaching, the key and the run endpoints works
without a key: for a keyless request the slug *is* the capability, since slugs
are 21 random base58 characters and the bytes are public on the CDN regardless.

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
  "url": "https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg",
  "markdown": "![foobar.jpg](https://cdn.krowk.com/…)",
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
  total that is an exact multiple of `--limit` costs one extra empty page.
- **Retries** — up to 3 attempts on a retryable failure (429, 5xx,
  `upload_missing`, `storage_unavailable`), honouring `Retry-After` in either
  spelling the HTTP spec allows, capped at 60 seconds so a header cannot wedge
  the CLI.
- **Expiry** — an anonymous artifact past its expiry answers `410 Gone`.
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

### Not idempotent across pushes

Pushing the same bytes twice creates **two artifacts with two links**. An
artifact's identity is a random slug, not a digest of its contents, so the
registry cannot tell a retry from a second upload. The earlier draft of this CLI
promised digest-derived IDs and one stable link per file; the registry does not
work that way, and this client no longer claims it does. An agent that retries a
whole `push` after a partial failure leaves the first attempt's artifacts behind
to expire.

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
too-large path).

It binds `127.0.0.1:8787` — **loopback by default**, because it accepts uploads
without a key and serves their bytes to anyone who can reach it. On a café or
office network a wider bind hands that to whoever is nearby. `--addr` can still
open it up, and the banner says so when you do.

## Development

```bash
make check       # go vet + go test ./...
make build       # → bin/krowk and bin/krowk-mcp, versions stamped from git describe
make mock        # the same registry as `krowk registry serve`, from the checkout
make install     # → $GOPATH/bin/{krowk,krowk-mcp}
```

Against the real registry, with [krowk-registry](../krowk-registry) up on port
3000 — note the hostname, because that app routes by it:

```bash
export KROWK_API_URL=http://api.krowk.localhost:3000/v1
./bin/krowk doctor
./bin/krowk push screenshot.png                      # anonymous, expires in 24h

# For the keyed flow, mint a key in the registry checkout:
#   bin/rails runner 'puts Workspace.find_or_create_by!(name: "Local Dev") \
#     .api_keys.create!(name: "krowk-cli local").token'
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

## What the POC still needs

Blocking, in order:

1. **The unfurl layer.** The whole pitch is the preview card. Today `url` points
   straight at the CDN object, so pasting it gets you an image or a file
   download, not a card with the run metadata on it. That means a
   `krowk.com/a/{slug}` page serving OpenGraph tags plus a rendered preview, and
   a Slack app for Slack's own unfurl path.
2. **Distribution.** `krowk` and `@krowk/mcp` are unclaimed on npm. The site
   tells people to run `npx krowk push`, so the first release needs GoReleaser
   for the binaries plus a thin npm wrapper that pulls the right one through
   per-platform `optionalDependencies` — the pattern esbuild uses. A Homebrew
   tap and `curl | bash` come off the same build.
3. **Getting a token.** Keys exist in the registry and the dashboard can issue
   them, but there is no device flow, so an agent in a container still needs a
   human to paste one in.

Non-blocking, in rough value order: a Claude Code `PostToolUse` hook so
screenshots upload with no prompting; a GitHub Action. The MCP server itself
already ships as `krowk-mcp`; what `@krowk/mcp` still needs is the npm wrapper.

## Open questions

- **Nothing dedupes.** See [above](#not-idempotent-across-pushes). Should the
  registry key artifacts by digest within a workspace, or is a link per push the
  intended behaviour?
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
