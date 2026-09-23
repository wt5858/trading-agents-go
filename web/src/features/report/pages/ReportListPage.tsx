import { useState } from 'react'
import { Link } from 'react-router-dom'
import { Card, Space, Table, Tag, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'

import { listReports } from '../../../api/report'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import { TimeText } from '../../../components/TimeText'
import { formatConfidence, formatRiskScore } from '../../../utils/number'
import { useAsyncData } from '../../../hooks/useAsyncData'
import type { ReportSummaryView } from '../../../types/api'

/** 买卖动作到颜色。A 股习惯红买绿卖，与涨跌色保持一致。 */
const ACTION_COLOR: Record<string, string> = {
  buy: 'red',
  strong_buy: 'red',
  sell: 'green',
  strong_sell: 'green',
  hold: 'default',
}

const columns: ColumnsType<ReportSummaryView> = [
  {
    title: '标的',
    width: 160,
    render: (_, row) => <Link to={`/reports/${row.id}`}>{row.symbol}</Link>,
  },
  {
    title: '结论',
    width: 120,
    render: (_, row) => (
      <Tag color={ACTION_COLOR[row.action ?? ''] ?? 'default'}>{row.actionText || row.action}</Tag>
    ),
  },
  {
    title: '置信度',
    dataIndex: 'confidence',
    width: 100,
    render: (value?: string) => formatConfidence(value),
  },
  {
    title: '风险分',
    dataIndex: 'riskScore',
    width: 110,
    render: (value?: string) => formatRiskScore(value),
  },
  { title: '交易日', dataIndex: 'tradeDate', width: 120 },
  {
    title: '摘要',
    dataIndex: 'summary',
    ellipsis: true,
    render: (value?: string) => value || '—',
  },
  {
    title: '生成时间',
    dataIndex: 'createdAt',
    width: 140,
    render: (v?: string) => <TimeText value={v} />,
  },
]

export function ReportListPage() {
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)

  const { data, loading, error, reload } = useAsyncData(
    (signal) => listReports({ page, pageSize }, signal),
    [page, pageSize],
  )

  return (
    <Card>
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <AsyncBoundary
          loading={loading}
          error={error}
          onRetry={reload}
          isEmpty={!!data && data.items.length === 0}
          emptyText="还没有报告。分析任务跑完后会自动生成。"
        >
          <Table<ReportSummaryView>
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

        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          报告仅为模型输出，不构成投资建议。投资有风险，可能损失本金；过往表现不代表未来收益。
        </Typography.Text>
      </Space>
    </Card>
  )
}
