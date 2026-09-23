import type { ReactNode } from 'react'
import { Result } from 'antd'

import { useAuth } from '../contexts/AuthContext'

/**
 * 管理员路由门禁。
 *
 * 这**不是**安全边界。真正的权限判定在后端——把菜单藏起来只是少让普通用户点进
 * 一个必然失败的页面，任何人手输地址或直接调接口时，拦住他的是服务端。
 * 别因为这里有个门禁就以为后端可以少判一次。
 */
export function RequireAdmin({ children }: { children: ReactNode }) {
  const { isAdmin } = useAuth()

  if (!isAdmin) {
    return <Result status="403" title="403" subTitle="这个页面需要管理员权限" />
  }

  return <>{children}</>
}
