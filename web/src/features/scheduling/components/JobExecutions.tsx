import { Table, Tag, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'

import { listExecutions, type ExecutionView } from '../../../api/scheduling'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import { TimeText } from '../../../components/TimeText'
import { useAsyncData } from '../../../hooks/useAsyncData'
import { formatDuration } from '../../../utils/datetime'

const STATUS_COLOR: Record<string, string> = {
  success: 'success',
  failed: 'error',
  running: 'processing',
  timeout: 'error',
}

const columns: ColumnsType<ExecutionView> = [
  {
    title: '计划时刻',
    dataIndex: 'scheduledAt',
    width: 140,
    render: (v?: string) => <TimeText value={v} />,
  },
  {
    title: '状态',
    width: 110,
    render: (_, row) => (
      <Tag color={STATUS_COLOR[row.status ?? ''] ?? 'default'}>
        {row.statusText || row.status}
      </Tag>
    ),
  },
  {
    title: '触发方式',
    width: 90,
    render: (_, row) => (row.manual ? <Tag>手动</Tag> : <Tag color="blue">自动</Tag>),
  },
  {
    title: '排队等待',
    dataIndex: 'queueWaitMs',
    width: 100,
    // 排队时间独立于执行时间：任务「慢」到底是自己慢还是在队列里堵着，
    // 这两个数分开看才判断得出来。
    render: (v?: number) => formatDuration(v),
  },
  { title: '耗时', dataIndex: 'durationMs', width: 100, render: (v?: number) => formatDuration(v) },
  { title: '处理条数', dataIndex: 'itemCount', width: 100, render: (v?: number) => v ?? '—' },
  {
    title: '结果',
    render: (_, row) =>
      row.error ? (
        <Typography.Text type="danger">{row.error}</Typography.Text>
      ) : (
        row.summary || '—'
      ),
  },
]

/** 某个定时任务的执行历史。只显示最近一页——排查问题看的就是最近几次。 */
export function JobExecutions({ jobId }: { jobId: string }) {
  const { data, loading, error, reload } = useAsyncData(
    (signal) => listExecutions(jobId, { page: 1, pageSize: 10 }, signal),
    [jobId],
  )

  return (
    <AsyncBoundary
      loading={loading}
      error={error}
      onRetry={reload}
      isEmpty={!!data && data.items.length === 0}
      emptyText="这个任务还没有执行记录"
    >
      <Table<ExecutionView>
        columns={columns}
        dataSource={data?.items ?? []}
        rowKey={(row) => String(row.id)}
        size="small"
        pagination={false}
      />
    </AsyncBoundary>
  )
}
