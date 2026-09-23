import { Link, useParams } from 'react-router-dom'
import { Alert, Anchor, Card, Col, Row, Space, Statistic, Tag, Typography } from 'antd'

import { getReport } from '../../../api/report'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import { Markdown } from '../../../components/Markdown'
import { TimeText } from '../../../components/TimeText'
import { useAsyncData } from '../../../hooks/useAsyncData'
import {
  formatConfidence,
  formatPosition,
  formatPrice,
  formatRiskScore,
} from '../../../utils/number'

const ACTION_COLOR: Record<string, string> = {
  buy: 'red',
  strong_buy: 'red',
  sell: 'green',
  strong_sell: 'green',
  hold: 'default',
}

export function ReportDetailPage() {
  const { id = '' } = useParams<{ id: string }>()
  const { data, loading, error, reload } = useAsyncData((signal) => getReport(id, signal), [id])

  const sections = [...(data?.sections ?? [])].sort((a, b) => (a.order ?? 0) - (b.order ?? 0))

  return (
    <AsyncBoundary loading={loading} error={error} onRetry={reload}>
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <Card>
          <Space direction="vertical" size={12} style={{ width: '100%' }}>
            <Space wrap align="center">
              <Typography.Title level={4} style={{ margin: 0 }}>
                <Link to={`/stocks/${data?.symbol}`}>{data?.symbol}</Link>
              </Typography.Title>
              <Tag color={ACTION_COLOR[data?.action ?? ''] ?? 'default'}>
                {data?.actionText || data?.action}
              </Tag>
              <Typography.Text type="secondary">{data?.tradeDate}</Typography.Text>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                生成于 <TimeText value={data?.createdAt} />
              </Typography.Text>
            </Space>

            {data?.title && <Typography.Text strong>{data.title}</Typography.Text>}

            <Row gutter={[32, 16]}>
              {/* 这组数字全部走展示层换算：后端给的是 0-1 的置信度、0-10 的风险分、
                  四位小数的价格。原样摆出来会和下面报告正文里的「置信度：42%」
                  对不上，读的人得停下来确认那是不是两个不同的东西。 */}
              <Col>
                <Statistic title="置信度" value={formatConfidence(data?.confidence)} />
              </Col>
              <Col>
                <Statistic title="风险分" value={formatRiskScore(data?.riskScore)} />
              </Col>
              <Col>
                <Statistic title="目标价" value={formatPrice(data?.targetPrice)} />
              </Col>
              <Col>
                <Statistic title="止损位" value={formatPrice(data?.stopLoss)} />
              </Col>
              <Col>
                <Statistic title="建议仓位" value={formatPosition(data?.position)} />
              </Col>
            </Row>

            {data?.summary && <Markdown>{data.summary}</Markdown>}
          </Space>
        </Card>

        <Alert
          type="warning"
          showIcon
          message="本报告由模型自动生成，不构成投资建议"
          description="投资有风险，可能损失本金；过往表现不代表未来收益。请自行判断并为决策负责。"
        />

        <Row gutter={16}>
          <Col xs={24} md={18}>
            <Space direction="vertical" size={16} style={{ width: '100%' }}>
              {sections.map((section) => (
                <Card key={section.key} id={`section-${section.key}`} title={section.title}>
                  {/* 正文是模型生成的 Markdown（**加粗**、- 列表、# 标题、表格都有），
                      当纯文本显示会把这些符号原样摆在页面上。 */}
                  <Markdown>{section.content ?? ''}</Markdown>
                </Card>
              ))}
              {sections.length === 0 && (
                <Card>
                  <Typography.Text type="secondary">这份报告没有正文小节。</Typography.Text>
                </Card>
              )}
            </Space>
          </Col>

          <Col xs={0} md={6}>
            {/* 报告小节有十来个，没有目录就只能一路滚。md 以下屏幕太窄，直接不显示。 */}
            <Anchor
              items={sections.map((s) => ({
                key: s.key!,
                href: `#section-${s.key}`,
                title: s.title,
              }))}
            />
          </Col>
        </Row>
      </Space>
    </AsyncBoundary>
  )
}
