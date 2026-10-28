import { describe, expect, it } from 'vitest'
import type { AgentEvent } from '@/api/types'
import { applyEvent, hydrate, initialState, LIVE_OUTPUT_LIMIT, markInterrupting, pendingApprovals, type ConversationState } from './reducer'

let seq = 0
function ev(partial: Omit<AgentEvent, 'seq' | 'session_id' | 'agent_id' | 'ts'> & { seq?: number }): AgentEvent {
  seq = partial.seq ?? seq + 1
  return { session_id: 's', agent_id: 'root', ts: '', ...partial, seq } as AgentEvent
}

function run(events: AgentEvent[], from: ConversationState = initialState) {
  return events.reduce(applyEvent, from)
}

describe('applyEvent', () => {
  it('streams an agent message', () => {
    seq = 0
    const s = run([
      ev({ type: 'turn.started', turn_id: 't1', data: { status: 'running', input: 'hi' } }),
      ev({ type: 'item.completed', turn_id: 't1', item_id: 'u1', data: { kind: 'user_message', status: 'completed', text: 'hi' } }),
      ev({ type: 'item.started', turn_id: 't1', item_id: 'a1', data: { kind: 'agent_message', status: 'in_progress' } }),
      ev({ type: 'item.delta', turn_id: 't1', item_id: 'a1', data: { kind: 'agent_message', text: 'Hel' } }),
      ev({ type: 'item.delta', turn_id: 't1', item_id: 'a1', data: { kind: 'agent_message', text: 'lo' } }),
    ])
    expect(s.runState).toBe('running')
    expect(s.activeTurn).toBe('t1')
    expect(s.entries).toEqual([
      { type: 'user', id: 'u1', turnId: 't1', text: 'hi' },
      { type: 'agent', id: 'a1', turnId: 't1', text: 'Hello', status: 'in_progress' },
    ])

    const done = run(
      [
        ev({ type: 'item.completed', turn_id: 't1', item_id: 'a1', data: { kind: 'agent_message', status: 'completed', text: 'Hello' } }),
        ev({ type: 'turn.completed', turn_id: 't1', data: { status: 'completed' } }),
      ],
      s,
    )
    expect(done.runState).toBe('idle')
    expect(done.entries[1]).toMatchObject({ status: 'completed', text: 'Hello' })
    expect(done.entries).toHaveLength(2)
  })

  it('merges tool call start and completion', () => {
    seq = 0
    const s = run([
      ev({ type: 'item.started', turn_id: 't', item_id: 'c', data: { kind: 'tool_call', status: 'in_progress', call_id: 'k', name: 'read_file', arguments: '{"path":"a"}' } }),
      ev({ type: 'item.completed', turn_id: 't', item_id: 'c', data: { kind: 'tool_call', status: 'failed', call_id: 'k', name: 'read_file', arguments: '{"path":"a"}', output: 'no such file', is_error: true, duration_ms: 3 } }),
    ])
    expect(s.entries).toEqual([
      { type: 'tool', id: 'c', turnId: 't', callId: 'k', name: 'read_file', args: '{"path":"a"}', output: 'no such file', status: 'failed', isError: true, durationMs: 3 },
    ])
  })

  it('keeps the file diff of an edit tool', () => {
    seq = 0
    const diff = '--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n'
    const s = run([
      ev({ type: 'item.started', turn_id: 't', item_id: 'c', data: { kind: 'tool_call', status: 'in_progress', call_id: 'k', name: 'write_file', arguments: '{}' } }),
      ev({ type: 'item.completed', turn_id: 't', item_id: 'c', data: { kind: 'tool_call', status: 'completed', call_id: 'k', name: 'write_file', arguments: '{}', output: 'Overwrote x', diff } }),
    ])
    expect(s.entries[0]).toMatchObject({ output: 'Overwrote x', diff })
  })

  it('ignores duplicate and stale events', () => {
    seq = 0
    const delta = ev({ type: 'item.delta', turn_id: 't', item_id: 'a', data: { kind: 'agent_message', text: 'x' } })
    const s = run([delta, delta, { ...delta, seq: 0 }])
    expect(s.entries).toEqual([{ type: 'agent', id: 'a', turnId: 't', text: 'x', status: 'in_progress' }])
    expect(s.lastSeq).toBe(1)
  })

  it('shows interrupted and failed turns', () => {
    seq = 0
    const s = run([
      ev({ type: 'turn.started', turn_id: 't', data: { status: 'running' } }),
      ev({ type: 'error', turn_id: 't', data: { message: 'upstream down' } }),
      ev({ type: 'turn.completed', turn_id: 't', data: { status: 'failed', error: 'upstream down' } }),
    ])
    expect(s.entries.map((e) => e.type)).toEqual(['notice', 'turn_end'])
    expect(s.entries[1]).toMatchObject({ status: 'failed', error: 'upstream down' })
    expect(markInterrupting(s).runState).toBe('idle')
    expect(markInterrupting({ ...s, runState: 'running' }).runState).toBe('interrupting')
  })

  it('accumulates token usage', () => {
    seq = 0
    const u = (n: number) => ({ input_tokens: n, output_tokens: 1, total_tokens: n + 1 })
    const s = run([
      ev({ type: 'token.usage', turn_id: 't', data: { last: u(10), turn: u(10), context_window: 32000 } }),
      ev({ type: 'token.usage', turn_id: 't', data: { last: u(20), turn: u(30) } }),
    ])
    expect(s.usage.last?.input_tokens).toBe(20)
    expect(s.usage.turn?.input_tokens).toBe(30)
    expect(s.usage.session).toEqual({ input_tokens: 30, output_tokens: 2, total_tokens: 32 })
  })
})

