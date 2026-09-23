import { Link } from 'react-router-dom'
import { Tag } from 'antd'
import type { ColumnsType } from 'antd/es/table'

import { TimeText } from '../../../components/TimeText'

import type { PositionView, TradeView } from '../../../api/paper'

/** 盈亏色：正红负绿，与全站涨跌色一致。 */
export function pnlColor(raw: string | number | undefined): string | undefined {
  const value = typeof raw === 'number' ? raw : Number.parseFloat(raw ?? '')
  if (Number.isNaN(value) || value === 0) return undefined
  return value > 0 ? '#cf1322' : '#3f8600'
}

export const positionColumns: ColumnsType<PositionView> = [
  {
    title: '代码',
    dataIndex: 'symbol',
    width: 130,
    render: (symbol: string) => <Link to={`/stocks/${symbol}`}>{symbol}</Link>,
  },
  { title: '持仓', dataIndex: 'quantity', width: 100 },
  { title: '成本价', dataIndex: 'avgCost', width: 110 },
  {
    title: '现价',
    width: 110,
    // hasQuote 为假说明没取到行情，市值和浮盈是拿成本价推的，不能当成真实估值。
    render: (_, row) => (row.hasQuote ? row.marketPrice : <Tag color="warning">无行情</Tag>),
  },
  { title: '市值', dataIndex: 'marketValue', width: 130 },
  {
    title: '浮动盈亏',
    dataIndex: 'unrealizedPnl',
    width: 130,
    render: (v?: string) => <span style={{ color: pnlColor(v) }}>{v ?? '-'}</span>,
  },
]

export const tradeColumns: ColumnsType<TradeView> = [
  {
    title: '时间',
    dataIndex: 'tradedAt',
    width: 140,
    render: (v?: string) => <TimeText value={v} />,
  },
  { title: '代码', dataIndex: 'symbol', width: 130 },
  {
    title: '方向',
    width: 90,
    render: (_, row) => (
      <Tag color={row.side === 'buy' ? 'red' : 'green'}>{row.sideText || row.side}</Tag>
    ),
  },
  { title: '价格', dataIndex: 'price', width: 110 },
  { title: '数量', dataIndex: 'quantity', width: 100 },
  { title: '金额', dataIndex: 'amount', width: 130 },
  { title: '手续费', dataIndex: 'fee', width: 100 },
  {
    title: '已实现盈亏',
    dataIndex: 'realizedPnl',
    width: 130,
    render: (v?: string) => <span style={{ color: pnlColor(v) }}>{v ?? '-'}</span>,
  },
]
