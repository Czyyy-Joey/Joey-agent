import { useEffect, useState } from 'react'
import { FolderOpen, PanelLeft } from 'lucide-react'
import { useConversation } from '@/state/store'
import { Button } from '@/components/ui/button'
import { Composer } from '@/components/Composer'
import { FolderPicker } from '@/components/FolderPicker'
import { SessionList } from '@/components/SessionList'
import { RunStateBadge, StatusBar } from '@/components/StatusBar'
import { Timeline } from '@/components/Timeline'

export default function App() {
  const s = useConversation()
  const { init, runState, stop } = s
  const [pickerOpen, setPickerOpen] = useState(false)
  const [sidebarOpen, setSidebarOpen] = useState(false)

  useEffect(() => {
    void init()
  }, [init])

  // 页面任意位置按 Esc 均可停止正在运行的回合。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && runState === 'running') void stop()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [runState, stop])

  if (s.bootError) {
    return (
      <div className="flex h-full items-center justify-center p-4">
        <div role="alert" className="max-w-md rounded-lg border border-border p-4">
          <p className="font-medium">无法连接 Joey 后端</p>
          <p className="mt-1 text-sm text-muted">{s.bootError}</p>
          <p className="mt-2 text-sm text-muted">
            请确认已运行 <code className="font-mono">joey serve</code>（默认 127.0.0.1:8080）。
          </p>
          <Button className="mt-3" size="sm" onClick={() => void init()}>
            重试
          </Button>
        </div>
      </div>
    )
  }

  const sidebar = (
    <SessionList
      sessions={s.sessions}
      currentId={s.sessionId}
      picking={s.picking}
      onSelect={(id) => {
        setSidebarOpen(false)
        void s.selectSession(id)
      }}
      onNew={() => {
        setSidebarOpen(false)
        void s.pickAndCreate().then((r) => {
          if (r === 'fallback') setPickerOpen(true)
        })
      }}
    />
  )

  return (
    <div className="flex h-full">
      <aside className="hidden w-64 shrink-0 border-r border-border md:block">{sidebar}</aside>
      {sidebarOpen && (
        <div className="fixed inset-0 z-40 bg-black/40 md:hidden" onClick={() => setSidebarOpen(false)}>
          <aside className="h-full w-72 max-w-[85vw] border-r border-border bg-background" onClick={(e) => e.stopPropagation()}>
            {sidebar}
          </aside>
        </div>
      )}

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-12 shrink-0 items-center gap-3 border-b border-border px-4">
          <Button variant="ghost" size="icon" className="md:hidden" onClick={() => setSidebarOpen(true)} aria-label="打开会话列表">
            <PanelLeft className="h-4 w-4" aria-hidden />
          </Button>
          <span className="truncate font-semibold">{s.title || 'Joey'}</span>
          <RunStateBadge runState={runState} />
          {s.cwd && (
            <span className="ml-auto hidden min-w-0 items-center gap-1 font-mono text-xs text-muted sm:flex" title={s.cwd}>
              <FolderOpen className="h-3.5 w-3.5 shrink-0" aria-hidden />
              <span className="truncate">{s.cwd}</span>
            </span>
          )}
        </header>

        <Timeline entries={s.entries} />

        <footer className="shrink-0 border-t border-border bg-background">
          <div className="mx-auto flex max-w-[760px] flex-col gap-2 px-4 py-3">
            {s.actionError && (
              <p role="alert" className="text-xs text-danger">
                {s.actionError}
              </p>
            )}
            <Composer runState={runState} disabled={!s.sessionId} onSend={s.send} onStop={() => void stop()} />
            <StatusBar usage={s.usage} connection={s.connection} />
          </div>
        </footer>
      </div>

      <FolderPicker open={pickerOpen} onClose={() => setPickerOpen(false)} onPick={(cwd) => s.newSession(cwd)} />
    </div>
  )
}
