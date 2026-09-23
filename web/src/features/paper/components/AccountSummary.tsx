import { Col, Row, Statistic } from 'antd'

import type { PortfolioView } from '../../../api/paper'
import { pnlColor } from './portfolioColumns'

/** 账户资金概览。抽出来是为了让 PaperTradingPage 保持在可读的长度内。 */
export function AccountSummary({ portfolio }: { portfolio: PortfolioView | undefined }) {
  return (
    <Row gutter={[32, 16]}>
      <Col>
        <Statistic title="总资产" value={portfolio?.totalValue ?? '-'} />
      </Col>
      <Col>
        <Statistic title="可用现金" value={portfolio?.cash ?? '-'} />
      </Col>
      <Col>
        <Statistic title="持仓市值" value={portfolio?.marketValue ?? '-'} />
      </Col>
      <Col>
        <Statistic
          title="浮动盈亏"
          value={portfolio?.unrealizedPnl ?? '-'}
          valueStyle={{ color: pnlColor(portfolio?.unrealizedPnl) }}
        />
      </Col>
      <Col>
        <Statistic
          title="累计盈亏"
          value={portfolio?.totalPnl ?? '-'}
          valueStyle={{ color: pnlColor(portfolio?.totalPnl) }}
        />
      </Col>
    </Row>
  )
}
