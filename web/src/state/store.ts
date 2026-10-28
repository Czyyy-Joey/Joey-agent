import { create } from 'zustand'
import { api, ApiError } from '@/api/client'
import { subscribe, type ConnectionState } from '@/api/events'
import type { SessionSummary, UserDecision } from '@/api/types'
import { applyEvent, hydrate, initialState, markInterrupting, type ConversationState } from './reducer'

const STORAGE_KEY = 'joey.sessionId'

interface Store extends ConversationState {
  connection: ConnectionState
  /** 当前会话的工作目录与标题。 */
  cwd?: string
  title?: string
  sessions: SessionSummary[]
  bootError?: string
  actionError?: string
  init: () => Promise<void>
  send: (text: string) => Promise<boolean>
  stop: () => Promise<void>
  /** 在 cwd 中新建会话；cwd 为空时使用服务器默认目录。成功返回 true。 */
  newSession: (cwd?: string) => Promise<boolean>
  /**
   * 弹出系统目录选择器并在所选目录新建会话。返回 'fallback' 表示原生对话框
   * 不可用（例如浏览器不在 joey 所在的机器上），调用方应改用内置目录浏览。
   */
  pickAndCreate: () => Promise<'created' | 'cancelled' | 'fallback' | 'error'>
  /** 原生对话框打开期间为 true。 */
  picking: boolean
  selectSession: (id: string) => Promise<void>
  /** 删除会话。删的是当前会话时切换到列表里的下一个，没有就新建一个。成功返回 true。 */
  deleteSession: (id: string) => Promise<boolean>
  /** 回答一次审批。 */
  resolveApproval: (approvalId: string, decision: UserDecision) => Promise<void>
  refreshSessions: () => Promise<void>
}

let unsubscribe: (() => void) | undefined

function readStoredId(): string | undefined {
  try {
    return localStorage.getItem(STORAGE_KEY) ?? undefined
  } catch {
    return undefined
  }
}

function storeId(id: string) {
  try {
    localStorage.setItem(STORAGE_KEY, id)
  } catch {
    // 存储可能不可用（隐私模式）；刷新后会回到最近的会话。
  }
}

export const useConversation = create<Store>((set, get) => {
  async function refreshSessions() {
    try {
      const { sessions } = await api.listSessions()
      // 标题由服务器在第一轮提交时生成，借列表同步给当前会话。
      const current = sessions.find((x) => x.id === get().sessionId)
      set(current ? { sessions, title: current.title } : { sessions })
    } catch {
      // 列表只是导航辅助，失败不影响当前会话。
    }
  }

  // attach 先加载快照，然后在其后流式接收事件。
  async function attach(id: string) {
    unsubscribe?.()
    unsubscribe = undefined
    const info = await api.getSession(id)
    set({ ...hydrate(info), cwd: info.cwd, title: info.title, bootError: undefined, actionError: undefined })
    storeId(info.id)
    unsubscribe = subscribe(info.id, info.last_seq, {
      onEvent: (e) => {
        set((s) => applyEvent(s, e))
        if (e.type === 'turn.started' || e.type === 'turn.completed') void refreshSessions()
      },
      onState: (connection) => set({ connection }),
      onResync: () => void attach(info.id),
    })
  }

  return {
    ...initialState,
    connection: 'connecting',
    sessions: [],
    picking: false,

    init: async () => {
      try {
        const { sessions } = await api.listSessions()
        set({ sessions })
        const stored = readStoredId()
        const target = sessions.find((s) => s.id === stored)?.id ?? sessions[0]?.id
        if (target) {
          try {
            await attach(target)
            return
          } catch (err) {
            if (!(err instanceof ApiError && err.status === 404)) throw err
          }
        }
        await attach((await api.createSession()).id)
        await refreshSessions()
      } catch (err) {
        set({ bootError: err instanceof Error ? err.message : String(err) })
      }
    },

    send: async (text) => {
      const id = get().sessionId
      if (!id) return false
      try {
        await api.submitTurn(id, text)
        set({ actionError: undefined })
        return true
      } catch (err) {
        const msg =
          err instanceof ApiError && err.status === 409 && /already running/.test(err.message)
            ? '当前任务仍在运行，请先停止或等待完成'
            : (err as Error).message
        set({ actionError: msg })
        return false
      }
    },

    stop: async () => {
      const id = get().sessionId
      if (!id) return
      set((s) => markInterrupting(s))
      try {
        await api.interrupt(id)
      } catch (err) {
        set({ actionError: (err as Error).message })
      }
    },

    newSession: async (cwd) => {
      try {
        const info = await api.createSession(cwd)
        await attach(info.id)
        await refreshSessions()
        return true
      } catch (err) {
        set({ actionError: (err as Error).message })
        return false
      }
    },

    pickAndCreate: async () => {
      if (get().picking) return 'cancelled'
      set({ picking: true, actionError: undefined })
      try {
        const res = await api.pickFolder(get().cwd)
        if (!res.path) return 'cancelled'
        return (await get().newSession(res.path)) ? 'created' : 'error'
      } catch (err) {
        if (err instanceof ApiError && err.status === 501) return 'fallback'
        set({ actionError: (err as Error).message })
        return 'error'
      } finally {
        set({ picking: false })
      }
    },

    selectSession: async (id) => {
      if (id === get().sessionId) return
      try {
        await attach(id)
      } catch (err) {
        set({ actionError: (err as Error).message })
      }
    },

    deleteSession: async (id) => {
      try {
        await api.deleteSession(id)
      } catch (err) {
        if (!(err instanceof ApiError && err.status === 404)) {
          const msg = err instanceof ApiError && err.status === 409 ? '这个会话的任务还在运行，请先停止再删除' : (err as Error).message
          set({ actionError: msg })
          return false
        }
      }
      const sessions = get().sessions.filter((x) => x.id !== id)
      set({ sessions, actionError: undefined })
      if (id === get().sessionId) {
        unsubscribe?.()
        unsubscribe = undefined
        try {
          await attach(sessions[0]?.id ?? (await api.createSession()).id)
        } catch (err) {
          set({ actionError: (err as Error).message })
        }
      }
      await refreshSessions()
      return true
    },

    resolveApproval: async (approvalId, decision) => {
      const id = get().sessionId
      if (!id) return
      try {
        await api.resolveApproval(id, approvalId, decision)
        set({ actionError: undefined })
      } catch (err) {
        // 404：审批已经超时或轮次已结束，approval.resolved 事件会更新界面。
        if (!(err instanceof ApiError && err.status === 404)) set({ actionError: (err as Error).message })
      }
    },

    refreshSessions,
  }
})
