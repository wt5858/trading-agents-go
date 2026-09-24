import {useState} from 'react'
import {Alert, App, Button, Card, Input, Modal, Popconfirm, Space, Table, Tag} from 'antd'
import type {ColumnsType} from 'antd/es/table'

import {deactivateUser, listUsers, resetUserPassword} from '../../../api/user'
import {ApiError} from '../../../api/errors'
import {AsyncBoundary} from '../../../components/AsyncBoundary'
import {useAsyncData} from '../../../hooks/useAsyncData'
import {useAuth} from '../../../contexts/AuthContext'
import type {UserView} from '../../../types/api'

export function UserListPage() {
  const { message } = App.useApp()
  const { user: me } = useAuth()
  const [keyword, setKeyword] = useState('')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)

  const { data, loading, error, reload } = useAsyncData(
    (signal) => listUsers({ keyword, page, pageSize }, signal),
    [keyword, page, pageSize],
  )

  // 后端不生成临时密码：接口要求调用方传 newPassword，响应体只有 {reset:true}。
  // 所以这里必须让管理员输入，而不是等着读一个不存在的字段。
  const [resetTarget, setResetTarget] = useState<UserView | null>(null)
  const [newPassword, setNewPassword] = useState('')
  const [resetting, setResetting] = useState(false)

  const onReset = async () => {
    if (!resetTarget?.id || !newPassword) return
    setResetting(true)
    try {
      await resetUserPassword(resetTarget.id, newPassword)
      message.success(`已重置 ${resetTarget.username} 的密码，请通过安全渠道转交`)
      setResetTarget(null)
      setNewPassword('')
      reload()
    } catch (err) {
      // 口令强度不足这类拒绝带着后端的具体说明，原样显示比「重置失败」有用。
      message.error(err instanceof ApiError ? err.message : '重置失败')
    } finally {
      setResetting(false)
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
            <Button size="small" onClick={() => setResetTarget(row)}>
              重置密码
            </Button>
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

      <Modal
        open={!!resetTarget}
        title={`重置 ${resetTarget?.username ?? ''} 的密码`}
        okText="重置"
        cancelText="取消"
        confirmLoading={resetting}
        okButtonProps={{ disabled: !newPassword }}
        onOk={onReset}
        onCancel={() => {
          setResetTarget(null)
          setNewPassword('')
        }}
      >
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message="旧密码立即失效，该用户的在途会话也会被吊销。"
        />
        <Input.Password
          aria-label="新密码"
          placeholder="新密码"
          autoComplete="new-password"
          value={newPassword}
          onChange={(e) => setNewPassword(e.target.value)}
          onPressEnter={onReset}
        />
      </Modal>
    </Card>
  )
}
