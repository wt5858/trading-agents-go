import {createContext, type ReactNode, useCallback, useContext, useEffect, useMemo, useState,} from 'react'

import {fetchMe, login as loginApi, logout as logoutApi} from '../api/auth'
import {ensureFreshAccessToken} from '../api/client'
import {getRefreshToken, subscribeTokens} from '../api/tokens'
import type {MeView} from '../types/api'

interface AuthState {
  user: MeView | null
  /** 启动时那次静默续期还没跑完。此时既不能当已登录，也不该跳登录页。 */
  bootstrapping: boolean
  login: (username: string, password: string) => Promise<void>
  logout: () => Promise<void>
  isAdmin: boolean
}

const AuthContext = createContext<AuthState | null>(null)

/**
 * 取当前登录态。
 *
 * 在 AuthProvider 外面调用直接抛错，而不是返回 null——后者会让「忘了包 Provider」
 * 表现为页面上莫名其妙的未登录状态，排查时要找很久。
 */
export function useAuth(): AuthState {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth 必须在 <AuthProvider> 内使用')
  return ctx
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<MeView | null>(null)
  const [bootstrapping, setBootstrapping] = useState(true)

  // 启动时的静默恢复。
  //
  // accessToken 只在内存里，刷新页面就没了；但 refreshToken 在 localStorage。
  // 这里拿它换一个新的访问令牌再取用户信息，用户就不必每次刷新都重新登录。
  useEffect(() => {
    let alive = true

    ;(async () => {
      if (!getRefreshToken()) {
        if (alive) setBootstrapping(false)
        return
      }
      try {
        await ensureFreshAccessToken()
        const me = await fetchMe()
        if (alive) setUser(me)
      } catch {
        // 续期失败（令牌过期/被吊销）时 client 已经清了令牌，这里只要保持未登录。
        // 不弹错误提示：用户只是打开了一个放了很久的标签页，这不是「出错」。
        if (alive) setUser(null)
      } finally {
        if (alive) setBootstrapping(false)
      }
    })()

    return () => {
      alive = false
    }
  }, [])

  // 令牌被别处清空时同步登出状态。
  //
  // 触发源是 client.ts 里 refresh 失败后的 clearTokens()——那条路径发生在
  // 任意一次后台请求中，它没法也不该直接操作 React 状态。用订阅把这件事接回来，
  // 路由守卫随即把用户送去登录页。
  useEffect(
    () =>
      subscribeTokens(() => {
        if (!getRefreshToken()) setUser(null)
      }),
    [],
  )

  const login = useCallback(async (username: string, password: string) => {
    const data = await loginApi(username, password)
    // 登录响应里带了 UserView，但它和 /auth/me 的 MeView 不是同一个结构。
    // 统一走 fetchMe，页面里就只需要认一种形状。
    setUser({
      userId: data.user?.id,
      username: data.user?.username,
      role: data.user?.role,
    })
  }, [])

  const logout = useCallback(async () => {
    await logoutApi()
    setUser(null)
  }, [])

  const value = useMemo<AuthState>(
    () => ({
      user,
      bootstrapping,
      login,
      logout,
      isAdmin: user?.role === 'admin',
    }),
    [user, bootstrapping, login, logout],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}
