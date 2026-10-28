import { useState } from 'react'
import { ShieldAlert, ShieldCheck, ShieldX } from 'lucide-react'
import type { ApprovalData, UserDecision } from '@/api/types'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'
import { DiffView } from './DiffView'

const decisionText: Record<string, string> = {
  approved: '已允许（仅这一次）',
  approved_for_session: '已允许（本会话不再询问）',
  denied: '已拒绝',
  abort: '已拒绝并停止任务',
  timed_out: '无人响应，已按拒绝处理',
  cancelled: '任务已中断，审批取消',
}

/** 工具执行前的审批卡片：展示要执行的命令或要修改的文件，等用户决定。 */
export function ApprovalCard({ approval, onDecide }: { approval: ApprovalData; onDecide: (d: UserDecision) => Promise<void> }) {
  const [busy, setBusy] = useState(false)
  const pending = !approval.decision
  const ok = approval.decision === 'approved' || approval.decision === 'approved_for_session'
  const decide = async (d: UserDecision) => {
    setBusy(true)
    try {
      await onDecide(d)
    } finally {
      setBusy(false)
    }
  }
  const Icon = pending ? ShieldAlert : ok ? ShieldCheck : ShieldX

  // 有了结果之后只留一行；命令和补丁内容在展开工具卡片的参数里仍然能看到。
  if (!pending) {
    return (
      <div className="flex items-center gap-2 border-t border-border bg-surface px-3 py-1.5 text-xs text-muted">
        <Icon className={cn('h-3.5 w-3.5 shrink-0', ok ? 'text-success' : 'text-danger')} aria-hidden />
        <span>{decisionText[approval.decision!] ?? approval.decision}</span>
        {approval.unsandboxed && ok && <span className="text-warning">· 在沙箱外执行</span>}
      </div>
    )
  }

  return (
    <div
      role={pending ? 'alertdialog' : undefined}
      aria-label={pending ? '需要审批' : undefined}
      className={cn(
        'space-y-2 border-t px-3 py-2.5',
        pending ? 'border-warning/40 bg-warning/5' : 'border-border bg-surface',
      )}
    >
      <div className="flex items-start gap-2 text-xs">
        <Icon className={cn('mt-px h-3.5 w-3.5 shrink-0', pending ? 'text-warning' : ok ? 'text-success' : 'text-danger')} aria-hidden />
        <div className="min-w-0 flex-1">
          <p className="font-medium text-foreground">
            {pending
              ? approval.kind === 'exec'
                ? approval.unsandboxed
                  ? '要在沙箱之外执行这条命令吗？'
                  : '要执行这条命令吗？'
                : '要修改这些文件吗？'
              : decisionText[approval.decision!] ?? approval.decision}
          </p>
          {approval.reason && <p className="mt-0.5 break-words text-muted">原因：{approval.reason}</p>}
        </div>
      </div>

      {approval.kind === 'exec' ? (
        <pre className="max-h-40 overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-code p-2 font-mono text-[12px]">
          <span className="select-none text-muted">{approval.cwd && approval.cwd !== '.' ? `${approval.cwd} $ ` : '$ '}</span>
          {approval.command}
        </pre>
      ) : (
        <div className="space-y-1">
          <p className="font-mono text-[12px] text-muted">{approval.paths?.join(', ')}</p>
          {approval.patch && <DiffView diff={approval.patch} className="max-h-64" />}
        </div>
      )}

      {approval.unsandboxed && pending && (
        <p className="text-xs text-warning">命令将不受沙箱限制，可以读写你账户能访问的所有文件，并且可以联网。</p>
      )}

      {pending && (
        <div className="flex flex-wrap gap-2 pt-0.5">
          <Button size="sm" disabled={busy} onClick={() => void decide('approved')}>
            允许一次
          </Button>
          <Button size="sm" variant="outline" disabled={busy} onClick={() => void decide('approved_for_session')}>
            本会话都允许
          </Button>
          <Button size="sm" variant="outline" disabled={busy} onClick={() => void decide('denied')}>
            拒绝
          </Button>
          <Button size="sm" variant="ghost" disabled={busy} onClick={() => void decide('abort')} className="text-danger">
            拒绝并停止
          </Button>
        </div>
      )}
    </div>
  )
}
