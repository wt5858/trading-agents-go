import type { ReactNode } from 'react'
import { Navigate, useLocation } from 'react-router-dom'
import { Spin } from 'antd'

import { useAuth } from '../contexts/AuthContext'

/**
 * 路由守卫：未登录就送去登录页。
 *
 * bootstrapping 必须单独处理。刷新页面时内存里的访问令牌是空的，要等那次静默续期
 * 跑完才知道用户到底登没登录——这期间把人踢去登录页的话，每次刷新都会先闪一下
 * 登录页再跳回来。
 */
export function RequireAuth({ children }: { children: ReactNode }) {
  const { user, bootstrapping } = useAuth()
  const location = useLocation()

  if (bootstrapping) {
    return <Spin style={{ display: 'block', marginTop: '20vh' }} />
  }

  if (!user) {
    // 把来路记下来，登录成功后送回去，而不是一律扔到首页。
    return <Navigate to="/login" replace state={{ from: location.pathname + location.search }} />
  }

  return <>{children}</>
}