describe('hydrate', () => {
  it('rebuilds the timeline from history items', () => {
    const s = hydrate({
      id: 's1',
      title: '',
      cwd: '/w',
      created_at: '',
      updated_at: '',
      status: 'idle',
      last_seq: 42,
      turns: [{ id: 't', status: 'completed' }],
      items: [
        { id: 'u', turn_id: 't', kind: 'user_message', text: 'q' },
        { id: 'c', turn_id: 't', kind: 'tool_call', call: { call_id: 'k1', name: 'read_file', arguments: '{}' } },
        { id: 'c2', turn_id: 't', kind: 'tool_call', call: { call_id: 'k2', name: 'list_directory', arguments: '{}' } },
        { id: 'o', turn_id: 't', kind: 'tool_output', output: { call_id: 'k1', content: 'data', diff: '--- a/f\n' } },
        { id: 'o2', turn_id: 't', kind: 'tool_output', output: { call_id: 'k2', content: 'aborted: x', is_error: true } },
        { id: 'a', turn_id: 't', kind: 'agent_message', text: 'answer' },
      ],
    })
    expect(s.sessionId).toBe('s1')
    expect(s.lastSeq).toBe(42)
    expect(s.entries.map((e) => e.type)).toEqual(['user', 'tool', 'tool', 'agent'])
    expect(s.entries[1]).toMatchObject({ output: 'data', diff: '--- a/f\n', status: 'completed', isError: false })
    expect(s.entries[2]).toMatchObject({ status: 'aborted', isError: true })

    // 快照已覆盖的事件会被跳过。
    const same = applyEvent(s, ev({ seq: 42, type: 'item.delta', turn_id: 't', item_id: 'a', data: { kind: 'agent_message', text: 'dup' } }))
    expect(same).toBe(s)
  })
})

describe('hydrate turn markers', () => {
  it('adds end markers for interrupted and failed turns and hides abort markers', () => {
    const s = hydrate({
      id: 's1',
      title: '',
      cwd: '/w',
      created_at: '',
      updated_at: '',
      status: 'idle',
      last_seq: 9,
      turns: [
        { id: 't1', status: 'interrupted', error: 'the joey process stopped before this turn finished' },
        { id: 't2', status: 'completed' },
        { id: 't3', status: 'failed', error: 'boom' },
      ],
      items: [
        { id: 'u1', turn_id: 't1', kind: 'user_message', text: 'a' },
        { id: 'm1', turn_id: 't1', kind: 'turn_aborted', text: '<turn_aborted>' },
        { id: 'u2', turn_id: 't2', kind: 'user_message', text: 'b' },
        { id: 'a2', turn_id: 't2', kind: 'agent_message', text: 'ok' },
        { id: 'u3', turn_id: 't3', kind: 'user_message', text: 'c' },
      ],
    })
    expect(s.entries.map((e) => e.type)).toEqual(['user', 'turn_end', 'user', 'agent', 'user', 'turn_end'])
    expect(s.entries[1]).toMatchObject({ turnId: 't1', status: 'interrupted' })
    expect(s.entries[5]).toMatchObject({ turnId: 't3', status: 'failed', error: 'boom' })
  })
})

