import { ApiError, BizCode, networkError } from './errors'
import {
  clearTokens,
  getAccessToken,
  getRefreshToken,
  isAccessTokenStale,
  setTokens,
} from './tokens'

/** 所有业务接口都挂在这个前缀下（/mcp、/healthz、/swagger 不在其列）。 */
export const API_PREFIX = '/api/v1'

/** 后端统一响应信封，结构见 Go 侧 internal/helpers/response.Envelope。 */
interface Envelope<T> {
  code: number
  message: string
  data: T
}

/** 分页信封的 data 部分，对应 Go 侧 response.PageData。 */
export interface Page<T> {
  items: T[]
  total: number
  page: number
  pageSize: number
}

export type QueryParams = Record<
  string,
  string | number | boolean | undefined | null | Array<string | number>
>

/** 把查询参数拼成串。undefined / null 整个跳过——否则会发出 `?status=undefined`。 */
export function buildQuery(params?: QueryParams): string {
  if (!params) return ''
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '') continue
    if (Array.isArray(value)) {
      for (const item of value) search.append(key, String(item))
    } else {
      search.append(key, String(value))
    }
  }
  const qs = search.toString()
  return qs ? `?${qs}` : ''
}

// ---------------------------------------------------------------------------
// 令牌刷新：单飞
// ---------------------------------------------------------------------------

// 同一时刻只允许有一个刷新请求在途。
//
// 不做这件事的后果是具体的：页面一打开往往同时发四五个请求，令牌恰好过期时
// 它们会一起拿到 401、一起去刷新。而后端的 refresh 会轮换 refreshToken——
// 第一个请求换走之后，其余几个手上那个旧的立刻失效，于是用户在正常使用中
// 被随机踢出登录。让后来者共享第一个 Promise 就没有这个问题。
let inflightRefresh: Promise<void> | null = null

async function refreshAccessToken(): Promise<void> {
  if (inflightRefresh) return inflightRefresh

  inflightRefresh = (async () => {
    const refreshToken = getRefreshToken()
    if (!refreshToken) {
      clearTokens()
      throw new ApiError({
        code: BizCode.Unauthorized,
        message: '登录已失效，请重新登录',
        httpStatus: 401,
      })
    }

    let res: Response
    try {
      // 刻意用裸 fetch 而不是下面的 request()：request 在 401 时会来调本函数，
      // 走 request 就是一个无限递归。
      res = await fetch(`${API_PREFIX}/auth/refresh`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refreshToken }),
      })
    } catch (cause) {
      // 网络抖动不该把用户踢下线——令牌可能还是好的，保留它，让调用方自己失败重试。
      throw networkError(cause)
    }

    const body = (await res.json().catch(() => null)) as Envelope<{
      accessToken: string
      refreshToken: string
      expiresIn: number
    }> | null

    if (!res.ok || !body || body.code !== BizCode.OK || !body.data) {
      // 到这里是后端明确拒绝了这个 refreshToken（过期、已吊销、账号停用）。
      // 清空令牌会触发 tokens 的订阅者，AuthContext 据此把用户导去登录页。
      clearTokens()
      throw new ApiError({
        code: body?.code ?? BizCode.Unauthorized,
        message: body?.message || '登录已失效，请重新登录',
        httpStatus: res.status,
        requestId: res.headers.get('X-Request-ID'),
      })
    }

    setTokens({
      accessToken: body.data.accessToken,
      refreshToken: body.data.refreshToken,
      expiresIn: body.data.expiresIn,
    })
  })()

  try {
    await inflightRefresh
  } finally {
    inflightRefresh = null
  }
}

/**
 * 确保手上有一个没过期的访问令牌。
 *
 * 主动续期而不是等 401：等 401 意味着每次令牌过期都要浪费一个往返，而且 SSE
 * 这种长连接拿到 401 之后没法「重试一次」——连接已经建立又断掉，页面上会闪一下。
 */
export async function ensureFreshAccessToken(): Promise<void> {
  if (!isAccessTokenStale()) return
  if (!getRefreshToken()) return // 未登录，交给路由守卫处理
  await refreshAccessToken()
}

// ---------------------------------------------------------------------------
// 请求
// ---------------------------------------------------------------------------

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  /** 会被 JSON 序列化；不要自己传字符串。 */
  body?: unknown
  query?: QueryParams
  signal?: AbortSignal
  /** 置 true 时不带令牌、不做刷新重试。只有登录接口用得上。 */
  anonymous?: boolean
}

/**
 * 发一次请求并把信封剥掉，直接返回 data。
 *
 * 失败一律抛 ApiError——包括 HTTP 200 但 code 非 0 的情况。让调用方用 try/catch
 * 而不是每次都判一遍 code，是因为「忘了判 code」是静默的：页面会拿着一个
 * undefined 的 data 继续渲染，症状出现在离错误很远的地方。
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, query, signal, anonymous = false } = options

  if (!anonymous) {
    await ensureFreshAccessToken()
  }

  const send = async (): Promise<Response> => {
    const headers: Record<string, string> = {}
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    if (!anonymous) {
      const token = getAccessToken()
      if (token) headers.Authorization = `Bearer ${token}`
    }
    try {
      return await fetch(`${API_PREFIX}${path}${buildQuery(query)}`, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal,
      })
    } catch (cause) {
      // AbortError 是调用方主动取消（组件卸载、切换筛选条件），原样抛出，
      // 别包成 ApiError——调用方靠 err.name === 'AbortError' 来跳过错误提示。
      if (cause instanceof DOMException && cause.name === 'AbortError') throw cause
      throw networkError(cause)
    }
  }

  let res = await send()

  // 主动续期之后仍然 401，说明令牌在这次请求的途中失效了（被别处登出、会话吊销）。
  // 给一次、且只给一次重试机会：再失败就是真的没救了，继续重试只会刷屏。
  if (res.status === 401 && !anonymous) {
    await refreshAccessToken()
    res = await send()
  }

  // 204 没有响应体，也没有信封可剥。
  if (res.status === 204) return undefined as T

  const requestId = res.headers.get('X-Request-ID')
  const envelope = (await res.json().catch(() => null)) as Envelope<T> | null

  if (!envelope) {
    throw new ApiError({
      code: BizCode.Internal,
      message: `服务端返回了无法解析的响应（HTTP ${res.status}）`,
      httpStatus: res.status,
      requestId,
    })
  }

  if (envelope.code !== BizCode.OK) {
    throw new ApiError({
      code: envelope.code,
      message: envelope.message || '请求失败',
      httpStatus: res.status,
      requestId,
    })
  }

  return envelope.data
}

export const api = {
  get: <T>(path: string, options?: Omit<RequestOptions, 'method' | 'body'>) =>
    request<T>(path, { ...options, method: 'GET' }),
  post: <T>(path: string, body?: unknown, options?: Omit<RequestOptions, 'method' | 'body'>) =>
    request<T>(path, { ...options, method: 'POST', body }),
  put: <T>(path: string, body?: unknown, options?: Omit<RequestOptions, 'method' | 'body'>) =>
    request<T>(path, { ...options, method: 'PUT', body }),
  del: <T>(path: string, options?: Omit<RequestOptions, 'method' | 'body'>) =>
    request<T>(path, { ...options, method: 'DELETE' }),
}
