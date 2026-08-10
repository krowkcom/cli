---
name: krowk
description: |
  Publish agent output — screenshots, logs, diffs, build artifacts — to a permalink
  with the krowk CLI, and paste that link where a person will read it. Uploading
  needs no account; an API key adds runs, listings and permanence.
  Use for ANY request to share, upload, host or link a local file produced during a session.
triggers:
  # Direct invocations
  - krowk
  - /krowk
  - krowk push
  - krowk runs
  - krowk claim
  - krowk uploads
  - krowk doctor
  # What the person actually asks for
  - push a screenshot
  - upload artifact
  - upload a screenshot
  - share this screenshot
  - share agent output
  - get a permalink
  - permalink for this file
  - link to this file
  - host this image
  - attach this to the PR
  - put this in the PR comment
  - show me the screenshot in Slack
  - paste this into Linear
  - group these uploads
  - take this down
  - delete the upload
  # Domains
  - krowk.com
  - cdn.krowk.com
invocable: true
argument-hint: "[push <file> | runs start | claim <artifact> <token> | doctor]"
---

# /krowk — permalinks for agent output

krowk turns a local file into a URL that unfurls in GitHub, Slack, Basecamp and
Linear. One file, one artifact, one link. That link is a card page —
`krowk.com/a/{slug}` — carrying the file and its run metadata; the bytes on the
CDN are `file_url`, which only an image embed should ever name. What groups
several links together is a **run**, not an artifact.

Seventeen commands, one JSON envelope, and a surface you can read as data:
`krowk help --json` is the whole thing — every command, argument, flag and
environment variable — so never guess at a spelling this file does not carry.

## Agent Invariants

**MUST follow these rules.**

1. **Parse the JSON, not the prose.** Pass `--json` on every call whose result
   you intend to read. Output is human on a TTY and JSON when piped, so a
   captured stdout is usually JSON already — but "usually" is not a contract, and
   `--json` is.
2. **One envelope, every command.** Success is
   `{"ok": true, "data": …, "paste": …, "summary": …, "breadcrumbs": […]}`.
   Failure is the same shape with `"ok": false` and an `error` object. Read
   `data` for the result and `summary` for the one-line version to show a person.
3. **Follow the breadcrumbs.** `breadcrumbs[]` is the work that is left, as
   commands rather than hints: `cmd` is ready to run with this result's own slugs
   and tokens already substituted. Prefer running a breadcrumb over composing the
   same call yourself.
4. **Never paste a `<placeholder>` into a shell.** A `cmd` containing `<run>` or
   `<file>` is a value krowk genuinely does not have. Substitute it. `<` and `>`
   are redirection, so a verbatim paste runs something other than what was
   suggested. Each placeholder is explained in that breadcrumb's `description`.
5. **A claim token is a bearer secret, and it is shown once.** `claim_token`
   comes back from a keyless push and is the only authority over that artifact —
   whoever holds it can keep it, re-upload over it, or destroy it. Never write one
   into a PR comment, a commit message, a log line, a chat message or a file the
   repository tracks. Hand it to `krowk claim` and let it be spent.
6. **Keyless works right now; runs need a key.** With no key an artifact lands in
   the shared anonymous workspace, expires in 24 hours, and has no run — metadata
   flags are *not* recorded, and `data.notes` says so. Do not invent a login step
   the task did not ask for: push first, and mention the key only when the person
   wants the link to outlive the day or wants the artifacts grouped.
7. **Paste the right form for the destination.** `paste.markdown` for GitHub,
   Linear and Notion — those render an image URL inline and a third-party link as
   a plain anchor, so an image comes as `[![name](file_url)](url)`: it renders,
   and it clicks through to the card. `paste.url`, bare, for Slack and Basecamp —
   both unfurl the card into a preview, and Slack renders no markdown image
   embeds at all. Never assemble either form yourself; both come back ready.
8. **Push what the person asked for, not what is nearby.** An artifact is public
   to anyone with the link. Never push `.env` files, key material, credential
   JSON or anything under `.ssh`/`.aws`, and never push a file you found rather
   than made.

## Quick Reference

