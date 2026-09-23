import { useCallback, useEffect, useState } from 'react'

import { ApiError } from '../api/errors'

export interface AsyncState<T> {
  data: T | null
  loading: boolean
  error: ApiError | null
  /** 手动重新拉一次（重试按钮、提交后刷新列表）。 */
  reload: () => void
}

/**
 * 拉一次远端数据，管好 loading / error / 竞态。
 *
 * # 竞态是这里的主要目的
 *
 * 用户在搜索框里连打三个字会发出三个请求，它们的返回顺序不保证。朴素写法
 * （直接 setState）会让最后渲染出来的是最先返回的那个——也就是最旧的关键词的结果，
 * 而且看起来完全正常，只是「搜索结果不对」。
 *
 * 这里用 AbortController 在依赖变化时掐掉上一次请求，再叠一个 alive 标志兜住
 * 「已经返回但还没 setState」的窗口。两者缺一不可：abort 管不到已经进入
 * 微任务队列的 resolve。
 *
 * deps 的语义与 useEffect 一致：变了就重新拉。fn 刻意不进依赖数组——页面里
 * 它多半是个内联箭头函数，每次渲染都是新的引用，进了依赖就是死循环。
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
