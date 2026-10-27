// 与 internal/protocol 保持一致，两者需同步修改。

export type ItemKind = 'user_message' | 'agent_message' | 'reasoning' | 'tool_call' | 'tool_output' | 'turn_aborted'
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
}

export interface Item {
  id: string
  turn_id: string
  kind: ItemKind
  text?: string
  call?: ToolCall
  output?: ToolOutput
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
}

export interface MessageData {
  message: string
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
