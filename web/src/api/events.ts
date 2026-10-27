import { api } from './client'
import type { AgentEvent } from './types'

export type ConnectionState = 'connecting' | 'open' | 'reconnecting'

export interface EventHandlers {
  onEvent: (e: AgentEvent) => void
  /** 服务器不再保留错过的事件；重新加载快照。 */
  onResync: () => void
  onState: (s: ConnectionState) => void
}

/**
 * 订阅会话在 `after` 之后的事件。EventSource 会自行重连并发送
 * Last-Event-ID，因此服务器会从最后收到的 seq 继续。返回一个取消订阅函数。
 */
export function subscribe(sessionId: string, after: number, h: EventHandlers): () => void {
  const es = new EventSource(api.eventsURL(sessionId, after))
  h.onState('connecting')
  es.onopen = () => h.onState('open')
  es.onerror = () => h.onState('reconnecting')
  es.onmessage = (msg) => {
    try {
      h.onEvent(JSON.parse(msg.data) as AgentEvent)
    } catch (err) {
      console.error('bad event', err, msg.data)
    }
  }
  es.addEventListener('resync', () => h.onResync())
  return () => es.close()
}
