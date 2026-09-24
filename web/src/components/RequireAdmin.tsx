import type {ReactNode} from 'react'
import {Result} from 'antd'

import {useAuth} from '../contexts/AuthContext'

/**
 * 管理员路由门禁。
 *
 * 这**不是**安全边界——真正的判定在后端。藏菜单只是少让人点进一个必然失败的页面，
 * 别因为这里有门禁就以为服务端可以少判一次。
 */
export function RequireAdmin({ children }: { children: ReactNode }) {
  const { isAdmin } = useAuth()

  if (!isAdmin) {
    return <Result status="403" title="403" subTitle="这个页面需要管理员权限" />
  }

  return <>{children}</>
}
