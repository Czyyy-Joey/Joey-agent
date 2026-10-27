import { useState } from 'react'
import { Ban, Check, ChevronRight, FileText, FolderOpen, Loader2, Wrench, X } from 'lucide-react'
import type { Entry } from '@/state/reducer'
import { cn, prettyJSON, summarizeArgs } from '@/lib/utils'

type ToolEntry = Extract<Entry, { type: 'tool' }>

const icons: Record<string, typeof Wrench> = {
  read_file: FileText,
  list_directory: FolderOpen,
}

const statusLabel: Record<string, string> = {
  in_progress: '运行中',
  completed: '完成',
  failed: '失败',
  aborted: '已中止',
  interrupted: '已中断',
}

function StatusIcon({ status }: { status: ToolEntry['status'] }) {
  switch (status) {
    case 'in_progress':
      return <Loader2 className="h-3.5 w-3.5 animate-spin text-muted" aria-hidden />
    case 'completed':
      return <Check className="h-3.5 w-3.5 text-success" aria-hidden />
    case 'aborted':
    case 'interrupted':
      return <Ban className="h-3.5 w-3.5 text-warning" aria-hidden />
    default:
      return <X className="h-3.5 w-3.5 text-danger" aria-hidden />
  }
}

export function ToolCallCard({ entry }: { entry: ToolEntry }) {
  const [open, setOpen] = useState(false)
  const Icon = icons[entry.name] ?? Wrench
  const panelId = `tool-${entry.id}`
  return (
    <div className="overflow-hidden rounded-lg border border-border bg-background">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        aria-controls={panelId}
        className="flex w-full items-center gap-2 px-3 py-2 text-left hover:bg-surface"
      >
        <ChevronRight className={cn('h-3.5 w-3.5 shrink-0 text-muted transition-transform', open && 'rotate-90')} aria-hidden />
        <Icon className="h-3.5 w-3.5 shrink-0 text-muted" aria-hidden />
        <span className="font-mono text-[13px] font-medium">{entry.name}</span>
        <span className="min-w-0 flex-1 truncate font-mono text-[12px] text-muted">{summarizeArgs(entry.args)}</span>
        {entry.durationMs !== undefined && entry.status !== 'in_progress' && (
          <span className="shrink-0 text-xs tabular-nums text-muted">{entry.durationMs}ms</span>
        )}
        <span className="flex shrink-0 items-center gap-1 text-xs text-muted">
          <StatusIcon status={entry.status} />
          <span>{statusLabel[entry.status] ?? entry.status}</span>
        </span>
      </button>
      {open && (
        <div id={panelId} className="space-y-2 border-t border-border bg-surface px-3 py-2">
          <Section title="参数">{prettyJSON(entry.args)}</Section>
          {entry.output !== undefined && (
            <Section title="输出" error={entry.isError}>
              {entry.output}
            </Section>
          )}
        </div>
      )}
    </div>
  )
}

function Section({ title, error, children }: { title: string; error?: boolean; children: string }) {
  return (
    <div>
      <div className="mb-1 text-[11px] font-medium uppercase tracking-wide text-muted">{title}</div>
      <pre
        className={cn(
          'max-h-80 overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-code p-2 font-mono text-[12px] leading-relaxed',
          error && 'text-danger',
        )}
      >
        {children}
      </pre>
    </div>
  )
}
