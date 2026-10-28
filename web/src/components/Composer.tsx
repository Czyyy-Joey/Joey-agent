import { useEffect, useRef, useState, type KeyboardEvent } from 'react'
import { ArrowUp, Square } from 'lucide-react'
import { Button } from '@/components/ui/button'
import type { RunState } from '@/state/reducer'

interface Props {
  runState: RunState
  disabled: boolean
  onSend: (text: string) => Promise<boolean>
  onStop: () => void
}

export function Composer({ runState, disabled, onSend, onStop }: Props) {
  const [text, setText] = useState('')
  const ref = useRef<HTMLTextAreaElement>(null)
  const busy = runState !== 'idle'

  useEffect(() => {
    const el = ref.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${Math.min(el.scrollHeight, 200)}px`
  }, [text])

  useEffect(() => {
    if (!busy) ref.current?.focus()
  }, [busy])

  async function submit() {
    const value = text.trim()
    if (!value || busy || disabled) return
    if (await onSend(value)) setText('')
  }

  function onKeyDown(e: KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault()
      void submit()
    }
  }

  return (
    <div className="rounded-xl border border-border bg-background p-2 shadow-sm focus-within:border-accent">
      <label htmlFor="composer" className="sr-only">
        输入任务
      </label>
      <textarea
        id="composer"
        ref={ref}
        rows={1}
        value={text}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={onKeyDown}
        placeholder={busy ? 'Agent 正在执行… 按 Esc 停止' : '让 Joey 读文件、改代码…（Enter 发送，Shift+Enter 换行，/compact 压缩上下文）'}
        disabled={disabled}
        className="block max-h-[200px] w-full resize-none bg-transparent px-2 py-1.5 outline-none placeholder:text-muted"
      />
      <div className="flex justify-end">
        {busy ? (
          <Button variant="danger" size="sm" onClick={onStop} disabled={runState === 'interrupting'} aria-label="停止当前任务">
            <Square className="h-3.5 w-3.5 fill-current" aria-hidden />
            {runState === 'interrupting' ? '停止中…' : '停止'}
          </Button>
        ) : (
          <Button size="sm" onClick={() => void submit()} disabled={disabled || !text.trim()} aria-label="发送">
            <ArrowUp className="h-4 w-4" aria-hidden />
            发送
          </Button>
        )}
      </div>
    </div>
  )
}
