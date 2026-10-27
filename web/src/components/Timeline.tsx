import { useEffect, useRef } from 'react'
import { AlertTriangle, Brain, ChevronRight, CircleSlash, XCircle } from 'lucide-react'
import type { Entry } from '@/state/reducer'
import { AgentMessage } from './AgentMessage'
import { ToolCallCard } from './ToolCallCard'

const turnEndText: Record<string, string> = {
  interrupted: '任务已中断',
  failed: '任务失败',
  max_steps: '已达到最大步数，任务未完成',
}

function EntryView({ entry }: { entry: Entry }) {
  switch (entry.type) {
    case 'user':
      return (
        <div className="flex justify-end">
          <div className="max-w-[85%] whitespace-pre-wrap break-words rounded-2xl rounded-br-md bg-surface px-3.5 py-2 ring-1 ring-border">
            {entry.text}
          </div>
        </div>
      )
    case 'agent':
      return <AgentMessage text={entry.text} status={entry.status} />
    case 'reasoning':
      return (
        <details className="group text-muted">
          <summary className="flex cursor-pointer list-none items-center gap-1.5 text-xs">
            <ChevronRight className="h-3 w-3 transition-transform group-open:rotate-90" aria-hidden />
            <Brain className="h-3 w-3" aria-hidden />
            {entry.status === 'in_progress' ? '思考中…' : '思考过程'}
          </summary>
          <p className="mt-1 whitespace-pre-wrap border-l-2 border-border pl-3 text-xs">{entry.text}</p>
        </details>
      )
    case 'tool':
      return <ToolCallCard entry={entry} />
    case 'notice':
      return (
        <div
          role={entry.level === 'error' ? 'alert' : undefined}
          className={`flex items-start gap-2 rounded-lg border px-3 py-2 text-xs ${
            entry.level === 'error' ? 'border-danger/40 text-danger' : 'border-warning/40 text-warning'
          }`}
        >
          {entry.level === 'error' ? <XCircle className="mt-px h-3.5 w-3.5 shrink-0" aria-hidden /> : <AlertTriangle className="mt-px h-3.5 w-3.5 shrink-0" aria-hidden />}
          <span className="break-words">{entry.text}</span>
        </div>
      )
    case 'turn_end':
      return (
        <div className="flex items-center gap-2 text-xs text-muted">
          <span className="h-px flex-1 bg-border" />
          <CircleSlash className="h-3 w-3" aria-hidden />
          {turnEndText[entry.status] ?? entry.status}
          <span className="h-px flex-1 bg-border" />
        </div>
      )
  }
}

export function Timeline({ entries }: { entries: Entry[] }) {
  const endRef = useRef<HTMLDivElement>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const stick = useRef(true)

  // 自动跟随新输出，除非用户向上滚动查看历史。
  useEffect(() => {
    if (stick.current) endRef.current?.scrollIntoView({ block: 'end' })
  }, [entries])

  return (
    <div
      ref={containerRef}
      onScroll={(e) => {
        const el = e.currentTarget
        stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80
      }}
      className="min-h-0 flex-1 overflow-y-auto"
    >
      <div className="mx-auto flex max-w-[760px] flex-col gap-4 px-4 py-6">
        {entries.length === 0 ? (
          <div className="mt-24 text-center text-muted">
            <p className="text-base font-medium text-foreground">Joey Agent</p>
            <p className="mt-1">试试：「列出当前目录，然后读取 README.md 并总结」</p>
          </div>
        ) : (
          entries.map((e) => <EntryView key={e.id} entry={e} />)
        )}
        <div ref={endRef} />
      </div>
    </div>
  )
}
