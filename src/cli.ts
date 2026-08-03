#!/usr/bin/env node
import { parseArgs } from "node:util"
import { statSync } from "node:fs"
import { fileURLToPath } from "node:url"
import { resolve } from "node:path"
import { ApiError, apiBase, createArtifact, credentialsPath, readToken, saveToken } from "./api"
import { detectContext, prune } from "./context"
import { renderArtifact, renderError, resolveFormat, type Format } from "./output"

export const VERSION = "0.1.0"

const HELP = `krowk ${VERSION} — permalinks for agent output

Usage
  krowk uploads create <file...> [flags]   Upload artifacts, get one canonical URL
  krowk push <file...> [flags]             Alias for \`uploads create\`
  krowk auth login --token <token>         Store an API token
  krowk auth token                         Print the stored token
  krowk doctor                             Check the local setup

Upload flags
  --pull-request <url>   Pull request the work belongs to
  --reference <url>      Related link — repeat for more than one
  --session <id>         Agent session ID
  --title <text>         Label for the unfurl card
  --repo <owner/name>    Override the detected repository
  --commit <sha>         Override the detected commit
  --agent <name>         Override the detected agent

Global flags
  --format <fmt>         human | json | markdown (default: human on a TTY, json when piped)
  --json                 Shorthand for --format json
  --quiet                Raw JSON, no envelope
  -h, --help             Show this
  -v, --version          Print the version

Environment
  KROWK_TOKEN            API token — wins over the credentials file
  KROWK_API_URL          API base URL (default https://api.krowk.com/v1)
  KROWK_AGENT            Agent name to report

Credentials live in ${credentialsPath()} (0600).
`

const OPTIONS = {
  "pull-request": { type: "string" },
  reference: { type: "string", multiple: true },
  session: { type: "string" },
  title: { type: "string" },
  repo: { type: "string" },
  commit: { type: "string" },
  agent: { type: "string" },
  token: { type: "string" },
  format: { type: "string" },
  json: { type: "boolean", default: false },
  quiet: { type: "boolean", default: false },
  help: { type: "boolean", short: "h", default: false },
  version: { type: "boolean", short: "v", default: false },
} as const

type Flags = {
  "pull-request"?: string
  reference?: string[]
  session?: string
  title?: string
  repo?: string
  commit?: string
  agent?: string
  quiet?: boolean
}

/** Every failure — local or remote — leaves through the same machine-readable body. */
function fail(error: string, fix: string): ApiError {
  return new ApiError(0, { error, fix, retryable: false })
}

export async function main(argv: string[]): Promise<number> {
  let format: Format = "human"

  try {
    const { values, positionals } = parseArgs({ args: argv, options: OPTIONS, allowPositionals: true })
    format = resolveFormat(values.format, values.json)

    if (values.version) return say(VERSION)
    if (values.help || positionals.length === 0 || positionals[0] === "help") return say(HELP)

    const [first, second] = positionals

    if (first === "push") return await upload(positionals.slice(1), values, format)
    if (first === "uploads" && second === "create") return await upload(positionals.slice(2), values, format)
    if (first === "auth" && second === "login") return authLogin(values.token)
    if (first === "auth" && second === "token") return authToken()
    if (first === "doctor") return await doctor(format)

    throw fail(
      `unknown_command`,
      `\`${positionals.slice(0, 2).join(" ")}\` is not a krowk command — run \`krowk --help\``,
    )
  } catch (err) {
    console.error(renderError(err, format, false))
    return 1
  }
}

const say = (s: string) => (console.log(s), 0)

async function upload(files: string[], flags: Flags, format: Format): Promise<number> {
  if (files.length === 0) throw fail("no_file", "pass at least one path: `krowk uploads create screenshot.png`")

  for (const file of files) {
    if (!statSync(file, { throwIfNoEntry: false })?.isFile()) {
      throw fail("file_unreadable", `cannot read \`${file}\` — paths resolve from the current directory`)
    }
  }

  const metadata = {
    ...detectContext(),
    ...prune({
      pull_request: flags["pull-request"],
      reference: flags.reference,
      session: flags.session,
      title: flags.title,
      repo: flags.repo,
      commit: flags.commit,
      agent: flags.agent,
    }),
    client: `krowk-cli/${VERSION}`,
  }

  const artifact = await createArtifact(files, metadata)
  console.log(renderArtifact(artifact, format, { title: metadata.title, quiet: flags.quiet }))
  return 0
}

function authLogin(token: string | undefined): number {
  if (!token) throw fail("missing_token", "pass the key: `krowk auth login --token krk_...`")
  return say(`✓ token stored in ${saveToken(token)}`)
}

function authToken(): number {
  const token = readToken()
  if (!token) throw fail("not_authenticated", "run `krowk auth login --token krk_...`, or upload anonymously")
  return say(token)
}

async function doctor(format: Format): Promise<number> {
  const report = {
    version: VERSION,
    runtime: process.version,
    api: apiBase(),
    api_status: await fetch(apiBase() + "/artifacts", { method: "OPTIONS" })
      .then((r) => `reachable (HTTP ${r.status})`)
      .catch((e) => `unreachable — ${String(e)}`),
    authenticated: Boolean(readToken()),
    credentials: credentialsPath(),
    context: detectContext(),
  }

  if (format !== "human") return say(JSON.stringify(report, null, 2))
  return say(
    Object.entries(report)
      .map(([k, v]) => `${k.padEnd(14)} ${typeof v === "object" ? JSON.stringify(v) : String(v)}`)
      .join("\n"),
  )
}

// Run only when invoked as the binary; tests import main() instead.
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exitCode = await main(process.argv.slice(2))
}
