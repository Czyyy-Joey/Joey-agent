import { memo } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { cn } from '@/lib/utils'
import type { ItemStatus } from '@/api/types'

interface Props {
  text: string
  status: ItemStatus
}

export const AgentMessage = memo(function AgentMessage({ text, status }: Props) {
  const streaming = status === 'in_progress'
  const cut = status === 'interrupted' || status === 'failed'
  return (
    <div className={cn('prose-joey', cut && 'opacity-70')} aria-live={streaming ? 'polite' : undefined}>
      <ReactMarkdown remarkPlugins={[remarkGfm]}>{text}</ReactMarkdown>
      {streaming && (
        <span aria-hidden className="ml-0.5 inline-block h-4 w-1.5 translate-y-0.5 animate-pulse rounded-sm bg-accent" />
      )}
      {cut && (
        <p className="text-xs text-muted">{status === 'interrupted' ? '已中断，此段输出不会进入对话历史' : '输出失败，此段未进入对话历史'}</p>
      )}
    </div>
  )
})
