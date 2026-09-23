import { useState } from 'react'
import { App, Button, Card, Input, Popconfirm, Space, Table, Tag } from 'antd'
import type { ColumnsType } from 'antd/es/table'

import { deactivateUser, listUsers, resetUserPassword } from '../../../api/user'
import { ApiError } from '../../../api/errors'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import { useAsyncData } from '../../../hooks/useAsyncData'
import { useAuth } from '../../../contexts/AuthContext'
import type { UserView } from '../../../types/api'

export function UserListPage() {
  const { message, modal } = App.useApp()
  const { user: me } = useAuth()
  const [keyword, setKeyword] = useState('')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)

  const { data, loading, error, reload } = useAsyncData(
    (signal) => listUsers({ keyword, page, pageSize }, signal),
    [keyword, page, pageSize],
  )

  const onReset = async (id: number) => {
    try {
      const result = (await resetUserPassword(id)) as { password?: string } | null
      // 重置后的临时密码只在这一次响应里出现，刷新页面就再也拿不到了。
      // 用 modal 而不是 message：后者会自己消失，管理员还没来得及复制。
      modal.info({
        title: '密码已重置',
        content: result?.password
          ? `临时密码：${result.password}（只显示这一次，请立即转交给用户）`
          : '密码已重置，请通过其他渠道告知用户。',
      })
      reload()
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '重置失败')
    }
  }

  const columns: ColumnsType<UserView> = [
    { title: 'ID', dataIndex: 'id', width: 80 },
    { title: '用户名', dataIndex: 'username', width: 160 },
    { title: '邮箱', dataIndex: 'email', ellipsis: true },
    {
      title: '角色',
      width: 100,
      render: (_, row) => (
        <Tag color={row.role === 'admin' ? 'gold' : 'default'}>{row.role}</Tag>
      ),
    },
    {
      title: '状态',
      width: 100,
      render: (_, row) => (
        <Tag color={row.active ? 'success' : 'default'}>{row.active ? '正常' : '已停用'}</Tag>
      ),
    },
    { title: '并发上限', dataIndex: 'concurrentLimit', width: 100 },
    {
      title: '操作',
      width: 200,
      render: (_, row) => {
        // 停用自己会当场把自己锁在门外。禁掉这个按钮比让后端拒绝更好——
        // 用户根本不该看到一个点了就出事的入口。
        const isSelf = row.id === me?.userId
        return (
          <Space size={4}>
            <Popconfirm
              title="重置该用户的密码？"
              description="旧密码立即失效。"
              onConfirm={() => onReset(row.id!)}
            >
              <Button size="small">重置密码</Button>
            </Popconfirm>
            <Popconfirm
              title="停用这个账号？"
              description="该用户将无法登录，在途会话也会失效。"
              onConfirm={async () => {
                try {
                  await deactivateUser(row.id!)
                  message.success('已停用')
                  reload()
                } catch (err) {
                  message.error(err instanceof ApiError ? err.message : '停用失败')
                }
              }}
            >
              <Button size="small" danger disabled={isSelf || !row.active}>
                {isSelf ? '不能停用自己' : '停用'}
              </Button>
            </Popconfirm>
          </Space>
        )
      },
    },
  ]

  return (
    <Card title="用户管理">
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <Input.Search
          aria-label="按用户名搜索"
          placeholder="用户名关键字"
          allowClear
          style={{ width: 280 }}
          onSearch={(value) => {
            setKeyword(value)
            setPage(1)
          }}
        />

        <AsyncBoundary
          loading={loading}
          error={error}
          onRetry={reload}
          isEmpty={!!data && data.items.length === 0}
          emptyText={keyword ? `没有匹配「${keyword}」的用户` : '没有用户'}
        >
          <Table<UserView>
            columns={columns}
            dataSource={data?.items ?? []}
            rowKey={(row) => String(row.id)}
            size="middle"
            scroll={{ x: 1000 }}
            pagination={{
              current: page,
              pageSize,
              total: data?.total ?? 0,
              showSizeChanger: true,
              showTotal: (total) => `共 ${total} 条`,
              onChange: (nextPage, nextSize) => {
                setPage(nextPage)
                setPageSize(nextSize)
              },
            }}
          />
        </AsyncBoundary>
      </Space>
    </Card>
  )
}
