import type { AgentEvent, ApprovalData, Compaction, Item, ItemStatus, SessionInfo, TurnStatus, Usage } from '@/api/types'

// 时间线条目是 UI 渲染的内容。工具调用及其输出在历史中是
// 单独的条目；这里按调用 id 合并为一个条目。
export type Entry =
  | { type: 'user'; id: string; turnId: string; text: string }
  | { type: 'agent'; id: string; turnId: string; text: string; status: ItemStatus }
  | { type: 'reasoning'; id: string; turnId: string; text: string; status: ItemStatus }
  | {
      type: 'tool'
      id: string
      turnId: string
      callId: string
      name: string
      args: string
      output?: string
      status: ItemStatus
      isError: boolean
      durationMs?: number
      /** 文件改动的 unified diff（write_file、apply_patch 成功时）。 */
      diff?: string
      /** 运行中收到的实时输出（命令的 stdout/stderr），完成后由 output 取代。 */
      live?: string
      /** 这次调用的审批；decision 为空表示仍在等待用户。 */
      approval?: ApprovalData
    }
  | {
      type: 'compaction'
      id: string
      turnId: string
      status: ItemStatus
      /** 完成时是摘要，失败时是错误信息，压缩中是已生成的摘要片段。 */
      text: string
      info?: Compaction
    }
  | { type: 'notice'; id: string; turnId: string; level: 'warning' | 'error'; text: string }
  | { type: 'turn_end'; id: string; turnId: string; status: TurnStatus; error?: string }

export type RunState = 'idle' | 'running' | 'interrupting'

export interface ConversationState {
  sessionId?: string
  lastSeq: number
  runState: RunState
  activeTurn?: string
  entries: Entry[]
  usage: {
    last?: Usage
    turn?: Usage
    session: Usage
    contextWindow?: number
    /** 当前上下文大小；压缩后会下降，不同于 last.input_tokens。 */
    contextTokens?: number
    estimated?: boolean
    autoCompactLimit?: number
  }
}

const zeroUsage = (): Usage => ({ input_tokens: 0, output_tokens: 0, total_tokens: 0 })

export const initialState: ConversationState = {
  lastSeq: 0,
  runState: 'idle',
  entries: [],
  usage: { session: zeroUsage() },
}

/** 根据会话快照构建状态（仅包含已结束的回合）。 */
export function hydrate(info: SessionInfo): ConversationState {
  const entries: Entry[] = []
  const toolIndex = new Map<string, number>()
  // 未正常完成的回合在其最后一个条目之后显示结束标记，与实时事件一致。
  const ended = new Map((info.turns ?? []).filter((t) => t.status !== 'completed').map((t) => [t.id, t]))
  const closeTurn = (turnId: string | undefined) => {
    const t = turnId ? ended.get(turnId) : undefined
    if (!t) return
    entries.push({ type: 'turn_end', id: `end_${t.id}`, turnId: t.id, status: t.status, error: t.error })
    ended.delete(t.id)
  }
  let current: string | undefined
  for (const it of info.items) {
    if (it.turn_id !== current) {
      closeTurn(current)
      current = it.turn_id
    }
    const e = entryFromItem(it)
    if (e) {
      if (e.type === 'tool') toolIndex.set(e.callId, entries.length)
      entries.push(e)
      continue
    }
    if (it.kind === 'tool_output' && it.output) {
      const idx = toolIndex.get(it.output.call_id)
      if (idx === undefined) continue
      const tool = entries[idx] as Extract<Entry, { type: 'tool' }>
      const aborted = it.output.content.startsWith('aborted:')
      entries[idx] = {
        ...tool,
        output: it.output.content,
        diff: it.output.diff,
        isError: !!it.output.is_error,
        status: aborted ? 'aborted' : it.output.is_error ? 'failed' : 'completed',
      }
    }
  }
  closeTurn(current)
  return { ...initialState, sessionId: info.id, lastSeq: info.last_seq, entries, usage: { session: zeroUsage() } }
}

function entryFromItem(it: Item): Entry | undefined {
  switch (it.kind) {
    case 'user_message':
      return { type: 'user', id: it.id, turnId: it.turn_id, text: it.text ?? '' }
    case 'agent_message':
      return { type: 'agent', id: it.id, turnId: it.turn_id, text: it.text ?? '', status: 'completed' }
    case 'tool_call':
      if (!it.call) return undefined
      return {
        type: 'tool',
        id: it.id,
        turnId: it.turn_id,
        callId: it.call.call_id,
        name: it.call.name,
        args: it.call.arguments,
        status: 'completed',
        isError: false,
      }
    case 'compaction':
      return { type: 'compaction', id: it.id, turnId: it.turn_id, status: 'completed', text: it.text ?? '', info: it.compaction }
  }
  return undefined
}

/**
 * 应用一个事件。seq 小于等于 lastSeq 的事件已经应用过（重连后的重放），
 * 会被忽略，因此 reducer 是幂等的。
 */
