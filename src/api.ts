import { openAsBlob, mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { homedir } from "node:os"
import { basename, join } from "node:path"

export const apiBase = () => (process.env.KROWK_API_URL ?? "https://api.krowk.com/v1").replace(/\/$/, "")

const MAX_ATTEMPTS = 3

// ── credentials ───────────────────────────────────────────────────────────
const configDir = () => join(process.env.XDG_CONFIG_HOME ?? join(homedir(), ".config"), "krowk")
export const credentialsPath = () => join(configDir(), "credentials.json")

export function readToken(): string | undefined {
  if (process.env.KROWK_TOKEN) return process.env.KROWK_TOKEN
  try {
    return JSON.parse(readFileSync(credentialsPath(), "utf8")).token || undefined
  } catch {
    return undefined
  }
}

export function saveToken(token: string): string {
  mkdirSync(configDir(), { recursive: true, mode: 0o700 })
  writeFileSync(credentialsPath(), JSON.stringify({ token }, null, 2) + "\n", { mode: 0o600 })
  return credentialsPath()
}

// ── errors ────────────────────────────────────────────────────────────────
/** Carries the machine-readable failure body verbatim: {error, fix, retryable, ...limits}. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly body: Record<string, unknown>,
  ) {
    super(String(body.error ?? `http_${status}`))
    this.name = "ApiError"
  }

  get code() {
    return String(this.body.error ?? `http_${this.status}`)
  }

  get fix() {
    return typeof this.body.fix === "string" ? this.body.fix : undefined
  }

  /** Servers may state it; otherwise 429 and 5xx are worth another try. */
  get retryable(): boolean {
    if (typeof this.body.retryable === "boolean") return this.body.retryable
    return this.status === 429 || this.status >= 500
  }
}

// ── requests ──────────────────────────────────────────────────────────────
export type Artifact = {
  id: string
  url: string
  preview_url?: string
  bytes?: number
  expires_at?: string
  files?: { filename: string; bytes: number; content_type?: string }[]
  metadata?: Record<string, unknown>
  rate_limit_remaining?: string | null
}

export async function createArtifact(
  files: string[],
  metadata: Record<string, unknown>,
): Promise<Artifact> {
  const form = new FormData()
  for (const path of files) form.append("file", await openAsBlob(path), basename(path))
  form.append("metadata", JSON.stringify(metadata))
  return request("POST", "/artifacts", form)
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

function backoffMs(res: Response, attempt: number): number {
  const header = Number(res.headers.get("retry-after"))
  return Number.isFinite(header) && header > 0 ? header * 1000 : 2 ** attempt * 500
}

async function request(method: string, path: string, body?: BodyInit): Promise<Artifact> {
  const token = readToken()
  let last: ApiError | undefined

  for (let attempt = 1; attempt <= MAX_ATTEMPTS; attempt++) {
    let res: Response
    try {
      res = await fetch(apiBase() + path, {
        method,
        body,
        headers: token ? { authorization: `Bearer ${token}` } : {},
      })
    } catch (cause) {
      throw new ApiError(0, {
        error: "network_unreachable",
        endpoint: apiBase() + path,
        detail: String(cause),
        fix: `cannot reach ${apiBase()} — check the network, or point KROWK_API_URL at a reachable registry`,
        retryable: false,
      })
    }

    const payload = (await res.json().catch(() => ({
      error: `http_${res.status}`,
      fix: "the registry did not return JSON — check KROWK_API_URL points at the API, not the website",
    }))) as Record<string, unknown>

    if (res.ok) {
      return { ...(payload as Artifact), rate_limit_remaining: res.headers.get("x-ratelimit-remaining") }
    }

    last = new ApiError(res.status, payload)
    if (!last.retryable || attempt === MAX_ATTEMPTS) throw last
    await sleep(backoffMs(res, attempt))
  }

  throw last!
}
