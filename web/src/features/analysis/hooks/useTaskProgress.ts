import { useEffect, useRef, useState } from 'react'

import { isTerminalStatus, subscribeProgress } from '../../../api/analysis'
import { ApiError } from '../../../api/errors'
import type { ProgressView } from '../../../types/api'

export interface TaskProgressState {
  progress: ProgressView | null
  /** 流已连上。用来把「连接中…」的占位切掉。 */
  connected: boolean
  /** 流层面的失败（重连耗尽、鉴权失败）。业务失败走任务自己的 status。 */
  streamError: string | null
  /** 流结束了。调用方据此重新拉一次任务详情，拿到终态和结果。 */
  finished: boolean
}

/**
 * 订阅一个任务的实时进度。
 *
 * 任务已经是终态时完全不建连——后端虽然会在这种情况下发一帧快照就关流，
 * 但那仍然是一次没必要的往返，而列表页跳进来时大部分任务都已经跑完了。
 */
export function useTaskProgress(
  taskId: string | undefined,
  status: string | undefined,
): TaskProgressState {
  const [progress, setProgress] = useState<ProgressView | null>(null)
  const [connected, setConnected] = useState(false)
  const [streamError, setStreamError] = useState<string | null>(null)
  const [finished, setFinished] = useState(false)

  // status 只用来决定「要不要建连」，不进依赖数组。
  //
  // 它会随着进度推进而变化（queued -> running），进了依赖就会在状态切换的瞬间
  // 把刚建好的连接拆掉重连——用 ref 读它的当前值，effect 只在 taskId 变时重跑。
  const statusRef = useRef(status)
  statusRef.current = status

  useEffect(() => {
    if (!taskId) return
    if (isTerminalStatus(statusRef.current)) {
      setFinished(true)
      return
    }

    const controller = new AbortController()
    let alive = true

    setConnected(false)
    setStreamError(null)
    setFinished(false)

    subscribeProgress(taskId, {
      signal: controller.signal,
      onOpen: () => {
        if (alive) setConnected(true)
      },
      onProgress: (next) => {
        if (alive) setProgress(next)
      },
      onServerError: (msg) => {
        if (alive) setStreamError(msg)
      },
    })
      .then(() => {
        // 正常收流 = 任务终结。让页面去重新拉一次详情拿最终结果。
        if (alive) setFinished(true)
      })
      .catch((err: unknown) => {
        if (!alive || controller.signal.aborted) return
        setStreamError(
          err instanceof ApiError ? err.message : '进度流断开，刷新页面可重新连接',
        )
      })
      .finally(() => {
        if (alive) setConnected(false)
      })

    return () => {
      alive = false
      // 组件卸载时必须 abort，否则这条 HTTP 连接会一直挂到后端超时——
      // 用户在任务列表里连点几个任务就能攒出一把泄漏的连接。
      controller.abort()
    }
  }, [taskId])

  return { progress, connected, streamError, finished }
}
