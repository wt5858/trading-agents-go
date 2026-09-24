import {useCallback, useEffect, useState} from 'react'

import {ApiError} from '../api/errors'

export interface AsyncState<T> {
  data: T | null
  loading: boolean
  error: ApiError | null
  reload: () => void
}

/**
 * 拉一次远端数据，管好 loading / error / 竞态。
 *
 * 竞态是主要目的：用户连打三个字会发出三个请求，返回顺序不保证，朴素写法会让
 * 最后渲染的是最旧关键词的结果——看起来完全正常，只是「搜索结果不对」。
 * AbortController 掐掉上一次请求，alive 标志兜住「已返回但还没 setState」的窗口，
 * 两者缺一不可（abort 管不到已进入微任务队列的 resolve）。
 *
 * fn 刻意不进依赖数组：页面里它多半是内联箭头函数，进了就是死循环。
 */
export function useAsyncData<T>(
  fn: (signal: AbortSignal) => Promise<T>,
  deps: React.DependencyList,
): AsyncState<T> {
  const [data, setData] = useState<T | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<ApiError | null>(null)
  const [nonce, setNonce] = useState(0)

  const reload = useCallback(() => setNonce((n) => n + 1), [])

  // deps 变了就在渲染期把旧数据清掉，而不是等 effect。
  //
  // effect 是 passive 的、在绘制之后才跑，所以「deps 已经变、loading 还是 false、
  // data 还是旧的」这一帧会真的被画出来——翻页时闪一下上一页的内容。
  // 这是 React 官方的「渲染期调整状态」模式：setState 会让本次渲染立刻作废重来，
  // 那一帧不会到达屏幕。
  const depsKey = JSON.stringify(deps)
  const [prevDepsKey, setPrevDepsKey] = useState(depsKey)
  if (depsKey !== prevDepsKey) {
    setPrevDepsKey(depsKey)
    setData(null)
    setError(null)
    setLoading(true)
  }

  useEffect(() => {
    const controller = new AbortController()
    let alive = true

    setLoading(true)
    setError(null)

    fn(controller.signal)
      .then((result) => {
        if (!alive) return
        setData(result)
      })
      .catch((err: unknown) => {
        if (!alive) return
        // 主动取消不是错误，别往页面上报。
        if (err instanceof DOMException && err.name === 'AbortError') return
        setError(
          err instanceof ApiError
            ? err
            : new ApiError({ code: 50000, message: '加载失败', httpStatus: 0 }),
        )
      })
      .finally(() => {
        if (alive) setLoading(false)
      })

    return () => {
      alive = false
      controller.abort()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce])

  return { data, loading, error, reload }
}
