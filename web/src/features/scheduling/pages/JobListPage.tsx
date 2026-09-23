import { useState } from 'react'
import { App, Button, Card, Popconfirm, Progress, Space, Table, Tag, Tooltip } from 'antd'
import type { ColumnsType } from 'antd/es/table'

import {
  deleteJob,
  listJobs,
  pauseJob,
  resumeJob,
  triggerJob,
  type JobView,
} from '../../../api/scheduling'
import { ApiError } from '../../../api/errors'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import { TimeText } from '../../../components/TimeText'
import { useAsyncData } from '../../../hooks/useAsyncData'
import { JobExecutions } from '../components/JobExecutions'

const STATUS_COLOR: Record<string, string> = {
  active: 'success',
  paused: 'warning',
  disabled: 'default',
}

const RUN_STATUS_COLOR: Record<string, string> = {
  success: 'success',
  failed: 'error',
  running: 'processing',
  timeout: 'error',
}

export function JobListPage() {
  const { message } = App.useApp()
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)

  const { data, loading, error, reload } = useAsyncData(
    (signal) => listJobs({ page, pageSize }, signal),
    [page, pageSize],
  )

  const act = async (fn: () => Promise<unknown>, okText: string) => {
    try {
      await fn()
      message.success(okText)
      reload()
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '操作失败')
    }
  }

  const columns: ColumnsType<JobView> = [
    { title: '名称', dataIndex: 'name', width: 200, ellipsis: true },
    {
      title: '类型',
      width: 140,
      render: (_, row) => row.kindText || row.kind,
    },
    {
      title: 'cron',
      dataIndex: 'cron',
      width: 140,
      render: (cron: string, row) =>
        row.cronValid === false ? (
          // cron 失效的任务永远不会再触发，而列表上它看起来和正常的一样。
          // 标红是唯一能让人注意到的地方。
          <Tooltip title="表达式非法，这个任务不会再被触发">
            <Tag color="error">{cron}</Tag>
          </Tooltip>
        ) : (
          <code>{cron}</code>
        ),
    },
    {
      title: '状态',
      width: 100,
      render: (_, row) => (
        <Tag color={STATUS_COLOR[row.status ?? ''] ?? 'default'}>
          {row.statusText || row.status}
        </Tag>
      ),
    },
    {
      title: '上次结果',
      width: 110,
      render: (_, row) =>
        row.lastStatus ? (
          <Tag color={RUN_STATUS_COLOR[row.lastStatus] ?? 'default'}>
            {row.lastStatusText || row.lastStatus}
          </Tag>
        ) : (
          '—'
        ),
    },
    {
      title: '成功率',
      width: 140,
      render: (_, row) => {
        const rate = Number.parseFloat(row.successRate ?? '')
        if (Number.isNaN(rate)) return '—'
        return (
          <Tooltip title={`${row.successRuns ?? 0} / ${row.totalRuns ?? 0} 次成功`}>
            <Progress percent={Math.round(rate)} size="small" />
          </Tooltip>
        )
      },
    },
    {
      title: '下次触发',
      dataIndex: 'nextRunAt',
      width: 140,
      render: (v?: string) => <TimeText value={v} />,
    },
    {
      title: '操作',
      width: 220,
      render: (_, row) => (
        <Space size={4}>
          <Button size="small" onClick={() => act(() => triggerJob(row.id!), '已触发')}>
            立即执行
          </Button>
          {row.status === 'paused' ? (
            <Button size="small" onClick={() => act(() => resumeJob(row.id!), '已恢复')}>
              恢复
            </Button>
          ) : (
            <Button size="small" onClick={() => act(() => pauseJob(row.id!), '已暂停')}>
              暂停
            </Button>
          )}
          <Popconfirm
            title="删除这个定时任务？"
            description="历史执行记录会保留。"
            onConfirm={() => act(() => deleteJob(row.id!), '已删除')}
          >
            <Button size="small" danger>
              删除
            </Button>
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <Card title="定时任务">
      <AsyncBoundary
        loading={loading}
        error={error}
        onRetry={reload}
        isEmpty={!!data && data.items.length === 0}
        emptyText="还没有定时任务"
      >
        <Table<JobView>
          columns={columns}
          dataSource={data?.items ?? []}
          rowKey={(row) => row.id!}
          size="middle"
          scroll={{ x: 1300 }}
          // 展开行拉执行历史，而不是单开一个详情页：执行记录是「看一眼就走」的东西，
          // 跳页再跳回来的代价比它本身的信息量还大。
          expandable={{
            expandedRowRender: (row) => <JobExecutions jobId={row.id!} />,
            rowExpandable: (row) => !!row.id,
          }}
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
    </Card>
  )
}
