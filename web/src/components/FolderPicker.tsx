import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react'
import { CornerLeftUp, Folder, FolderOpen, Home, X } from 'lucide-react'
import { api } from '@/api/client'
import type { DirListing } from '@/api/types'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

interface Props {
  open: boolean
  onClose: () => void
  /** 返回 true 表示会话已创建，对话框随之关闭。 */
  onPick: (cwd: string) => Promise<boolean>
}

/** 浏览服务器上允许的目录，并选择一个作为新会话的工作目录。 */
export function FolderPicker({ open, onClose, onPick }: Props) {
  const [listing, setListing] = useState<DirListing>()
  const [input, setInput] = useState('')
  const [hidden, setHidden] = useState(false)
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const dialogRef = useRef<HTMLDivElement>(null)

  const load = useCallback(
    async (path: string) => {
      try {
        const l = await api.listDirs(path, hidden)
        setListing(l)
        setInput(l.path)
        setError(undefined)
      } catch (err) {
        setError((err as Error).message)
      }
    },
    [hidden],
  )

  // 打开时定位到默认目录。
  useEffect(() => {
    if (!open) return
    let cancelled = false
    void (async () => {
      try {
        const roots = await api.listDirs('')
        if (!cancelled) await load(roots.default)
      } catch (err) {
        if (!cancelled) setError((err as Error).message)
      }
    })()
    return () => {
      cancelled = true
    }
    // 只在打开时执行；切换隐藏目录由下面的 effect 处理。
  }, [open])

  useEffect(() => {
    if (open && listing) void load(listing.path)
  }, [hidden])

  useEffect(() => {
    if (!open) return
    dialogRef.current?.focus()
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation()
        onClose()
      }
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [open, onClose])

  if (!open) return null

  const current = listing?.path ?? ''

  async function pick() {
    if (!current) return
    setBusy(true)
    const ok = await onPick(current)
    setBusy(false)
    if (ok) onClose()
  }

  function go(e: FormEvent) {
    e.preventDefault()
    void load(input.trim())
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby="folder-picker-title"
        tabIndex={-1}
        onMouseDown={(e) => e.stopPropagation()}
        className="flex max-h-[80vh] w-full max-w-lg flex-col rounded-xl border border-border bg-background shadow-xl outline-none"
      >
        <div className="flex items-center gap-2 border-b border-border px-4 py-3">
          <h2 id="folder-picker-title" className="flex-1 font-medium">
            选择工作目录
          </h2>
          <Button variant="ghost" size="icon" onClick={onClose} aria-label="关闭">
            <X className="h-4 w-4" aria-hidden />
          </Button>
        </div>

        <form onSubmit={go} className="flex gap-2 px-4 pt-3">
          <label htmlFor="folder-path" className="sr-only">
            目录路径
          </label>
          <input
            id="folder-path"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="/绝对路径 或 ~/项目"
            className="min-w-0 flex-1 rounded-lg border border-border bg-background px-2.5 py-1.5 font-mono text-xs outline-none focus:border-accent"
          />
          <Button type="submit" variant="outline" size="sm">
            前往
          </Button>
        </form>

        <div className="flex flex-wrap items-center gap-1 px-4 pt-2 text-xs">
          {listing?.roots.map((r) => (
            <button
              key={r.path}
              type="button"
              onClick={() => void load(r.path)}
              className="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-muted hover:bg-surface hover:text-foreground"
            >
              <Home className="h-3 w-3" aria-hidden />
              {r.name}
            </button>
          ))}
          <label className="ml-auto inline-flex cursor-pointer items-center gap-1 text-muted">
            <input type="checkbox" checked={hidden} onChange={(e) => setHidden(e.target.checked)} />
            显示隐藏目录
          </label>
        </div>

        {error && (
          <p role="alert" className="px-4 pt-2 text-xs text-danger">
            {error}
          </p>
        )}

        <ul className="mx-4 mt-2 min-h-40 flex-1 overflow-y-auto rounded-lg border border-border py-1" aria-label="子目录">
          {listing?.parent && (
            <li>
              <button
                type="button"
                onClick={() => void load(listing.parent)}
                className="flex w-full items-center gap-2 px-3 py-1.5 text-left hover:bg-surface"
              >
                <CornerLeftUp className="h-4 w-4 text-muted" aria-hidden />
                上一级
              </button>
            </li>
          )}
          {listing?.entries.map((d) => (
            <li key={d.path}>
              <button
                type="button"
                onClick={() => void load(d.path)}
                className="flex w-full items-center gap-2 px-3 py-1.5 text-left hover:bg-surface"
              >
                <Folder className="h-4 w-4 shrink-0 text-muted" aria-hidden />
                <span className="truncate">{d.name}</span>
              </button>
            </li>
          ))}
          {listing && listing.entries.length === 0 && <li className="px-3 py-1.5 text-xs text-muted">没有子目录</li>}
          {listing?.truncated && <li className="px-3 py-1.5 text-xs text-muted">目录过多，仅显示前 1000 个</li>}
        </ul>

        <div className="flex items-center gap-2 px-4 py-3">
          <FolderOpen className="h-4 w-4 shrink-0 text-muted" aria-hidden />
          <span className={cn('min-w-0 flex-1 truncate font-mono text-xs', !current && 'text-muted')} title={current}>
            {current || '请选择一个目录'}
          </span>
          <Button size="sm" onClick={() => void pick()} disabled={!current || busy}>
            在此目录新建会话
          </Button>
        </div>
      </div>
    </div>
  )
}
