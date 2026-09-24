import {useEffect, useRef, useState} from 'react'

import {isTerminalStatus, subscribeProgress} from '../../../api/analysis'
import {ApiError} from '../../../api/errors'
import type {ProgressView} from '../../../types/api'

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
 * status 进依赖数组，不是用 ref 读一次。
 *
 * 原先用 ref 是为了避免「状态从 queued 变 running 时把刚建好的连接拆掉重连」，
 * 但那带来两个真问题：一是首次挂载时详情还没拉回来、status 是 undefined，
 * 于是「终态不建连」这道守卫恒为假——每次打开一个早就跑完的任务都白建一条 SSE；
 * 二是任务在观看过程中进入终态时，这个 effect 不会重跑，连接不会被拆掉。
 *
 * 改成进依赖之后，代价是 queued→running 会重连一次（后端会立刻推一帧当前快照，
 * 用户看不出来），换来的是连接生命周期与任务状态真正对齐。
 */
export function useTaskProgress(
  taskId: string | undefined,
  status: string | undefined,
): TaskProgressState {
  const [progress, setProgress] = useState<ProgressView | null>(null)
  const [connected, setConnected] = useState(false)
  const [streamError, setStreamError] = useState<string | null>(null)
  const [finished, setFinished] = useState(false)

  // 已经收到过终局帧。用 ref 而不是 state：它只用来阻止后续重连，
  // 不需要触发渲染，进 state 反而会多一轮。
  const sawFinalRef = useRef(false)

  useEffect(() => {
    if (!taskId) return

    // 详情还没回来时 status 是 undefined，此时先不建连，等它到位。
    // 这样既不会对已终结的任务白建连接，也不会漏掉真正在跑的任务。
    if (status === undefined) return

    if (isTerminalStatus(status)) {
      setFinished(true)
      setConnected(false)
      return
    }

    const controller = new AbortController()
    let alive = true

    setConnected(false)
    setStreamError(null)
    if (!sawFinalRef.current) setFinished(false)

    subscribeProgress(taskId, {
      signal: controller.signal,
      onOpen: () => {
        if (alive) setConnected(true)
      },
      onProgress: (next) => {
        if (!alive) return
        setProgress(next)
        // 后端在终局那一帧会置 final。据此立刻收口，不必等流关闭——
        // 也不必等 HTTP 层超时。
        if (next.final) {
          sawFinalRef.current = true
          setFinished(true)
        }
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
      // 卸载或状态变化时必须 abort，否则这条 HTTP 连接会一直挂到后端超时——
      // 用户在任务列表里连点几个任务就能攒出一把泄漏的连接。
      controller.abort()
    }
  }, [taskId, status])

  return { progress, connected, streamError, finished }
}
