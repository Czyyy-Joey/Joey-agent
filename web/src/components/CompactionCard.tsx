import { ChevronRight, Loader2, Shrink, XCircle } from 'lucide-react'
import type { CompactionReason } from '@/api/types'
import { formatTokens } from '@/lib/utils'
import type { Entry } from '@/state/reducer'

const reasonText: Record<CompactionReason, string> = {
  pre_turn: '开始前上下文接近上限',
  mid_turn: '工具输出让上下文接近上限',
  post_turn: '本轮结束时上下文较大',
  overflow: '模型报告上下文超限',
  manual: '手动 /compact',
}

type CompactionEntry = Extract<Entry, { type: 'compaction' }>

export function CompactionCard({ entry }: { entry: CompactionEntry }) {
  const info = entry.info
  const reason = info ? reasonText[info.reason] : undefined
  if (entry.status === 'in_progress') {
    return (
      <div role="status" className="rounded-lg border border-accent/40 px-3 py-2 text-xs">
        <div className="flex items-center gap-2 text-accent">
          <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
          正在压缩上下文…
          {reason && <span className="text-muted">（{reason}）</span>}
        </div>
        {entry.text && <p className="mt-1.5 line-clamp-4 whitespace-pre-wrap text-muted">{entry.text}</p>}
      </div>
    )
  }
  if (entry.status !== 'completed') {
    return (
      <div className="flex items-start gap-2 rounded-lg border border-warning/40 px-3 py-2 text-xs text-warning">
        <XCircle className="mt-px h-3.5 w-3.5 shrink-0" aria-hidden />
        <span className="break-words">
          上下文压缩{entry.status === 'interrupted' ? '已中断' : '失败'}
          {entry.text ? `：${entry.text}` : ''}
        </span>
      </div>
    )
  }
  return (
    <details className="group rounded-lg border border-border text-xs">
      <summary className="flex cursor-pointer list-none flex-wrap items-center gap-x-2 gap-y-1 px-3 py-2 text-muted">
        <ChevronRight className="h-3 w-3 transition-transform group-open:rotate-90" aria-hidden />
        <Shrink className="h-3.5 w-3.5 text-accent" aria-hidden />
        <span className="text-foreground">已压缩上下文</span>
        {info && (
          <span>
            {info.items_before} 条 / ~{formatTokens(info.tokens_before)} → {info.items_after} 条 / ~{formatTokens(info.tokens_after)} tokens
          </span>
        )}
        {reason && <span>· {reason}</span>}
        {info?.background && <span>· 使用后台提前生成的摘要</span>}
      </summary>
      <div className="border-t border-border px-3 py-2">
        <p className="mb-1 text-muted">模型之后看到的是保留的用户消息加上这份摘要：</p>
        <p className="whitespace-pre-wrap break-words">{entry.text}</p>
      </div>
    </details>
  )
}
