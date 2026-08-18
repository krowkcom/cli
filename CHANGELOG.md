# Changelog

What changed in each release of the krowk CLI, for the people upgrading rather
than for the people who wrote it. Newest first.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
the versions are the `v*` tags a release is cut from. Entries land under
`[Unreleased]` as the work merges, and move under a version when it is tagged.

## [Unreleased]

### Added

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

[Unreleased]: https://github.com/krowkcom/cli/compare/v0.4.1...HEAD
