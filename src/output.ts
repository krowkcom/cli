import { ApiError, type Artifact } from "./api"

export type Format = "human" | "json" | "markdown"

export type Breadcrumb = { action: string; cmd: string }

/** Same envelope shape the Basecamp CLI uses: agents get data plus a next move. */
export type Envelope = {
  ok: boolean
  data?: unknown
  summary?: string
  breadcrumbs?: Breadcrumb[]
  error?: Record<string, unknown>
}

const tty = () => Boolean(process.stdout.isTTY)
const paint = (code: string, s: string) => (tty() ? `\x1b[${code}m${s}\x1b[0m` : s)
const dim = (s: string) => paint("2", s)
const green = (s: string) => paint("32", s)
const red = (s: string) => paint("31", s)

export function resolveFormat(flag: string | undefined, json: boolean): Format {
  if (json) return "json"
  if (flag === "human" || flag === "json" || flag === "markdown") return flag
  if (flag) throw new Error(`unknown --format ${flag} (expected human, json or markdown)`)
  return tty() ? "human" : "json"
}

export function humanBytes(n: number | undefined): string {
  if (n === undefined) return "?"
  const units = ["B", "KB", "MB", "GB"]
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  return `${n < 10 && i > 0 ? n.toFixed(1) : Math.round(n)} ${units[i]}`
}

export function relativeExpiry(iso: string | undefined, now = Date.now()): string | undefined {
  if (!iso) return undefined
  const ms = Date.parse(iso) - now
  if (!Number.isFinite(ms)) return undefined
  if (ms <= 0) return "expired"
  const hours = Math.round(ms / 3_600_000)
  return hours < 48 ? `expires in ${hours}h` : `expires in ${Math.round(hours / 24)}d`
}

export function markdown(a: Artifact, title?: string): string {
  const label = title ?? a.files?.[0]?.filename ?? a.id
  return a.preview_url ? `[![${label}](${a.preview_url})](${a.url})` : `[${label}](${a.url})`
}

function humanArtifact(a: Artifact, title?: string): string {
  const count = a.files?.length ?? 1
  const what = count === 1 ? (a.files?.[0]?.filename ?? a.id) : `${count} artifacts`
  const lines = [`${green("✓")} uploaded  ${what}  ${humanBytes(a.bytes)}`, `  ${a.url}`]

  const notes = [relativeExpiry(a.expires_at)].filter(Boolean) as string[]
  if (notes.length) lines.push(dim(`  ${notes.join(" · ")}`))
  if (title) lines.push(dim(`  ${title}`))
  return lines.join("\n")
}

export function renderArtifact(a: Artifact, format: Format, opts: { title?: string; quiet?: boolean }): string {
  if (format === "markdown") return markdown(a, opts.title)
  if (format === "human") return humanArtifact(a, opts.title)

  const envelope: Envelope = {
    ok: true,
    data: a,
    summary: `${a.files?.length ?? 1} artifact(s), ${humanBytes(a.bytes)}`,
    breadcrumbs: [{ action: "share", cmd: `open ${a.url}` }],
  }
  return JSON.stringify(opts.quiet ? a : envelope, null, 2)
}

export function renderError(err: unknown, format: Format, quiet = false): string {
  const body: Record<string, unknown> =
    err instanceof ApiError ? { status: err.status, ...err.body } : { error: "cli_error", detail: String(err) }

  if (format === "human" || format === "markdown") {
    const { error, fix, retryable, status, ...rest } = body
    const lines = [`${red("✗")} ${String(error)}${status ? dim(`  (HTTP ${status})`) : ""}`]
    for (const [k, v] of Object.entries(rest)) lines.push(dim(`  ${k}: ${String(v)}`))
    if (fix) lines.push(`  fix: ${String(fix)}`)
    if (retryable === true) lines.push(dim("  retryable: yes"))
    return lines.join("\n")
  }
  return JSON.stringify(quiet ? body : ({ ok: false, error: body } satisfies Envelope), null, 2)
}
