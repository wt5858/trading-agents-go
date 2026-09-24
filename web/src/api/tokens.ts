// accessToken 只在内存、refreshToken 落 localStorage。
//
// 这么分是权衡的结果：全放 localStorage 等于给 XSS 留一个长期可读的高权限凭据；
// 全放内存则每次刷新页面都要重新登录，对一个每天开十几次的内部工具不可接受。
// 净效果是 XSS 能偷到的只有 refreshToken，而它至少能被后端吊销。
// 代价是刷新页面后要用 refreshToken 静默换一次，这一步在 AuthContext 启动时做。

const REFRESH_KEY = 'ta.refreshToken'

let accessToken: string | null = null
let accessExpiresAt = 0

type Listener = () => void
const listeners = new Set<Listener>()

function notify() {
  for (const fn of listeners) fn()
}

/** 订阅令牌变化。AuthContext 用它感知「refresh 失败被强制登出」这类外部清空。 */
export function subscribeTokens(fn: Listener): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

export function getAccessToken(): string | null {
  return accessToken
}

export function getRefreshToken(): string | null {
  try {
    return localStorage.getItem(REFRESH_KEY)
  } catch {
    // 隐私模式下 localStorage 可能直接抛。退化成「本次会话内有效」，
    // 而不是让整个应用打不开。
    return null
  }
}

export function setTokens(params: {
  accessToken: string
  refreshToken: string
  expiresIn: number
}): void {
  // 提前 30 秒判过期：掐着到期时间用必然会撞上一批注定失败的请求，
  // 把它们变成一次主动续期。
  const skewSeconds = 30
  accessToken = params.accessToken
  accessExpiresAt = Date.now() + Math.max(0, params.expiresIn - skewSeconds) * 1000
  try {
    localStorage.setItem(REFRESH_KEY, params.refreshToken)
  } catch {
    // 同上，存不进去只影响「刷新页面保持登录」。
  }
  notify()
}

export function clearTokens(): void {
  accessToken = null
  accessExpiresAt = 0
  try {
    localStorage.removeItem(REFRESH_KEY)
  } catch {
    // ignore
  }
  notify()
}

export function isAccessTokenStale(): boolean {
  return !accessToken || Date.now() >= accessExpiresAt
}
