import { useState } from 'react'
import { Link } from 'react-router-dom'
import { Card, Input, Select, Space, Table, Tag, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'

import { listStocks } from '../../../api/stock'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import { useAsyncData } from '../../../hooks/useAsyncData'
import type { StockView } from '../../../types/api'

const MARKETS = [
  { value: '', label: '全部市场' },
  { value: 'CN', label: 'A 股' },
  { value: 'HK', label: '港股' },
  { value: 'US', label: '美股' },
]

const columns: ColumnsType<StockView> = [
  {
    title: '代码',
    dataIndex: 'symbol',
    width: 140,
    render: (symbol: string, row) => <Link to={`/stocks/${row.fullCode ?? symbol}`}>{symbol}</Link>,
  },
  { title: '名称', dataIndex: 'name', width: 160 },
  { title: '市场', dataIndex: 'market', width: 90 },
  { title: '行业', dataIndex: 'industry' },
  {
    title: '状态',
    width: 120,
    render: (_, row) =>
      row.delisted ? (
        <Tag color="default">已退市</Tag>
      ) : row.analyzable ? (
        <Tag color="green">可分析</Tag>
      ) : (
        <Tag color="orange">不可分析</Tag>
      ),
  },
]

export function StockListPage() {
  const [keyword, setKeyword] = useState('')
  const [market, setMarket] = useState('')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)

  const { data, loading, error, reload } = useAsyncData(
    (signal) => listStocks({ keyword, market, page, pageSize }, signal),
    [keyword, market, page, pageSize],
  )

  return (
    <Card>
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <Space wrap>
          <Input.Search
            aria-label="按代码或名称搜索股票"
            placeholder="代码 / 名称 / 行业"
            allowClear
            style={{ width: 280 }}
            // 用 onSearch 而不是 onChange：后者每敲一个字母就发一次请求，
            // 而这个接口会走数据库模糊匹配。竞态本身 useAsyncData 兜住了，
            // 但没必要制造那么多必然被丢弃的请求。
            onSearch={(value) => {
              setKeyword(value)
              setPage(1)
            }}
          />
          <Select
            aria-label="按市场筛选"
            value={market}
            options={MARKETS}
            style={{ width: 140 }}
            onChange={(value) => {
              setMarket(value)
              setPage(1)
            }}
          />
        </Space>

        <AsyncBoundary
          loading={loading}
          error={error}
          onRetry={reload}
          isEmpty={!!data && data.items.length === 0}
          emptyText={keyword ? `没有匹配「${keyword}」的股票` : '股票库为空，先跑一次行情同步'}
        >
          <Table<StockView>
            columns={columns}
            dataSource={data?.items ?? []}
            // 用 fullCode 而不是数组下标做 key：翻页时下标会重复，
            // React 会把上一页的行状态错搭到新行上。
            rowKey={(row) => row.fullCode ?? String(row.id)}
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
          点代码进详情页可提交分析。
        </Typography.Text>
      </Space>
    </Card>
  )
}