describe('approvals and live output', () => {
  const start = () =>
    ev({ type: 'item.started', turn_id: 't', item_id: 'c', data: { kind: 'tool_call', status: 'in_progress', call_id: 'k', name: 'exec_command', arguments: '{"command":"go test"}' } })

  it('attaches approvals to the tool card and tracks the decision', () => {
    seq = 0
    const req = { approval_id: 'a1', call_id: 'k', tool: 'exec_command', kind: 'exec' as const, command: 'go test', cwd: '.' }
    const s = run([start(), ev({ type: 'approval.requested', turn_id: 't', item_id: 'c', data: req })])
    expect(pendingApprovals(s)).toEqual([req])
    expect(s.entries[0]).toMatchObject({ approval: req })

    const done = run(
      [
        ev({ type: 'approval.resolved', turn_id: 't', item_id: 'c', data: { ...req, decision: 'denied' } }),
        ev({ type: 'item.completed', turn_id: 't', item_id: 'c', data: { kind: 'tool_call', status: 'failed', call_id: 'k', name: 'exec_command', arguments: '{}', output: 'the user denied this action', is_error: true } }),
      ],
      s,
    )
    expect(pendingApprovals(done)).toEqual([])
    expect(done.entries[0]).toMatchObject({ status: 'failed', approval: { decision: 'denied' } })
  })

  it('ignores approvals for unknown calls', () => {
    seq = 0
    const s = run([ev({ type: 'approval.requested', turn_id: 't', item_id: 'zz', data: { approval_id: 'a', call_id: 'nope', tool: 'x', kind: 'exec' } })])
    expect(s.entries).toEqual([])
  })

  it('streams command output and replaces it with the final output', () => {
    seq = 0
    const delta = (out: string) => ev({ type: 'item.delta', turn_id: 't', item_id: 'c', data: { kind: 'tool_call', call_id: 'k', output: out } })
    const s = run([start(), delta('ok  pkg/a\n'), delta('ok  pkg/b\n')])
    expect(s.entries[0]).toMatchObject({ status: 'in_progress', live: 'ok  pkg/a\nok  pkg/b\n', name: 'exec_command' })

    const big = run([delta('x'.repeat(LIVE_OUTPUT_LIMIT + 10))], s)
    expect((big.entries[0] as { live: string }).live).toHaveLength(LIVE_OUTPUT_LIMIT)

    const done = run(
      [ev({ type: 'item.completed', turn_id: 't', item_id: 'c', data: { kind: 'tool_call', status: 'completed', call_id: 'k', name: 'exec_command', arguments: '{}', output: 'Exit code: 0' } })],
      s,
    )
    expect(done.entries[0]).toMatchObject({ output: 'Exit code: 0', status: 'completed' })
    expect((done.entries[0] as { live?: string }).live).toBeUndefined()
  })
})

describe('context compaction', () => {
  const info = { reason: 'mid_turn' as const, items_before: 9, items_after: 2, tokens_before: 7300, tokens_after: 900 }

  it('streams the summary and shows the result', () => {
    seq = 0
    const s = run([
      ev({ type: 'item.started', turn_id: 't', item_id: 'cp', data: { kind: 'compaction', status: 'in_progress', compaction: { ...info, items_before: 0 } } }),
      ev({ type: 'item.delta', turn_id: 't', item_id: 'cp', data: { kind: 'compaction', text: '## 摘' } }),
      ev({ type: 'item.delta', turn_id: 't', item_id: 'cp', data: { kind: 'compaction', text: '要' } }),
    ])
    expect(s.entries[0]).toMatchObject({ type: 'compaction', status: 'in_progress', text: '## 摘要' })
    const done = run([ev({ type: 'item.completed', turn_id: 't', item_id: 'cp', data: { kind: 'compaction', status: 'completed', text: '## 摘要', compaction: info } })], s)
    expect(done.entries).toHaveLength(1)
    expect(done.entries[0]).toMatchObject({ status: 'completed', info })
  })

  it('tracks context tokens and keeps the last request when usage is zero', () => {
    seq = 0
    const last = { input_tokens: 7000, output_tokens: 60, total_tokens: 7060 }
    const zero = { input_tokens: 0, output_tokens: 0, total_tokens: 0 }
    const s = run([
      ev({ type: 'token.usage', data: { last, turn: last, context_window: 8000, context_tokens: 7060, auto_compact_limit: 7200 } }),
      ev({ type: 'token.usage', data: { last: zero, turn: last, context_window: 8000, context_tokens: 900, estimated: true, auto_compact_limit: 7200 } }),
    ])
    expect(s.usage).toMatchObject({ last, contextTokens: 900, estimated: true, autoCompactLimit: 7200 })
    expect(s.usage.session.total_tokens).toBe(7060)
  })

  it('hydrates compaction checkpoints', () => {
    const s = hydrate({
      id: 's', title: '', cwd: '/', created_at: '', updated_at: '', status: 'idle', last_seq: 3, turns: [],
      items: [
        { id: 'u1', turn_id: 't1', kind: 'user_message', text: 'goal' },
        { id: 'cp', turn_id: 't1', kind: 'compaction', text: 'summary', compaction: info },
      ],
    })
    expect(s.entries.map((e) => e.type)).toEqual(['user', 'compaction'])
    expect(s.entries[1]).toMatchObject({ text: 'summary', info, status: 'completed' })
  })
})
