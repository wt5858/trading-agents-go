import { useState } from 'react'
import { Link } from 'react-router-dom'
import { App, Button, Card, List, Radio, Space, Tag, Typography } from 'antd'

import {
  deleteNotification,
  listNotifications,
  markAllRead,
  markRead,
  type ReadStatus,
} from '../../../api/notification'
import { ApiError } from '../../../api/errors'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import { TimeText } from '../../../components/TimeText'
import { useAsyncData } from '../../../hooks/useAsyncData'

const LEVEL_COLOR: Record<string, string> = {
  info: 'blue',
  warning: 'orange',
  error: 'red',
  success: 'green',
}

/**
 * 通知指向的实体 → 站内路径。
 *
 * 取值来自 notification/value_objects/link.go 的 LinkType 常量，一共四个：
 * 空串（系统公告，无跳转）、analysis_task、sync_run、scheduled_job。
 * 后端刻意只存 (类型, ID) 这一对标量而不内联实体快照——所以拼路由是前端的活。
 *
 * sync_run 目前没有对应页面，返回 null 而不是拼一个会 404 的地址：
 * 一个点了没反应的链接比没有链接更让人困惑。
 */
function linkTo(linkType: string | undefined, linkId: string | undefined): string | null {
  if (!linkId) return null
  switch (linkType) {
    case 'analysis_task':
      return `/analysis/tasks/${linkId}`
    case 'scheduled_job':
      return '/scheduling'
    default:
      return null
  }
}

export function NotificationsPage() {
  const { message } = App.useApp()
  const [status, setStatus] = useState<ReadStatus | ''>('')
  const [page, setPage] = useState(1)

  const { data, loading, error, reload } = useAsyncData(
    (signal) => listNotifications({ status, page, pageSize: 20 }, signal),
    [status, page],
  )

  const act = async (fn: () => Promise<unknown>) => {
    try {
      await fn()
      reload()
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '操作失败')
    }
  }

  return (
    <Card
      title="通知"
      extra={
        <Space>
          <Radio.Group
            value={status}
            onChange={(e) => {
              setStatus(e.target.value)
              setPage(1)
            }}
          >
            <Radio.Button value="">全部</Radio.Button>
            <Radio.Button value="unread">未读</Radio.Button>
            <Radio.Button value="read">已读</Radio.Button>
          </Radio.Group>
          <Button onClick={() => act(markAllRead)}>全部标为已读</Button>
        </Space>
      }
    >
      <AsyncBoundary
        loading={loading}
        error={error}
        onRetry={reload}
        isEmpty={!!data && data.items.length === 0}
        emptyText={status === 'unread' ? '没有未读通知' : '还没有通知'}
      >
        <List
          dataSource={data?.items ?? []}
          pagination={{
            current: page,
            pageSize: 20,
            total: data?.total ?? 0,
            onChange: setPage,
            showSizeChanger: false,
          }}
          renderItem={(n) => {
            const href = linkTo(n.linkType, n.linkId)
            return (
              <List.Item
                actions={[
                  !n.read && (
                    <Button key="read" size="small" type="link" onClick={() => act(() => markRead(n.id!))}>
                      标为已读
                    </Button>
                  ),
                  <Button
                    key="del"
                    size="small"
                    type="link"
                    danger
                    onClick={() => act(() => deleteNotification(n.id!))}
                  >
                    删除
                  </Button>,
                ].filter(Boolean)}
              >
                <List.Item.Meta
                  title={
                    <Space wrap>
                      {/* 未读加粗是这个列表唯一的视觉层次，去掉它就只能靠标签辨认。 */}
                      <Typography.Text strong={!n.read}>{n.title}</Typography.Text>
                      {n.level && (
                        <Tag color={LEVEL_COLOR[n.level] ?? 'default'}>{n.levelText || n.level}</Tag>
                      )}
                      {n.kindText && <Tag>{n.kindText}</Tag>}
                      {!n.read && <Tag color="blue">未读</Tag>}
                    </Space>
                  }
                  description={
                    <Space direction="vertical" size={2}>
                      <Typography.Text type="secondary">{n.body}</Typography.Text>
                      <Space size={12}>
                        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                          <TimeText value={n.createdAt} />
                        </Typography.Text>
                        {href && <Link to={href}>查看详情</Link>}
                      </Space>
                    </Space>
                  }
                />
              </List.Item>
            )
          }}
        />
      </AsyncBoundary>
    </Card>
  )
}
