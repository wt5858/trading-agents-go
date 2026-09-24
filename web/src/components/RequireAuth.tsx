import type {ReactNode} from 'react'
import {Navigate, useLocation} from 'react-router-dom'
import {Spin} from 'antd'

import {useAuth} from '../contexts/AuthContext'

/**
 * 路由守卫：未登录就送去登录页。
 *
 * bootstrapping 要单独处理——刷新页面时内存里没有访问令牌，得等静默续期跑完
 * 才知道登没登录；这期间就跳转的话，每次刷新都会先闪一下登录页再跳回来。
 */
export function RequireAuth({ children }: { children: ReactNode }) {
  const { user, bootstrapping } = useAuth()
  const location = useLocation()

  if (bootstrapping) {
    return <Spin style={{ display: 'block', marginTop: '20vh' }} />
  }

  if (!user) {
    // 记下来路，登录后送回去，而不是一律扔到首页。
    return <Navigate to="/login" replace state={{ from: location.pathname + location.search }} />
  }

  return <>{children}</>
}
