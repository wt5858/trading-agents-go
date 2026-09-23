import { useState } from 'react'
import { Link } from 'react-router-dom'
import { Alert, App, Button, Card, Select, Space, Table, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'

import {
  getFields,
  listTemplates,
  runScreening,
  runTemplate,
  type ResultSetView,
} from '../../../api/screening'
import { ApiError } from '../../../api/errors'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import { useAsyncData } from '../../../hooks/useAsyncData'
import { CriteriaBuilder, type Criterion } from '../components/CriteriaBuilder'

type Row = NonNullable<ResultSetView['items']>[number]

export function ScreeningPage() {
  const { message } = App.useApp()
  const [criteria, setCriteria] = useState<Criterion[]>([])
  const [result, setResult] = useState<ResultSetView | null>(null)
  const [running, setRunning] = useState(false)

  const fields = useAsyncData((signal) => getFields(signal), [])
  const templates = useAsyncData((signal) => listTemplates(signal), [])

  const run = async (fn: () => Promise<ResultSetView>) => {
    setRunning(true)
    try {
      setResult(await fn())
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '筛选失败')
    } finally {
      setRunning(false)
    }
  }

  // 列由后端给：可筛选字段是后端定义的，硬编码一套列必然和字段字典对不上。
  const columns: ColumnsType<Row> = [
    {
      title: '代码',
      width: 130,
      fixed: 'left',
      render: (_, row) => <Link to={`/stocks/${row.symbol}`}>{row.symbol}</Link>,
    },
    { title: '名称', dataIndex: 'name', width: 140, fixed: 'left' },
    ...(result?.columns ?? []).map((col) => ({
      title: col.unit ? `${col.label}（${col.unit}）` : col.label,
      key: col.field,
      width: 130,
      render: (_: unknown, row: Row) => row.values?.[col.field!] ?? '—',
    })),
  ]

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Card title="筛选条件">
        <AsyncBoundary loading={fields.loading} error={fields.error} onRetry={fields.reload}>
          <Space direction="vertical" size={12} style={{ width: '100%' }}>
            <CriteriaBuilder
              fields={fields.data?.fields ?? []}
              value={criteria}
              onChange={setCriteria}
            />

            <Space wrap>
              <Button
                type="primary"
                loading={running}
                disabled={criteria.length === 0}
                onClick={() => run(() => runScreening({ criteria, limit: 200 }))}
              >
                运行筛选
              </Button>

              <Select
                aria-label="套用已保存的模板"
                placeholder="套用模板"
                style={{ width: 220 }}
                loading={templates.loading}
                options={(templates.data ?? []).map((t) => ({
                  value: t.id!,
                  label: t.name,
                }))}
                onChange={(id) => run(() => runTemplate(id))}
              />
            </Space>
          </Space>
        </AsyncBoundary>
      </Card>

      <Card title="筛选结果">
        {result?.truncated && (
          // 截断必须说出来。结果少了却不说，用户会以为「就这么多符合条件的」，
          // 那是个会影响决策的误解。
          <Alert
            type="warning"
            showIcon
            style={{ marginBottom: 16 }}
            message="结果已被截断"
            description="符合条件的标的超过了返回上限，下面只是其中一部分。收紧条件可以看到完整结果。"
          />
        )}

        {result ? (
          <>
            <Table<Row>
              columns={columns}
              dataSource={result.items ?? []}
              rowKey={(row) => row.symbol!}
              size="middle"
              scroll={{ x: 'max-content' }}
              pagination={{ pageSize: 20, showTotal: (t) => `共 ${t} 条` }}
            />
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              数据截至 {result.asOf ?? '—'}。筛选结果仅为数据过滤，不构成投资建议；
              投资有风险，可能损失本金。
            </Typography.Text>
          </>
        ) : (
          <Typography.Text type="secondary">
            设好条件后点「运行筛选」，或直接套用一个已保存的模板。
          </Typography.Text>
        )}
      </Card>
    </Space>
  )
}