| Task | Command |
|------|---------|
| Upload one file, get a link | `krowk push shot.png --json` |
| Upload several, one run | `krowk push a.png b.log --json` |
| Upload with PR context | `krowk push shot.png --pull-request https://github.com/acme/app/pull/12 --json` |
| Label the markdown link | `krowk push shot.png --title "Cart after the fix" --json` |
| Markdown to paste in a PR | `krowk push shot.png --format markdown` |
| Bare URL to paste in Slack | `krowk push shot.png --format url` |
| Open a run to group under | `krowk runs start --session "$SESSION" --json` |
| Push into an open run | `krowk push shot.png --run run_8Kd2wq --json` |
| Close a run | `krowk runs finish run_8Kd2wq --json` |
| What a run produced | `krowk runs show run_8Kd2wq --json` |
| Recent runs | `krowk runs list --limit 10 --json` |
| Recent artifacts | `krowk uploads list --limit 10 --json` |
| One run's artifacts | `krowk uploads list --run run_8Kd2wq --json` |
| Next page of a listing | `krowk uploads list --before <next> --json` |
| Read one artifact back | `krowk uploads show art_2e1d --json` |
| Keep an anonymous artifact | `krowk claim art_2e1d <claim-token> --json` |
| Keep it and group it at once | `krowk claim art_2e1d <claim-token> --run run_8Kd2wq --json` |
| Group an artifact afterwards | `krowk uploads attach art_2e1d --run run_8Kd2wq --json` |
| Take an artifact down | `krowk uploads delete art_2e1d --json` |
| Take down a keyless artifact | `krowk uploads delete art_2e1d <claim-token> --json` |
| Sign in with nobody's key to paste | `krowk auth login --no-browser --json` |
| Store an API key you were handed | `krowk auth login --token krowk_sk_… --json` |
| Which key is this, whose workspace | `krowk auth verify --json` |
| Diagnose a failure | `krowk doctor --json` |
| The whole surface, as data | `krowk help --json` |
| One command's surface | `krowk help uploads attach --json` |

Global flags on every command: `--json` (or `--format json`), `--format`
(`human`, `json`, `markdown`, `url`), `--quiet` (the raw record, no envelope and
therefore no breadcrumbs), `--dev`, `--help`, `--version`.

## Decision Trees

### Is there a key?

```
krowk auth verify --json
├── ok:true  → keyed. Artifacts keep, group under runs, and are listable.
│              Use runs for anything producing more than one file.
└── ok:false, exit 3 → keyless. Push still works:
                 • the link is live immediately
                 • it expires in 24h unless claimed
                 • data.artifacts[].claim_token is the only way back to it
                 • no run, so --pull-request/--session/--title metadata is
                   dropped and reported in data.notes
               Say this to the person; do not stop and ask them to sign up.
```

### One file or several?

```
One file, no session context   → krowk push shot.png --json          (done)
Several files, one moment      → krowk push a.png b.png --json       (one run, closed for you)
Files produced over a session  → krowk runs start --json
                                 krowk push … --run <run> --json     (repeat)
                                 krowk runs finish <run> --json
```

`push` with no `--run` and a key opens a run, attaches everything, and closes it
on the way out. `--run` names a run you opened, and leaves closing it to you.

### An artifact has a claim token and needs to survive

```
Do you now have a key?
├── no  → krowk auth login --no-browser --json   (a person approves it; then continue)
│         or krowk auth login --token krowk_sk_… --json if you were handed one
└── yes → does it belong under a run?
          ├── no  → krowk claim <artifact> <token> --json
          └── yes → krowk claim <artifact> <token> --run <run> --json
```

Claiming moves the artifact into the key's workspace and does **not** move the
link. A claim without `--run` leaves the artifact with no run at all — that is what
the `krowk uploads attach` breadcrumb in its result is for.

### Something must come down now

```
Pushed a secret, or the wrong file?
├── the artifact is in your workspace → krowk uploads delete <artifact> --json
└── it was pushed keylessly           → krowk uploads delete <artifact> <claim-token> --json
```

Immediate and irreversible: the bytes go at once. Withhold the key when using a
claim token — a keyless artifact sits in the anonymous workspace, so a request that
carries a key looks there instead and finds nothing. Then rotate whatever was in
the file: it was readable at a public URL for as long as it was up.

## Common Workflows

### Push a screenshot into a PR comment

```bash
krowk push shot.png \
  --pull-request https://github.com/acme/storefront/pull/412 \
  --title "Cart total after the fix" --json
```

Read `paste.markdown` from the envelope and put that line in the comment body —
it is an image embed wrapped in a link to the card, so the screenshot renders
inline and clicking it lands on the page with the run metadata.
`data.artifacts[0].url` is that card page bare, for Slack.

### A whole session's output under one run

```bash
krowk runs start --session "$CLAUDE_SESSION_ID" \
  --pull-request https://github.com/acme/storefront/pull/412 --json
# → data.slug, e.g. run_8Kd2wq

krowk push before.png --run run_8Kd2wq --json
krowk push after.png  --run run_8Kd2wq --json
krowk push build.log  --run run_8Kd2wq --json

krowk runs finish run_8Kd2wq --json
```

