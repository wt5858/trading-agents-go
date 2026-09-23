import { useState } from 'react'
import { useParams } from 'react-router-dom'
import { Button, Card, Descriptions, Space, Statistic, Tag, Typography } from 'antd'

import { getQuote, getStock } from '../../../api/stock'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import { useAsyncData } from '../../../hooks/useAsyncData'
import { SubmitAnalysisModal } from '../../analysis/components/SubmitAnalysisModal'

/** 涨跌色：后端把 change/changePct 透传成字符串，这里只判正负不做运算。 */
function trendColor(changePct: string | undefined): string | undefined {
  if (!changePct) return undefined
  const value = Number.parseFloat(changePct)
  if (Number.isNaN(value) || value === 0) return undefined
  // A 股习惯：红涨绿跌，与欧美相反。
  return value > 0 ? '#cf1322' : '#3f8600'
}

export function StockDetailPage() {
  const { code = '' } = useParams<{ code: string }>()
  const [submitOpen, setSubmitOpen] = useState(false)

  const stock = useAsyncData((signal) => getStock(code, signal), [code])
  const quote = useAsyncData((signal) => getQuote(code, signal), [code])

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Card>
        <AsyncBoundary loading={stock.loading} error={stock.error} onRetry={stock.reload}>
          <Space
            style={{ width: '100%', justifyContent: 'space-between', alignItems: 'flex-start' }}
            wrap
          >
            <Space direction="vertical" size={4}>
              <Typography.Title level={4} style={{ margin: 0 }}>
                {stock.data?.name} <Typography.Text type="secondary">{stock.data?.symbol}</Typography.Text>
              </Typography.Title>
              <Space size={4} wrap>
                <Tag>{stock.data?.market}</Tag>
                {stock.data?.industry && <Tag color="blue">{stock.data.industry}</Tag>}
                {stock.data?.delisted && <Tag color="default">已退市</Tag>}
              </Space>
            </Space>

            <Button
              type="primary"
              onClick={() => setSubmitOpen(true)}
              // 退市或后端标记不可分析的标的，提交了也会被领域层拒绝，
              // 不如在这里就禁掉并说明原因。
              disabled={!stock.data?.analyzable || stock.data?.delisted}
            >
              提交分析
            </Button>
          </Space>
        </AsyncBoundary>
      </Card>

      <Card title="最新行情">
        <AsyncBoundary
          loading={quote.loading}
          error={quote.error}
          onRetry={quote.reload}
          isEmpty={!quote.loading && !quote.error && !quote.data}
          emptyText="暂无行情数据，可能还没同步过这只标的"
        >
          <Space size={48} wrap>
            <Statistic
              title={`收盘（${quote.data?.tradeDate ?? '-'}）`}
              value={quote.data?.close ?? '-'}
              valueStyle={{ color: trendColor(quote.data?.changePct) }}
            />
            <Statistic
              title="涨跌"
              value={quote.data?.change ?? '-'}
              valueStyle={{ color: trendColor(quote.data?.changePct) }}
            />
            <Statistic
              title="涨跌幅"
              value={quote.data?.changePct ? `${quote.data.changePct}%` : '-'}
              valueStyle={{ color: trendColor(quote.data?.changePct) }}
            />
          </Space>

          <Descriptions size="small" column={{ xs: 1, sm: 2, md: 4 }} style={{ marginTop: 24 }}>
            <Descriptions.Item label="今开">{quote.data?.open ?? '-'}</Descriptions.Item>
            <Descriptions.Item label="最高">{quote.data?.high ?? '-'}</Descriptions.Item>
            <Descriptions.Item label="最低">{quote.data?.low ?? '-'}</Descriptions.Item>
            <Descriptions.Item label="昨收">{quote.data?.preClose ?? '-'}</Descriptions.Item>
            <Descriptions.Item label="市盈率">{quote.data?.pe ?? '-'}</Descriptions.Item>
            <Descriptions.Item label="市净率">{quote.data?.pb ?? '-'}</Descriptions.Item>
            <Descriptions.Item label="成交额">{quote.data?.amount ?? '-'}</Descriptions.Item>
            <Descriptions.Item label="数据源">{quote.data?.source ?? '-'}</Descriptions.Item>
          </Descriptions>
        </AsyncBoundary>
      </Card>

      <SubmitAnalysisModal
        open={submitOpen}
        code={code}
        market={stock.data?.market}
        onClose={() => setSubmitOpen(false)}
      />
    </Space>
  )
}
