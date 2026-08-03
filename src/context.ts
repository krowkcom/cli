import { execFileSync } from "node:child_process"

/** Run metadata the agent should never have to type. Flags override every field. */
export type Context = {
  repo?: string
  commit?: string
  branch?: string
  agent?: string
  pull_request?: string
}

export function prune<T extends object>(o: T): T {
  return Object.fromEntries(
    Object.entries(o).filter(([, v]) => v !== undefined && v !== null && v !== ""),
  ) as T
}

function git(...args: string[]): string | undefined {
  try {
    return execFileSync("git", args, { stdio: ["ignore", "pipe", "ignore"] }).toString().trim() || undefined
  } catch {
    return undefined
  }
}

/** git@github.com:acme/storefront.git → acme/storefront */
export function slug(remote: string | undefined): string | undefined {
  return remote?.match(/[:/]([^/:]+\/[^/]+?)(?:\.git)?\/?$/)?.[1]
}

/** refs/pull/412/merge → https://github.com/acme/storefront/pull/412 */
export function ciPullRequest(env: NodeJS.ProcessEnv): string | undefined {
  const number = env.GITHUB_REF?.match(/^refs\/pull\/(\d+)\//)?.[1]
  return number && env.GITHUB_REPOSITORY
    ? `https://github.com/${env.GITHUB_REPOSITORY}/pull/${number}`
    : undefined
}

export function detectAgent(env: NodeJS.ProcessEnv): string | undefined {
  if (env.KROWK_AGENT) return env.KROWK_AGENT
  if (env.CLAUDECODE || env.CLAUDE_CODE) return "claude-code"
  if (env.CURSOR_TRACE_ID) return "cursor"
  if (env.GITHUB_ACTIONS) return "github-actions"
  return undefined
}

export function detectContext(env: NodeJS.ProcessEnv = process.env): Context {
  return prune({
    repo: env.GITHUB_REPOSITORY ?? slug(git("remote", "get-url", "origin")),
    commit: env.GITHUB_SHA ?? git("rev-parse", "HEAD"),
    branch: git("rev-parse", "--abbrev-ref", "HEAD"),
    agent: detectAgent(env),
    pull_request: ciPullRequest(env),
  })
}
