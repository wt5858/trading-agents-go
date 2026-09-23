import { Suspense, useMemo } from 'react'
import { Link, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { App, Badge, Button, Layout, Menu, Space, Spin, Typography } from 'antd'
import {
  BarChartOutlined,
  BellOutlined,
  ClockCircleOutlined,
  FileTextOutlined,
  FilterOutlined,
  LineChartOutlined,
  LogoutOutlined,
  SettingOutlined,
  StarOutlined,
  TeamOutlined,
  WalletOutlined,
} from '@ant-design/icons'

import { unreadCount } from '../api/notification'
import { useAuth } from '../contexts/AuthContext'
import { useAsyncData } from '../hooks/useAsyncData'

interface NavItem {
  key: string
  icon: React.ReactNode
  label: string
  adminOnly?: boolean
}

const NAV: NavItem[] = [
  { key: '/stocks', icon: <LineChartOutlined />, label: '股票' },
  { key: '/watchlist', icon: <StarOutlined />, label: '自选股' },
  { key: '/screening', icon: <FilterOutlined />, label: '选股筛选' },
  { key: '/analysis', icon: <BarChartOutlined />, label: '分析任务' },
  { key: '/reports', icon: <FileTextOutlined />, label: '报告' },
  { key: '/paper', icon: <WalletOutlined />, label: '模拟交易' },
  { key: '/scheduling', icon: <ClockCircleOutlined />, label: '定时任务' },
  { key: '/notifications', icon: <BellOutlined />, label: '通知' },
  { key: '/config', icon: <SettingOutlined />, label: '系统配置', adminOnly: true },
  { key: '/users', icon: <TeamOutlined />, label: '用户管理', adminOnly: true },
]

export function AppLayout() {
  const { user, isAdmin, logout } = useAuth()
  const location = useLocation()
  const navigate = useNavigate()
  const { message } = App.useApp()

  // 未读数只在切换路由时重新拉一次，不做轮询。
  // 这是个内部工具，为一个徽标挂一条定时请求不划算；真要实时，该走 SSE 而不是轮询。
  const unread = useAsyncData((signal) => unreadCount(signal), [location.pathname])

  const items = useMemo(
    () =>
      NAV.filter((item) => !item.adminOnly || isAdmin).map((item) => ({
        key: item.key,
        icon: item.icon,
        label: <Link to={item.key}>{item.label}</Link>,
      })),
    [isAdmin],
  )

  // 选中项按路径前缀匹配，而不是全等：/analysis/tasks/xxx 也该让「分析任务」高亮。
  // 取最长匹配，否则 /stocks 和 /stocks/600519 会同时选中。
  const selected = NAV.filter((item) => location.pathname.startsWith(item.key))
    .sort((a, b) => b.key.length - a.key.length)
    .slice(0, 1)
    .map((item) => item.key)

  const onLogout = async () => {
    try {
      await logout()
    } catch {
      // logout 内部已保证本地令牌被清掉，后端那次吊销失败不影响用户离开。
    }
    message.success('已登出')
    navigate('/login', { replace: true })
  }

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Layout.Sider breakpoint="lg" collapsedWidth={0} theme="light">
        <div style={{ padding: 16 }}>
          <Typography.Text strong>TradingAgents</Typography.Text>
        </div>
        <Menu mode="inline" selectedKeys={selected} items={items} />
      </Layout.Sider>

      <Layout>
        <Layout.Header
          style={{
            background: '#fff',
            display: 'flex',
            justifyContent: 'flex-end',
            alignItems: 'center',
            paddingInline: 16,
          }}
        >
          <Space size={16}>
            <Badge count={unread.data?.unread ?? 0} size="small">
              <Button
                type="text"
                icon={<BellOutlined />}
                aria-label={`通知，${unread.data?.unread ?? 0} 条未读`}
                onClick={() => navigate('/notifications')}
              />
            </Badge>
            <Typography.Text type="secondary">
              {user?.username}
              {user?.role ? `（${user.role}）` : ''}
            </Typography.Text>
            <Button icon={<LogoutOutlined />} onClick={onLogout}>
              登出
            </Button>
          </Space>
        </Layout.Header>

        <Layout.Content style={{ padding: 16 }}>
          {/* 路由是懒加载的，切页时这里兜一下，别让内容区闪成空白。 */}
          <Suspense fallback={<Spin style={{ display: 'block', marginTop: 64 }} />}>
            <Outlet />
          </Suspense>
        </Layout.Content>
      </Layout>
    </Layout>
  )
}