export function applyEvent(state: ConversationState, ev: AgentEvent): ConversationState {
  if (ev.seq <= state.lastSeq) return state
  const s: ConversationState = { ...state, lastSeq: ev.seq }
  const turnId = ev.turn_id ?? ''

  switch (ev.type) {
    case 'turn.started':
      return { ...s, runState: 'running', activeTurn: turnId, usage: { ...s.usage, turn: undefined } }

    case 'turn.completed': {
      const entries = [...s.entries]
      if (ev.data.status !== 'completed') {
        entries.push({ type: 'turn_end', id: `end_${turnId}`, turnId, status: ev.data.status, error: ev.data.error })
      }
      return { ...s, entries, runState: 'idle', activeTurn: undefined }
    }

    case 'token.usage': {
      const session = { ...s.usage.session }
      session.input_tokens += ev.data.last.input_tokens
      session.output_tokens += ev.data.last.output_tokens
      session.total_tokens += ev.data.last.total_tokens
      return {
        ...s,
        usage: {
          // 压缩和工具执行后的用量事件没有新的请求，last 为零。
          last: ev.data.last.total_tokens ? ev.data.last : s.usage.last,
          turn: ev.data.turn,
          session,
          contextWindow: ev.data.context_window,
          contextTokens: ev.data.context_tokens,
          estimated: ev.data.estimated,
          autoCompactLimit: ev.data.auto_compact_limit,
        },
      }
    }

    case 'warning':
    case 'error':
      return {
        ...s,
        entries: [
          ...s.entries,
          { type: 'notice', id: `notice_${ev.seq}`, turnId, level: ev.type, text: ev.data.message },
        ],
      }

    case 'item.started':
    case 'item.delta':
    case 'item.completed':
      return { ...s, entries: applyItemEvent(s.entries, ev, turnId) }

    case 'approval.requested':
    case 'approval.resolved': {
      const idx = s.entries.findIndex((e) => e.type === 'tool' && (e.id === ev.item_id || e.callId === ev.data.call_id))
      if (idx < 0) return s
      const entries = [...s.entries]
      entries[idx] = { ...(entries[idx] as ToolEntry), approval: ev.data }
      return { ...s, entries }
    }
  }
  return s
}

type ToolEntry = Extract<Entry, { type: 'tool' }>

/** 实时输出最多保留的字符数；完整输出在调用完成时给出。 */
export const LIVE_OUTPUT_LIMIT = 32_000

/** 返回仍在等待用户决定的审批。 */
export function pendingApprovals(state: ConversationState): ApprovalData[] {
  return state.entries.flatMap((e) => (e.type === 'tool' && e.approval && !e.approval.decision ? [e.approval] : []))
}

type ItemEvent = Extract<AgentEvent, { type: 'item.started' | 'item.delta' | 'item.completed' }>

function applyItemEvent(entries: Entry[], ev: ItemEvent, turnId: string): Entry[] {
  const id = ev.item_id ?? ''
  const d = ev.data
  const idx = entries.findIndex((e) => e.id === id)
  const existing = idx >= 0 ? entries[idx] : undefined
  let next: Entry

  switch (d.kind) {
    case 'user_message':
      next = { type: 'user', id, turnId, text: d.text ?? '' }
      break
    case 'agent_message':
    case 'reasoning': {
      const type = d.kind === 'agent_message' ? 'agent' : 'reasoning'
      const prev = existing && existing.type === type ? existing : undefined
      const text =
        ev.type === 'item.delta' ? (prev?.text ?? '') + (d.text ?? '') : ev.type === 'item.completed' ? (d.text ?? prev?.text ?? '') : (prev?.text ?? '')
      const status = ev.type === 'item.completed' ? (d.status ?? 'completed') : 'in_progress'
      next = { type, id, turnId, text, status }
      break
    }
    case 'tool_call': {
      const prev = existing && existing.type === 'tool' ? existing : undefined
      if (ev.type === 'item.delta') {
        if (!prev) return entries
        const live = (prev.live ?? '') + (d.output ?? '')
        next = { ...prev, live: live.length > LIVE_OUTPUT_LIMIT ? live.slice(-LIVE_OUTPUT_LIMIT) : live }
        break
      }
      next = {
        ...prev,
        type: 'tool',
        id,
        turnId,
        callId: d.call_id ?? prev?.callId ?? '',
        name: d.name ?? prev?.name ?? '',
        args: d.arguments ?? prev?.args ?? '',
        output: d.output ?? prev?.output,
        diff: d.diff ?? prev?.diff,
        status: d.status ?? prev?.status ?? 'in_progress',
        isError: d.is_error ?? prev?.isError ?? false,
        // duration_ms 为零时不会出现在 JSON 中。
        durationMs: d.duration_ms ?? (ev.type === 'item.completed' ? 0 : prev?.durationMs),
        live: ev.type === 'item.completed' ? undefined : prev?.live,
      }
      break
    }
    case 'compaction': {
      const prev = existing && existing.type === 'compaction' ? existing : undefined
      const text = ev.type === 'item.delta' ? (prev?.text ?? '') + (d.text ?? '') : (d.text ?? prev?.text ?? '')
      const status = ev.type === 'item.completed' ? (d.status ?? 'completed') : 'in_progress'
      next = { type: 'compaction', id, turnId, status, text, info: d.compaction ?? prev?.info }
      break
    }
    default:
      return entries
  }

  const out = [...entries]
  if (idx >= 0) out[idx] = next
  else out.push(next)
  return out
}

/** 将运行中的回合标记为正在中断（乐观 UI）。 */
export function markInterrupting(state: ConversationState): ConversationState {
  return state.runState === 'running' ? { ...state, runState: 'interrupting' } : state
}
