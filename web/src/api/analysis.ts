import { api, type Page } from './client'
import { openEventStream } from './sse'
import type {
  DecisionChainView,
  ProgressView,
  ResultView,
  SubmitRequest,
  TaskView,
} from '../types/api'

export function submitTask(body: SubmitRequest): Promise<TaskView> {
  return api.post<TaskView>('/analysis/tasks', body)
}

export interface ListTasksParams {
  status?: string
  page?: number
  pageSize?: number
}

export function listTasks(
  params: ListTasksParams,
  signal?: AbortSignal,
): Promise<Page<TaskView>> {
  return api.get<Page<TaskView>>('/analysis/tasks', { query: { ...params }, signal })
}

export function getTask(id: string, signal?: AbortSignal): Promise<TaskView> {
  return api.get<TaskView>(`/analysis/tasks/${encodeURIComponent(id)}`, { signal })
}

export function getResult(id: string, signal?: AbortSignal): Promise<ResultView> {
  return api.get<ResultView>(`/analysis/tasks/${encodeURIComponent(id)}/result`, { signal })
}

export function getDecisionChain(
  id: string,
  signal?: AbortSignal,
): Promise<DecisionChainView> {
  return api.get<DecisionChainView>(
    `/analysis/tasks/${encodeURIComponent(id)}/decision-chain`,
    { signal },
  )
}

export function cancelTask(id: string): Promise<{ canceled?: boolean }> {
  return api.post<{ canceled?: boolean }>(`/analysis/tasks/${encodeURIComponent(id)}/cancel`)
}

/**
 * 任务状态，取值与 Go 侧 value_objects/status.go 一一对应。
 * 注意 canceled 是单 l 拼写，跟着后端走。
 */
export const TASK_STATUSES = ['queued', 'running', 'completed', 'failed', 'canceled'] as const
export type TaskStatus = (typeof TASK_STATUSES)[number]

/** 终态：到了这里就别再订阅进度，也别再轮询。 */
const TERMINAL_STATUSES = new Set<string>(['completed', 'failed', 'canceled'])

export function isTerminalStatus(status: string | undefined): boolean {
  return !!status && TERMINAL_STATUSES.has(status)
}

export interface ProgressSubscription {
  onProgress: (progress: ProgressView) => void
  /** 服务端通过 `event: error` 帧报告的业务失败，不是连接故障。 */
  onServerError?: (message: string) => void
  onOpen?: () => void
  signal: AbortSignal
}

/**
 * 订阅一个任务的进度流。
 *
 * 每一帧的 data 仍然是统一信封——后端刻意没有因为换了传输方式就绕开响应约定，
 * 所以这里要剥一层才能拿到 ProgressView。
 */
export function subscribeProgress(id: string, sub: ProgressSubscription): Promise<void> {
  return openEventStream(`/analysis/tasks/${encodeURIComponent(id)}/progress`, {
    signal: sub.signal,
    onOpen: sub.onOpen,
    onFrame: (frame) => {
      let envelope: { code?: number; message?: string; data?: ProgressView } | null = null
      try {
        envelope = JSON.parse(frame.data)
      } catch {
        // 半个帧不该让整条流挂掉。丢掉这一帧，下一帧会带来更新的进度快照——
        // 进度是全量快照而不是增量，所以丢帧不会让状态错乱。
        return
      }
      if (frame.event === 'error') {
        sub.onServerError?.(envelope?.message || '任务进度流报错')
        return
      }
      if (envelope?.data) sub.onProgress(envelope.data)
    },
  })
}
