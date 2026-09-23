import type { ReactNode } from 'react'
import { Alert, Button, Empty, Skeleton, Space } from 'antd'

import type { ApiError } from '../api/errors'

interface Props {
  loading: boolean
  error: ApiError | null
  /** 判空。列表页传 items.length === 0，详情页一般不传。 */
  isEmpty?: boolean
  emptyText?: string
  onRetry?: () => void
  children: ReactNode
}

/**
 * 把「加载中 / 出错 / 空」三态收在一处。
 *
 * 每个页面各写一遍的话，漏掉的永远是空态和错误态——它们在开发时不常出现，
 * 而用户第一次打开看到的恰恰是空态。
 */
export function AsyncBoundary({
  loading,
  error,
  isEmpty = false,
  emptyText = '暂无数据',
  onRetry,
  children,
}: Props) {
  // 首屏加载用骨架屏而不是转圈：高度稳定，不会在内容到达时整页跳一下。
  if (loading) return <Skeleton active paragraph={{ rows: 6 }} />

  if (error) {
    return (
      <Alert
        type="error"
        showIcon
        message="加载失败"
        description={
          <Space direction="vertical" size={4}>
            <span>{error.message}</span>
            {/* requestId 是拿去后端日志里搜的，出错时显示出来能省一轮来回问询。 */}
            {error.requestId && (
              <span style={{ fontSize: 12, opacity: 0.65 }}>
                请求编号：{error.requestId}
              </span>
            )}
          </Space>
        }
        action={
          onRetry ? (
            <Button size="small" onClick={onRetry}>
              重试
            </Button>
          ) : undefined
        }
      />
    )
  }

  if (isEmpty) return <Empty description={emptyText} />

  return <>{children}</>
}
