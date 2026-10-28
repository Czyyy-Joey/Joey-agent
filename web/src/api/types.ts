// 与 internal/protocol 保持一致，两者需同步修改。

export type ItemKind =
  | 'user_message'
  | 'agent_message'
  | 'reasoning'
  | 'tool_call'
  | 'tool_output'
  | 'turn_aborted'
  | 'compaction'
export type ItemStatus = 'in_progress' | 'completed' | 'failed' | 'aborted' | 'interrupted'
export type TurnStatus = 'running' | 'completed' | 'interrupted' | 'failed' | 'max_steps'

export interface Usage {
  input_tokens: number
  output_tokens: number
  total_tokens: number
}

export interface ToolCall {
  call_id: string
  name: string
  arguments: string
}

export interface ToolOutput {
  call_id: string
  content: string
  is_error?: boolean
  diff?: string
}

export type CompactionReason = 'pre_turn' | 'mid_turn' | 'post_turn' | 'overflow' | 'manual'

export interface Compaction {
  reason: CompactionReason
  /** 采用的是后台提前生成的摘要。 */
  background?: boolean
  items_before: number
  items_after: number
  tokens_before: number
  tokens_after: number
}

export interface Item {
  id: string
  turn_id: string
  kind: ItemKind
  text?: string
  call?: ToolCall
  output?: ToolOutput
  compaction?: Compaction
}

export interface ItemData {
  kind: ItemKind
  status?: ItemStatus
  text?: string
  call_id?: string
  name?: string
  arguments?: string
  output?: string
  is_error?: boolean
  duration_ms?: number
  diff?: string
  compaction?: Compaction
}

export interface TurnData {
  status: TurnStatus
  input?: string
  final_message?: string
  error?: string
  usage?: Usage
}

export interface UsageData {
  last: Usage
  turn: Usage
  context_window?: number
  /** 下一次请求的上下文大小（上游用量 + 新条目估算）。 */
  context_tokens?: number
  estimated?: boolean
  auto_compact_limit?: number
}

export interface MessageData {
  message: string
}

export type ReviewDecision = 'approved' | 'approved_for_session' | 'denied' | 'abort' | 'timed_out' | 'cancelled'
/** 用户可以主动给出的决定。 */
export type UserDecision = Extract<ReviewDecision, 'approved' | 'approved_for_session' | 'denied' | 'abort'>

export interface ApprovalData {
  approval_id: string
  call_id: string
  tool: string
  kind: 'exec' | 'patch'
  command?: string
  cwd?: string
  paths?: string[]
  patch?: string
  reason?: string
  unsandboxed?: boolean
  decision?: ReviewDecision
}

interface Envelope<T extends string, D> {
  seq: number
  session_id: string
  agent_id: string
  turn_id?: string
  item_id?: string
  type: T
  ts: string
  data: D
}

export type AgentEvent =
  | Envelope<'turn.started' | 'turn.completed', TurnData>
  | Envelope<'item.started' | 'item.delta' | 'item.completed', ItemData>
  | Envelope<'token.usage', UsageData>
  | Envelope<'warning' | 'error', MessageData>
  | Envelope<'approval.requested' | 'approval.resolved', ApprovalData>

export interface SessionSummary {
  id: string
  title: string
  cwd: string
  created_at: string
  updated_at: string
  status: 'idle' | 'running'
}

export interface TurnInfo {
  id: string
  status: TurnStatus
  error?: string
}

export interface SessionInfo extends SessionSummary {
  active_turn?: string
  last_turn_status?: TurnStatus
  last_seq: number
  items: Item[]
  turns: TurnInfo[]
}

export interface DirEntry {
  name: string
  path: string
}

export interface DirListing {
  /** 被列出的目录；列出根目录时为空字符串。 */
  path: string
  parent: string
  entries: DirEntry[]
  truncated?: boolean
  roots: DirEntry[]
  default: string
}
