import { cn } from '@/lib/utils'

/** 按行着色显示 unified diff 或 apply_patch 补丁。 */
export function DiffView({ diff, className }: { diff: string; className?: string }) {
  const lines = diff.endsWith('\n') ? diff.slice(0, -1).split('\n') : diff.split('\n')
  return (
    <pre
      className={cn(
        'max-h-80 overflow-auto rounded-md border border-border bg-code p-2 font-mono text-[12px] leading-relaxed',
        className,
      )}
    >
      {lines.map((line, i) => (
        <div
          key={i}
          className={cn(
            'whitespace-pre-wrap break-words',
            line.startsWith('+') && !line.startsWith('+++') && 'bg-success/10 text-success',
            line.startsWith('-') && !line.startsWith('---') && 'bg-danger/10 text-danger',
            (line.startsWith('---') || line.startsWith('+++')) && 'font-medium text-foreground',
            (line.startsWith('***') || line.startsWith('@@') || line.startsWith('\\')) && 'text-muted',
          )}
        >
          {line || ' '}
        </div>
      ))}
    </pre>
  )
}
