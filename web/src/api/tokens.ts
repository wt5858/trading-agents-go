// 令牌的存放位置是刻意分开的：
//
//   accessToken   只在内存里。它每次请求都会被带上，寿命只有 expiresIn（默认一小时），
//                 放进 localStorage 等于给 XSS 留一个长期可读的高权限凭据。
//   refreshToken  落 localStorage。不这么做就没法「刷新页面还保持登录」——
//                 而这是个每天要开十几次的内部工具，每次刷新都重新登录不可接受。
//
// 净效果：XSS 能偷到的只有 refreshToken。它确实也能换访问令牌，但至少后端能
// 通过吊销会话把它作废（AuthService 管着会话表），而已经签发出去的 accessToken
// 在过期前是拦不住的。
//
// 代价是刷新页面后内存里没有 accessToken，需要用 refreshToken 静默换一次——
// 这一步在 AuthContext 启动时做。

const REFRESH_KEY = 'ta.refreshToken'

let accessToken: string | null = null
/** accessToken 的到期时刻（毫秒时间戳）。用来在请求前主动续期，而不是等 401。 */
let accessExpiresAt = 0

type Listener = () => void
const listeners = new Set<Listener>()

function notify() {
  for (const fn of listeners) fn()
}

/** 订阅令牌变化。AuthContext 用它感知「refresh 失败被强制登出」这类外部触发的清空。 */
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
    // 隐私模式下 localStorage 可能直接抛异常。退化成「本次会话内有效」，
    // 而不是让整个应用打不开。
    return null
  }
}

/**
 * 写入一次登录/刷新的产物。
 *
 * skewSeconds 是故意提前的量：拿到令牌到它真正被用掉之间有网络延迟和时钟偏差，
 * 掐着到期时间用必然会撞上一批必然失败的请求。提前 30 秒当它已经过期，
 * 把这批请求变成一次主动续期。
 */
export function setTokens(params: {
  accessToken: string
  refreshToken: string
  expiresIn: number
}): void {
  const skewSeconds = 30
  accessToken = params.accessToken
  accessExpiresAt = Date.now() + Math.max(0, params.expiresIn - skewSeconds) * 1000
  try {
    localStorage.setItem(REFRESH_KEY, params.refreshToken)
  } catch {
    // 同上，存不进去只影响「刷新页面保持登录」，不影响当前会话。
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

/** 访问令牌是否已经过期（或从未拿到）。 */
export function isAccessTokenStale(): boolean {
  return !accessToken || Date.now() >= accessExpiresAt
}
