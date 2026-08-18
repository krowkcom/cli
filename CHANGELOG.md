# Changelog

What changed in each release of the krowk CLI, for the people upgrading rather
than for the people who wrote it. Newest first.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
the versions are the `v*` tags a release is cut from. Entries land under
`[Unreleased]` as the work merges, and move under a version when it is tagged.

## [Unreleased]

### Added

- `--jq '<expression>'` filters a result inside krowk, with jq compiled in — no
  jq binary to install and no pipe to build. It works on every command, implies
  `--json`, and reads what the command rendered: the envelope normally, the bare
  record under `--quiet`. A string result prints without its quotes, so
  `URL=$(krowk push shot.png --jq '.data.artifacts[0].url')` is the whole
  ceremony; anything else prints as JSON, one value per line. `krowk help --json`
  is filterable too, which is the shortest way for an agent to read the surface.
  A failure is filtered like any other result, in whatever shape the command
  rendered it: `--jq '.error.error'` reads the code out of an envelope, and
  `--quiet --jq '.error'` reads it out of the bare body. An expression that does
  not parse is refused as `bad_jq` before the command sends anything, and one
  that does not fit the result it was pointed at answers `jq_failed` afterwards,
  saying that the command itself succeeded so that a wrapper retrying on a
  non-zero exit does not repeat the work. A failure `--jq` caused is always
  reported whole, since filtering the complaint with the expression behind it
  would bury it. `auth token`, `registry serve` and `--version` print no JSON,
  and refuse the flag rather than ignore it — the surface says which commands
  those are, under `no_json`.
- Every command that names a record now takes the link as readily as the slug.
  Paste the card page, `https://krowk.com/a/art_…`, the CDN URL under it, or the
  markdown line carrying both, into `uploads show`, `uploads attach`,
  `uploads delete`, `claim`, `runs show`, `runs finish` or `--run` — the slug is
  read out of it, and the MCP tools take one the same way. Anything that is not
  link-shaped is passed on untouched, so slugs behave exactly as they did.
- A link carrying no slug of the kind the command wants now fails as
  `bad_artifact` or `bad_run` (exit 1) before anything is sent, instead of going
  out and coming back as a record that does not exist. A card link handed to
  `runs show` names the artifact it carries, and a link carrying two different
  artifacts is refused rather than acted on — the takedown has no undo.

### Changed

- A `--format` nobody has heard of is now refused even when `--json` or `--jq`
  was passed as well. It used to be accepted and ignored, so a caller who meant
  `--format markdown` and mistyped it was told nothing.
- A claim token is trimmed before it is sent, on `claim` and on
  `uploads delete`, so one copied with a trailing space or newline works instead
  of failing as an unauthorised claim.
- A blank record where one is required is now the command's own missing-argument
  failure rather than a request. `--run "  "` — a shell expanding an unset
  variable — no longer reads as "no run at all", where it used to open a fresh
  run on a push and widen `uploads list` to the whole workspace. A blank
  artifact answers `no_artifact` (exit 1) on both the CLI and the MCP server,
  which previously answered `missing_claim` (exit 3, a credential to fix).
- Failures about a pasted value no longer quote it back. A URL is where
  credentials travel, and a refusal is written to stderr and into the JSON
  envelope.

[Unreleased]: https://github.com/krowkcom/cli/compare/v0.4.1...HEAD
