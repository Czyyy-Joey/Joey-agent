import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

export function formatTokens(n: number): string {
  if (n < 1000) return String(n)
  return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`
}

/** 格式化 JSON 工具参数；如果无效则原样返回输入。 */
export function prettyJSON(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}

/** 工具参数的单行摘要，例如 `path: README.md`。 */
export function summarizeArgs(raw: string): string {
  try {
    const v = JSON.parse(raw) as Record<string, unknown>
    return Object.entries(v)
      .map(([k, val]) => `${k}: ${typeof val === 'string' ? val : JSON.stringify(val)}`)
      .join(', ')
  } catch {
    return raw
  }
}
