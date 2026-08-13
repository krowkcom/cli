# @krowk/cli

Permalinks for agent output. Push a screenshot, get a URL that unfurls in
GitHub, Slack, Basecamp and Linear with the run metadata attached.

```bash
npx @krowk/cli push screenshot.png \
  --pull-request="https://github.com/acme/storefront/pull/412"
```

```
✓ uploaded  screenshot.png  412 KB
  https://cdn.krowk.com/ws_9f3c/art_2e1d/screenshot.png
  run run_8Kd2wq · expires in 24h
```

## What this package is

A launcher. krowk itself is a single static Go binary with no runtime and no
dependencies; this package exists because the website says `npx @krowk/cli push`
and some people are already in Node.

The binary it installs is still called `krowk`, and npx runs a package's only bin
whatever that bin is named, so `npx @krowk/cli push` works. The bare `krowk` name
on npm is not available — npm's typosquat filter rejects it as too similar to the
existing `growl` package — which is why this ships under the `@krowk` scope.

Installing it pulls one more package — `@krowk/cli-linux-x64` or whichever
matches your machine — through the same npm registry as everything else. There
is no postinstall script and no download from a second host, so it works behind
a proxy, under `npm ci --ignore-scripts`, and off a private mirror.

If Node is not already in the picture, skip it. The binary is the primary
channel:

```bash
go install github.com/krowkcom/cli/cmd/krowk@latest
# or grab an archive from https://github.com/krowkcom/cli/releases/latest
```

## Documentation

Commands, flags, output formats, MCP, and what the CLI refuses to upload and
why: <https://github.com/krowkcom/cli>.

MIT.
