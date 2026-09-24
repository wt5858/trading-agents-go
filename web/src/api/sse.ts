import {API_PREFIX, ensureFreshAccessToken} from './client'
import {ApiError, BizCode} from './errors'
import {getAccessToken} from './tokens'

// 不用原生 EventSource 是因为它的构造函数不接受任何自定义请求头（规范限制），
// 而后端鉴权认的是 Authorization: Bearer。
// 让后端改成支持 ?token= 会让访问令牌进 access log 和浏览器历史，是实打实的
// 安全降级，为省几十行解析代码不值得。代价是断线重连要自己实现。

export interface SSEFrame {
  event: string
  data: string
}

export interface EventStreamHandlers {
  onFrame: (frame: SSEFrame) => void
  /** 连接建立时回调。重连成功也会再调一次，所以必须幂等。 */
  onOpen?: () => void
}

export interface EventStreamOptions extends EventStreamHandlers {
  signal: AbortSignal
  retry?: boolean
  maxRetries?: number
}

function parseFrame(raw: string): SSEFrame | null {
  let event = 'message'
  const dataLines: string[] = []

  for (const line of raw.split('\n')) {
    // 注释行。后端的保活心跳 `: keep-alive` 走这条，客户端应完全无视。
    if (line.startsWith(':')) continue

    const colon = line.indexOf(':')
    const field = colon === -1 ? line : line.slice(0, colon)
    // 规范规定字段值前若有一个空格要去掉，且只去一个。
    let value = colon === -1 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)

    if (field === 'event') event = value
    else if (field === 'data') dataLines.push(value)
  }

  if (dataLines.length === 0) return null
  return { event, data: dataLines.join('\n') }
}

async function pumpOnce(path: string, handlers: EventStreamHandlers, signal: AbortSignal) {
  await ensureFreshAccessToken()

  const headers: Record<string, string> = { Accept: 'text/event-stream' }
  const token = getAccessToken()
  if (token) headers.Authorization = `Bearer ${token}`

  const res = await fetch(`${API_PREFIX}${path}`, { headers, signal })

  if (!res.ok || !res.body) {
    // 建连失败时后端回的仍是统一信封，解出来，别让用户看到「HTTP 404」。
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
 * 正常读完就返回、不重连；只有抛异常才算断线。后端在任务终结时会主动关流，
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
      await pumpOnce(
        path,
        {
          ...handlers,
          onOpen: () => {
            // 连上就清零。不清的话 maxRetries 变成「整条流生命周期内的断线总次数」，
            // 一个跑半小时的分析累计断 5 次之后就再也不重连了——而此时网络是好的。
            attempt = 0
            handlers.onOpen?.()
          },
        },
        signal,
      )
      return
    } catch (err) {
      if (signal.aborted) return
      if (err instanceof ApiError && (err.isUnauthorized || err.isNotFound || err.isForbidden)) {
        throw err // 重连也不会变好
      }
      if (!retry || attempt >= maxRetries) throw err

      attempt += 1
      // 指数退避封顶 10 秒。后端重启时所有页面会同时断线，不退避就是自我 DDoS。
      const delay = Math.min(1000 * 2 ** (attempt - 1), 10_000)
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, delay)
        signal.addEventListener(
          'abort',
          () => {
            clearTimeout(timer)
            resolve()
          },
          { once: true },
        )
      })
      if (signal.aborted) return
    }
  }
}
