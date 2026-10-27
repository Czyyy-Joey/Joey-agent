import { cn, formatTokens } from '@/lib/utils'
import type { ConversationState, RunState } from '@/state/reducer'
import type { ConnectionState } from '@/api/events'

const runLabel: Record<RunState, string> = { idle: '空闲', running: '运行中', interrupting: '停止中' }

export function RunStateBadge({ runState }: { runState: RunState }) {
  return (
    <span
      role="status"
      className={cn(
        'inline-flex items-center gap-1.5 rounded-full border border-border px-2 py-0.5 text-xs',
        runState === 'idle' ? 'text-muted' : 'text-foreground',
      )}
    >
      <span
        aria-hidden
        className={cn(
          'h-1.5 w-1.5 rounded-full',
          runState === 'idle' && 'bg-muted',
          runState === 'running' && 'animate-pulse bg-success',
          runState === 'interrupting' && 'bg-warning',
        )}
      />
      {runLabel[runState]}
    </span>
  )
}

export function StatusBar({ usage, connection }: { usage: ConversationState['usage']; connection: ConnectionState }) {
  const window = usage.contextWindow
  const lastInput = usage.last?.input_tokens ?? 0
  const pct = window ? Math.min(100, (lastInput / window) * 100) : 0
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-1 px-1 text-xs text-muted">
      <span title="最近一次请求的输入 token / 上下文窗口">
        上下文 {formatTokens(lastInput)}
        {window ? ` / ${formatTokens(window)}` : ''}
      </span>
      {window ? (
        <span
          className="h-1 w-24 overflow-hidden rounded-full bg-border"
          role="meter"
          aria-label="上下文占用"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(pct)}
        >
          <span
            className={cn('block h-full rounded-full', pct > 90 ? 'bg-danger' : pct > 70 ? 'bg-warning' : 'bg-accent')}
            style={{ width: `${pct}%` }}
          />
        </span>
      ) : null}
      <span>本轮 {formatTokens(usage.turn?.total_tokens ?? 0)} tokens</span>
      <span>会话累计 {formatTokens(usage.session.total_tokens)} tokens</span>
      {connection !== 'open' && (
        <span className="text-warning">{connection === 'connecting' ? '连接中…' : '事件流断开，正在重连…'}</span>
      )}
    </div>
  )
}