Metadata is recorded on the run, once, when it is opened — repo, commit, branch
and agent are detected, so pass only what detection cannot know. Afterwards,
`krowk runs show run_8Kd2wq --json` lists everything the run produced.

### Sign in when there is no key to paste

```bash
krowk auth login --no-browser --json
```

It blocks until a person approves it, up to a quarter of an hour. While it waits,
the code and the page go to **stderr** — capture both streams, show the person
both, and say that the code on the page has to match the one you printed. stdout
stays one JSON document: the receipt, once the key is stored. Drop `--no-browser`
where the browser on this machine is that person's own; over SSH, or with no
display, it is what happens anyway.

The key itself is not yours to read. It lands in the credentials file at 0600 and
every later command finds it there.

### Keep an artifact that was pushed before signing in

```bash
krowk push diagram.png --json
# → data.artifacts[0].claim_token — treat as a secret, never echo it

krowk auth login --token krowk_sk_… --json
krowk claim art_2e1d "$CLAIM_TOKEN" --run run_8Kd2wq --json
```

The token is spent by the claim and is worthless afterwards, which is the whole
reason it can be handed over: one shot, then gone. The link does not change, so
anything already pasted keeps working.

### Take down something that should never have been pushed

```bash
krowk uploads delete art_2e1d --json                    # yours
krowk uploads delete art_2e1d "$CLAIM_TOKEN" --json     # pushed keylessly
```

Do this before anything else — before reporting it, before cleaning up the
repository. Then tell the person exactly what was exposed, and rotate it.

## Environment

| Variable | Does |
|----------|------|
| `KROWK_TOKEN` | API key, e.g. `krowk_sk_…`. Outranks the stored credentials file |
| `KROWK_API_URL` | Base URL of the registry. Default `https://api.krowk.com/v1` |
| `KROWK_AGENT` | Agent name recorded on runs. Detected otherwise |
| `KROWK_DEV` | `1`/`true`/`yes`/`on` — same as `--dev`, a local registry |

A stored key lives at `~/.config/krowk/credentials.json`, mode 0600, written by
`krowk auth login`. Never read it out to show a person, and never put a token on
a command line in a shared shell — `KROWK_TOKEN` in the environment is better.

## Error Handling

**Read `error.fix` first.** Every failure carries it, and it names the one thing
to change. Repeating the same call after a non-retryable error gets the same
answer.

```json
{"ok": false,
 "error": {"error": "run_needs_key", "status": 401, "retryable": false,
           "fix": "krowk auth login --token krowk_sk_…, then push again"}}
```

- `error.retryable` says whether trying again can work at all. When it is false,
  change the command.
- **429:** `error.retry_after` carries the registry's own Retry-After in seconds
  when it sent one. Wait that long — not less — then retry once. Do not open a
  parallel push to get around it.
- **Anything unexplained:** `krowk doctor --json` reports the version, whether
  the registry is reachable, whether a key is configured and what run context was
  detected. Run it before reporting a bug, and include its output.
- **A link that 404s or is gone:** the artifact expired (keyless, 24 hours) or was
  taken down. Exit 8. No retry brings it back; push again.
- **`krowk help <command> --json`** settles any question about a flag, and cannot
  be out of date — it is generated from the same catalog that routes the command.

## Exit Codes

| Exit | Means | What to do |
|------|-------|------------|
| 0 | It worked | — |
| 1 | The command was wrong, or krowk failed on its own — bad flag, unknown command, unreadable file. Also anything unclassified | Fix the command; `krowk help <command> --json` |
| 2 | Not found — no such artifact or run in this workspace, or an unrecognised claim token | Check the slug and the token, or `KROWK_API_URL` |
| 3 | Refused for want of credentials — no key where one is needed, a rejected key, or no claim token where that is the only authority | `krowk auth login --token …`, or pass the claim token |
| 4 | Understood and refused — validation, an artifact already finalized, a run that needs a key | Change something; retrying unchanged answers the same |
| 5 | Rate limited | Wait `error.retry_after` seconds, then retry |
| 6 | The bytes did not move — registry or object storage unreachable | Retry; `krowk doctor --json` |
| 7 | The registry failed on its side, or answered something this client could not read | Retry, and report it if it persists |
| 8 | Gone — expired or taken down | Push again; nothing brings it back |

1 stays the catch-all, so a check for `!= 0` keeps working and a failure that
gains a class can only move *out* of 1.

## Learn More

- CLI repo, wire contract and the MCP server: https://github.com/krowkcom/cli
- The surface as data, always current: `krowk help --json`
