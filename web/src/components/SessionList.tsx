import { useState } from 'react'
import { FolderOpen, Plus, Trash2 } from 'lucide-react'
import type { SessionSummary } from '@/api/types'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

interface Props {
  sessions: SessionSummary[]
  currentId?: string
  /** 系统目录选择器打开中。 */
  picking?: boolean
  onSelect: (id: string) => void
  onNew: () => void
  onDelete: (id: string) => Promise<boolean>
}

/** 目录名作为会话的副标题，例如 "/a/b/joey-agent" → "joey-agent"。 */
function baseName(p: string): string {
  const parts = p.split('/').filter(Boolean)
  return parts[parts.length - 1] ?? p
}

function relativeTime(iso: string): string {
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return ''
  const min = Math.round((Date.now() - t) / 60000)
  if (min < 1) return '刚刚'
  if (min < 60) return `${min} 分钟前`
  const h = Math.round(min / 60)
  if (h < 24) return `${h} 小时前`
  return new Date(t).toLocaleDateString()
}

export function SessionList({ sessions, currentId, picking, onSelect, onNew, onDelete }: Props) {
  // 删除需要点两次：第一次进入确认状态，第二次才真正删除。
  const [confirming, setConfirming] = useState<string>()
  const [deleting, setDeleting] = useState<string>()
  return (
    <nav aria-label="会话列表" className="flex h-full flex-col">
      <div className="p-2">
        <Button variant="outline" size="sm" className="w-full" onClick={onNew} disabled={picking} aria-busy={picking}>
          <Plus className="h-4 w-4" aria-hidden />
          {picking ? '请在弹出的窗口中选择文件夹…' : '新会话'}
        </Button>
      </div>
      <ul className="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
        {sessions.length === 0 && <li className="px-2 py-1 text-xs text-muted">还没有会话</li>}
        {sessions.map((s) => {
          const active = s.id === currentId
          return (
            <li key={s.id} className="group relative">
              <button
                type="button"
                onClick={() => onSelect(s.id)}
                aria-current={active ? 'page' : undefined}
                className={cn(
                  'flex w-full flex-col gap-0.5 rounded-lg py-2 pl-2.5 pr-8 text-left transition-colors',
                  active ? 'bg-surface ring-1 ring-border' : 'hover:bg-surface',
                )}
              >
                <span className="flex items-center gap-1.5">
                  {s.status === 'running' && <span aria-label="运行中" className="h-1.5 w-1.5 shrink-0 animate-pulse rounded-full bg-success" />}
                  <span className={cn('truncate', !s.title && 'text-muted')}>{s.title || '新会话'}</span>
                </span>
                <span className="flex items-center gap-1 text-xs text-muted" title={s.cwd}>
                  <FolderOpen className="h-3 w-3 shrink-0" aria-hidden />
                  <span className="truncate">{baseName(s.cwd)}</span>
                  <span className="ml-auto shrink-0">{relativeTime(s.updated_at)}</span>
                </span>
              </button>
              <DeleteButton
                title={s.title || '新会话'}
                running={s.status === 'running'}
                confirming={confirming === s.id}
                busy={deleting === s.id}
                onArm={() => setConfirming(s.id)}
                onCancel={() => setConfirming((c) => (c === s.id ? undefined : c))}
                onConfirm={async () => {
                  setDeleting(s.id)
                  try {
                    await onDelete(s.id)
                  } finally {
                    setDeleting(undefined)
                    setConfirming(undefined)
                  }
                }}
              />
            </li>
          )
        })}
      </ul>
    </nav>
  )
}

interface DeleteProps {
  title: string
  running: boolean
  confirming: boolean
  busy: boolean
  onArm: () => void
  onCancel: () => void
  onConfirm: () => Promise<void>
}

/** 会话右上角的删除按钮。悬停或聚焦时出现，点一次变成"确认删除"，再点才删除。 */
function DeleteButton({ title, running, confirming, busy, onArm, onCancel, onConfirm }: DeleteProps) {
  if (running) return null
  if (confirming) {
    return (
      <button
        type="button"
        autoFocus
        disabled={busy}
        onClick={() => void onConfirm()}
        onBlur={onCancel}
        onKeyDown={(e) => e.key === 'Escape' && onCancel()}
        aria-label={`确认删除会话「${title}」`}
        className="absolute right-1.5 top-1.5 rounded-md bg-danger px-2 py-0.5 text-xs font-medium text-white hover:opacity-90 disabled:opacity-60"
      >
        {busy ? '删除中…' : '确认删除'}
      </button>
    )
  }
  return (
    <button
      type="button"
      onClick={onArm}
      aria-label={`删除会话「${title}」`}
      title="删除会话"
      className="absolute right-1.5 top-1.5 rounded-md p-1 text-muted opacity-0 transition-opacity hover:bg-background hover:text-danger focus-visible:opacity-100 group-hover:opacity-100 [@media(pointer:coarse)]:opacity-100"
    >
      <Trash2 className="h-3.5 w-3.5" aria-hidden />
    </button>
  )
}
