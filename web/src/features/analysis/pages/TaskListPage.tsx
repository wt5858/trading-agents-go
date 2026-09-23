import { useState } from 'react'
import { Link } from 'react-router-dom'
import { App, Button, Card, Popconfirm, Progress, Select, Space, Table } from 'antd'
import type { ColumnsType } from 'antd/es/table'

import { cancelTask, isTerminalStatus, listTasks, TASK_STATUSES } from '../../../api/analysis'
import { ApiError } from '../../../api/errors'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import { TimeText } from '../../../components/TimeText'
import { useAsyncData } from '../../../hooks/useAsyncData'
import type { TaskView } from '../../../types/api'
import { TaskStatusTag } from '../components/TaskStatusTag'

const STATUS_OPTIONS = [
  { value: '', label: '全部状态' },
  ...TASK_STATUSES.map((s) => ({ value: s, label: s })),
]

export function TaskListPage() {
  const [status, setStatus] = useState('')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const { message } = App.useApp()

  const { data, loading, error, reload } = useAsyncData(
    (signal) => listTasks({ status, page, pageSize }, signal),
    [status, page, pageSize],
  )

  const onCancel = async (id: string) => {
    try {
      await cancelTask(id)
      message.success('已取消')
      reload()
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '取消失败')
    }
  }

  const columns: ColumnsType<TaskView> = [
    {
      title: '任务',
      dataIndex: 'id',
      width: 260,
      render: (id: string, row) => (
        <Link to={`/analysis/tasks/${id}`}>
          {row.symbol} · {id.slice(-12)}
        </Link>
      ),
    },
    {
      title: '状态',
      width: 120,
      render: (_, row) => <TaskStatusTag status={row.status} text={row.statusText} />,
    },
    {
      title: '进度',
      width: 180,
      render: (_, row) => {
        // percent 是后端给的字符串（"42.50"），Progress 要数字。
        // 解析失败时退回 0，而不是让 NaN 传进去——那会让进度条整个消失。
        const percent = Number.parseFloat(row.progress?.percent ?? '0')
        return (
          <Progress
            percent={Number.isNaN(percent) ? 0 : Math.round(percent)}
            size="small"
            status={
              row.status === 'failed'
                ? 'exception'
                : row.status === 'running'
                  ? 'active'
                  : undefined
            }
          />
        )
      },
    },
    { title: '交易日', dataIndex: 'tradeDate', width: 120 },
    {
      title: '提交时间',
      dataIndex: 'createdAt',
      width: 140,
      render: (v?: string) => <TimeText value={v} />,
    },
    {
      title: '操作',
      width: 100,
      render: (_, row) =>
        isTerminalStatus(row.status) ? null : (
          <Popconfirm
            title="取消这个任务？"
            description="已经消耗的额度不会退回。"
            okText="取消任务"
            cancelText="再想想"
            onConfirm={() => onCancel(row.id!)}
          >
            <Button size="small" danger>
              取消
            </Button>
          </Popconfirm>
        ),
    },
  ]

  return (
    <Card>
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <Space wrap>
          <Select
            aria-label="按状态筛选任务"
            value={status}
            options={STATUS_OPTIONS}
            style={{ width: 160 }}
            onChange={(value) => {
              setStatus(value)
              setPage(1)
            }}
          />
          <Button onClick={reload}>刷新</Button>
        </Space>

        <AsyncBoundary
          loading={loading}
          error={error}
          onRetry={reload}
          isEmpty={!!data && data.items.length === 0}
          emptyText={status ? `没有「${status}」状态的任务` : '还没有分析任务，去股票页提交一个'}
        >
          <Table<TaskView>
            columns={columns}
            dataSource={data?.items ?? []}
            rowKey={(row) => row.id!}
            size="middle"
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
