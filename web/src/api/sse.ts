import { API_PREFIX, ensureFreshAccessToken } from './client'
import { ApiError, BizCode } from './errors'
import { getAccessToken } from './tokens'

// 为什么不用浏览器原生的 EventSource
//
// 后端的鉴权是 Authorization: Bearer（internal/server/middleware.go 的 AuthRequired
// 只认这个头），而 EventSource 的构造函数不接受任何自定义请求头——这是规范层面的
// 限制，没有绕法。
//
// 常见的变通是让后端额外支持 ?token=，但那样访问令牌会进 access log、进 Referer、
// 进浏览器历史，是一次实打实的安全降级，为了省几十行解析代码不值得。
//
// 于是这里用 fetch + ReadableStream 自己读流、自己解析 SSE 帧。代价是 EventSource
// 自带的断线重连要自己实现（见下面的 retry 逻辑）。

export interface SSEFrame {
  /** 对应 `event:` 行；后端目前只发 progress 和 error 两种。 */
  event: string
  /** 对应 `data:` 行的原文。多行 data 已按规范用 \n 拼接。 */
  data: string
}

export interface EventStreamHandlers {
  onFrame: (frame: SSEFrame) => void
  /**
   * 连接建立时回调。用来把页面的「连接中」状态切掉。
   * 重连成功也会再调一次，所以它必须是幂等的。
   */
  onOpen?: () => void
}

export interface EventStreamOptions extends EventStreamHandlers {
  signal: AbortSignal
  /** 断线后是否自动重连。默认 true。 */
  retry?: boolean
  /** 最多重连几次，超过就放弃并抛错。默认 5。 */
  maxRetries?: number
}

/** 把一个完整帧的原文解析成 SSEFrame，注释行和空帧返回 null。 */
function parseFrame(raw: string): SSEFrame | null {
  let event = 'message'
  const dataLines: string[] = []

  for (const line of raw.split('\n')) {
    // 以冒号开头的是注释。后端的保活心跳 `: keep-alive` 走的就是这条——
    // 它存在的意义是让中间代理认为连接还活着，对客户端而言应当完全无视。
    if (line.startsWith(':')) continue

    const colon = line.indexOf(':')
    const field = colon === -1 ? line : line.slice(0, colon)
    // 规范规定字段值前若有一个空格要去掉，且只去一个。
    let value = colon === -1 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)

    if (field === 'event') event = value
    else if (field === 'data') dataLines.push(value)
    // id / retry 字段后端没用上，忽略。
  }

  if (dataLines.length === 0) return null
  return { event, data: dataLines.join('\n') }
}

/** 读一次流直到结束或出错。返回 true 表示服务端正常收流（不该重连）。 */
async function pumpOnce(path: string, handlers: EventStreamHandlers, signal: AbortSignal) {
  await ensureFreshAccessToken()

  const headers: Record<string, string> = { Accept: 'text/event-stream' }
  const token = getAccessToken()
  if (token) headers.Authorization = `Bearer ${token}`

  const res = await fetch(`${API_PREFIX}${path}`, { headers, signal })

  if (!res.ok || !res.body) {
    // 建连失败时后端回的仍是统一信封，把它解出来，别让用户看到「HTTP 404」。
    const envelope = (await res.json().catch(() => null)) as {
      code?: number
      message?: string
    } | null
    throw new ApiError({
      code: envelope?.code ?? BizCode.Internal,
      message: envelope?.message || `进度流连接失败（HTTP ${res.status}）`,
      httpStatus: res.status,
      requestId: res.headers.get('X-Request-ID'),
    })
  }

  handlers.onOpen?.()

  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader()
  let buffer = ''

  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break

      // \r\n 与 \r 都是合法换行，先归一化再按空行切帧，省得三种分隔符各写一遍。
      buffer += value.replace(/\r\n?/g, '\n')

      let split: number
      while ((split = buffer.indexOf('\n\n')) !== -1) {
        const raw = buffer.slice(0, split)
        buffer = buffer.slice(split + 2)
        const frame = parseFrame(raw)
        if (frame) handlers.onFrame(frame)
      }
      // 剩下的是半个帧，留在 buffer 里等下一个 chunk 补齐。
      // 不这么做的症状是进度偶尔跳一帧——正好被 TCP 切在中间的那些。
    }
  } finally {
    reader.releaseLock()
  }
}

/**
 * 订阅一条 SSE 流，直到服务端收流或调用方 abort。
 *
 * 服务端主动结束（任务跑完、被取消）与网络断开在 fetch 这一层是同一个现象——
 * 流读完了。这里的处理是：正常读完就返回，不重连；只有抛异常才算断线。
 * 后端在任务终结时会主动关流（analysis_handler.go 里判 progressFinished），
 * 所以这个判据是准的。
 */
export async function openEventStream(
  path: string,
  options: EventStreamOptions,
): Promise<void> {
  const { signal, retry = true, maxRetries = 5, ...handlers } = options
  let attempt = 0

  for (;;) {
    try {
      await pumpOnce(path, handlers, signal)
      return
    } catch (err) {
      if (signal.aborted) return // 调用方主动取消，不是故障
      if (err instanceof ApiError && (err.isUnauthorized || err.isNotFound || err.isForbidden)) {
        throw err // 重连也不会变好，直接抛给页面
      }
      if (!retry || attempt >= maxRetries) throw err

      attempt += 1
      // 指数退避，封顶 10 秒。后端重启时所有页面会同时断线，不退避就是一次自我 DDoS。
      const delay = Math.min(1000 * 2 ** (attempt - 1), 10_000)
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, delay)
        signal.addEventListener('abort', () => {
          clearTimeout(timer)
          resolve()
        }, { once: true })
      })
      if (signal.aborted) return
    }
  }
}
