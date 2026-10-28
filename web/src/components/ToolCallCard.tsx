import { useState } from 'react'
import { Ban, Check, ChevronRight, FilePen, FileText, FolderOpen, Loader2, Search, ShieldAlert, Terminal, Wrench, X } from 'lucide-react'
import type { UserDecision } from '@/api/types'
import type { Entry } from '@/state/reducer'
import { cn, prettyJSON, summarizeArgs } from '@/lib/utils'
import { ApprovalCard } from './ApprovalCard'
import { DiffView } from './DiffView'

type ToolEntry = Extract<Entry, { type: 'tool' }>

const icons: Record<string, typeof Wrench> = {
  read_file: FileText,
  list_directory: FolderOpen,
  search_files: Search,
  write_file: FilePen,
  apply_patch: FilePen,
  exec_command: Terminal,
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

/** exec_command 只展示命令本身，其他工具展示参数摘要。 */
function headline(entry: ToolEntry): string {
  if (entry.name === 'exec_command') {
    try {
      return '$ ' + String((JSON.parse(entry.args) as { command?: string }).command ?? '')
    } catch {
      // 参数无效时退回到通用摘要。
    }
  }
  if (entry.name === 'apply_patch') {
    const files = [...entry.args.matchAll(/\*\*\* (?:Add|Update|Delete) File: ([^\\\n]+)/g)].map((m) => m[1])
    if (files.length) return files.join(', ')
  }
  return summarizeArgs(entry.args)
}

export function ToolCallCard({ entry, onDecide }: { entry: ToolEntry; onDecide: (approvalId: string, d: UserDecision) => Promise<void> }) {
  const [open, setOpen] = useState(false)
  const Icon = icons[entry.name] ?? Wrench
  const panelId = `tool-${entry.id}`
  const waiting = !!entry.approval && !entry.approval.decision
  const live = entry.status === 'in_progress' ? entry.live : undefined
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
        <span className="min-w-0 flex-1 truncate font-mono text-[12px] text-muted">{headline(entry)}</span>
        {entry.durationMs !== undefined && entry.status !== 'in_progress' && (
          <span className="shrink-0 text-xs tabular-nums text-muted">{entry.durationMs}ms</span>
        )}
        <span className="flex shrink-0 items-center gap-1 text-xs text-muted">
          {waiting ? <ShieldAlert className="h-3.5 w-3.5 text-warning" aria-hidden /> : <StatusIcon status={entry.status} />}
          <span>{waiting ? '等待审批' : statusLabel[entry.status] ?? entry.status}</span>
        </span>
      </button>
      {entry.approval && <ApprovalCard approval={entry.approval} onDecide={(d) => onDecide(entry.approval!.approval_id, d)} />}
      {live && !open && <LiveOutput text={live} />}
      {entry.diff && !open && (
        <div className="border-t border-border px-3 py-2">
          <DiffView diff={entry.diff} />
        </div>
      )}
      {open && (
        <div id={panelId} className="space-y-2 border-t border-border bg-surface px-3 py-2">
          {entry.diff && (
            <div>
              <div className="mb-1 text-[11px] font-medium uppercase tracking-wide text-muted">改动</div>
              <DiffView diff={entry.diff} />
            </div>
          )}
          <Section title="参数">{prettyJSON(entry.args)}</Section>
          {live && <Section title="实时输出">{live}</Section>}
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

/** 运行中的命令输出，固定高度并自动滚到底部。 */
function LiveOutput({ text }: { text: string }) {
  return (
    <pre
      ref={(el) => {
        if (el) el.scrollTop = el.scrollHeight
      }}
      aria-live="polite"
      className="max-h-48 overflow-auto whitespace-pre-wrap break-words border-t border-border bg-code px-3 py-2 font-mono text-[12px] leading-relaxed"
    >
      {text}
    </pre>
  )
}
