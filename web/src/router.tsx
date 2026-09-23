import { lazy } from 'react'
import { createBrowserRouter, Navigate } from 'react-router-dom'
import { Button, Result } from 'antd'

import { AppLayout } from './components/AppLayout'
import { RequireAdmin } from './components/RequireAdmin'
import { RequireAuth } from './components/RequireAuth'
import { LoginPage } from './features/identity/pages/LoginPage'

// 业务页面一律 lazy。
//
// antd 全量进包之后首屏就有近 600 KB，把每个页面切出去能让首屏只加载登录页
// 用得上的那部分。登录页本身不 lazy——它是未登录用户必然要看的第一个页面，
// 懒加载只会给它多加一个往返。
const StockListPage = lazy(() =>
  import('./features/stock/pages/StockListPage').then((m) => ({ default: m.StockListPage })),
)
const StockDetailPage = lazy(() =>
  import('./features/stock/pages/StockDetailPage').then((m) => ({
    default: m.StockDetailPage,
  })),
)
const TaskListPage = lazy(() =>
  import('./features/analysis/pages/TaskListPage').then((m) => ({ default: m.TaskListPage })),
)
const TaskDetailPage = lazy(() =>
  import('./features/analysis/pages/TaskDetailPage').then((m) => ({
    default: m.TaskDetailPage,
  })),
)
const ReportListPage = lazy(() =>
  import('./features/report/pages/ReportListPage').then((m) => ({
    default: m.ReportListPage,
  })),
)
const ReportDetailPage = lazy(() =>
  import('./features/report/pages/ReportDetailPage').then((m) => ({
    default: m.ReportDetailPage,
  })),
)
const WatchlistPage = lazy(() =>
  import('./features/watchlist/pages/WatchlistPage').then((m) => ({ default: m.WatchlistPage })),
)
const JobListPage = lazy(() =>
  import('./features/scheduling/pages/JobListPage').then((m) => ({ default: m.JobListPage })),
)
const ScreeningPage = lazy(() =>
  import('./features/screening/pages/ScreeningPage').then((m) => ({ default: m.ScreeningPage })),
)
const PaperTradingPage = lazy(() =>
  import('./features/paper/pages/PaperTradingPage').then((m) => ({
    default: m.PaperTradingPage,
  })),
)
const NotificationsPage = lazy(() =>
  import('./features/notification/pages/NotificationsPage').then((m) => ({
    default: m.NotificationsPage,
  })),
)
const ConfigPage = lazy(() =>
  import('./features/config/pages/ConfigPage').then((m) => ({ default: m.ConfigPage })),
)
const UserListPage = lazy(() =>
  import('./features/identity/pages/UserListPage').then((m) => ({ default: m.UserListPage })),
)

function NotFound() {
  return (
    <Result
      status="404"
      title="404"
      subTitle="页面不存在"
      extra={
        <Button type="primary" href="/">
          回首页
        </Button>
      }
    />
  )
}

export const router = createBrowserRouter([
  { path: '/login', element: <LoginPage /> },
  {
    path: '/',
    element: (
      <RequireAuth>
        <AppLayout />
      </RequireAuth>
    ),
    children: [
      { index: true, element: <Navigate to="/analysis" replace /> },
      { path: 'stocks', element: <StockListPage /> },
      { path: 'stocks/:code', element: <StockDetailPage /> },
      { path: 'analysis', element: <TaskListPage /> },
      { path: 'analysis/tasks/:id', element: <TaskDetailPage /> },
      { path: 'reports', element: <ReportListPage /> },
      { path: 'reports/:id', element: <ReportDetailPage /> },
      { path: 'watchlist', element: <WatchlistPage /> },
      { path: 'screening', element: <ScreeningPage /> },
      { path: 'scheduling', element: <JobListPage /> },
      { path: 'paper', element: <PaperTradingPage /> },
      { path: 'notifications', element: <NotificationsPage /> },
      // 配置与用户管理是管理员功能。这里只做路由级的门禁，
      // 真正的权限判定在后端——前端藏起来只是少让人点错，不是安全边界。
      { path: 'config', element: <RequireAdmin><ConfigPage /></RequireAdmin> },
      { path: 'users', element: <RequireAdmin><UserListPage /></RequireAdmin> },
      { path: '*', element: <NotFound /> },
    ],
  },
])
