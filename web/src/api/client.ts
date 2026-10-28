import type { DirListing, SessionInfo, SessionSummary, UserDecision } from './types'

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message)
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const data = res.status === 204 ? {} : await res.json().catch(() => ({}))
  if (!res.ok) {
    throw new ApiError(res.status, (data as { error?: string }).error ?? res.statusText)
  }
  return data as T
}

export const api = {
  listSessions: () => request<{ sessions: SessionSummary[] }>('GET', '/api/sessions?limit=100'),
  createSession: (cwd?: string) => request<SessionInfo>('POST', '/api/sessions', cwd ? { cwd } : {}),
  listDirs: (path = '', hidden = false) =>
    request<DirListing>('GET', `/api/workspace/dirs?path=${encodeURIComponent(path)}${hidden ? '&hidden=1' : ''}`),
  /** 在运行 joey 的机器上弹出系统目录选择器；不支持时抛出 status 501 的 ApiError。 */
  pickFolder: (start?: string) =>
    request<{ path?: string; cancelled?: boolean }>('POST', '/api/workspace/pick', start ? { start } : {}),
  getSession: (id: string) => request<SessionInfo>('GET', `/api/sessions/${encodeURIComponent(id)}`),
  /** 删除会话及其全部历史；会话正在运行时返回 409。 */
  deleteSession: (id: string) => request<Record<string, never>>('DELETE', `/api/sessions/${encodeURIComponent(id)}`),
  submitTurn: (id: string, text: string) =>
    request<{ turn_id: string }>('POST', `/api/sessions/${encodeURIComponent(id)}/turns`, { text }),
  interrupt: (id: string) =>
    request<{ interrupted: boolean }>('POST', `/api/sessions/${encodeURIComponent(id)}/interrupt`),
  resolveApproval: (id: string, approvalId: string, decision: UserDecision) =>
    request<{ decision: string }>(
      'POST',
      `/api/sessions/${encodeURIComponent(id)}/approvals/${encodeURIComponent(approvalId)}`,
      { decision },
    ),
  eventsURL: (id: string, after: number) => `/api/sessions/${encodeURIComponent(id)}/events?after=${after}`,
}
