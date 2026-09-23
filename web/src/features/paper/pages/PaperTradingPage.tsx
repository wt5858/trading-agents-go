import { useState } from 'react'
import { Alert, App, Button, Card, Popconfirm, Select, Space, Table } from 'antd'

import {
  getPortfolio,
  listAccounts,
  listTrades,
  resetAccount,
  type PositionView,
  type TradeView,
} from '../../../api/paper'
import { ApiError } from '../../../api/errors'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import { useAsyncData } from '../../../hooks/useAsyncData'
import { AccountSummary } from '../components/AccountSummary'
import { PlaceOrderModal } from '../components/PlaceOrderModal'
import { positionColumns, tradeColumns } from '../components/portfolioColumns'

export function PaperTradingPage() {
  const { message } = App.useApp()
  const [accountId, setAccountId] = useState<string | null>(null)
  const [orderOpen, setOrderOpen] = useState(false)

  const accounts = useAsyncData((signal) => listAccounts(signal), [])
  const currentId = accountId ?? accounts.data?.accounts?.[0]?.id ?? null

  const portfolio = useAsyncData(
    (signal) => (currentId ? getPortfolio(currentId, signal) : Promise.resolve(null)),
    [currentId],
  )
  const trades = useAsyncData(
    (signal) =>
      currentId ? listTrades(currentId, { page: 1, pageSize: 20 }, signal) : Promise.resolve(null),
    [currentId],
  )

  // 免责声明取响应里的那一份，不在前端抄常量。
  // 后端把它做成响应字段就是为了让任何消费者都拿不到「不带声明」的版本
  // （见 paper_trading_handler.go 顶部那段注释），前端抄一份会让后端改措辞时这里悄悄过期。
  const disclaimer =
    portfolio.data?.disclaimer ?? accounts.data?.disclaimer ?? trades.data?.disclaimer

  const refresh = () => {
    portfolio.reload()
    trades.reload()
    accounts.reload()
  }

  const onReset = async () => {
    if (!currentId) return
    try {
      await resetAccount(currentId)
      message.success('账户已重置')
      refresh()
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '重置失败')
    }
  }

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <AsyncBoundary
        loading={accounts.loading}
        error={accounts.error}
        onRetry={accounts.reload}
        isEmpty={!!accounts.data && (accounts.data.accounts ?? []).length === 0}
        emptyText="还没有模拟账户"
      >
        <Card
          size="small"
          title={
            <Select
              aria-label="选择模拟账户"
              style={{ width: 240 }}
              value={currentId ?? undefined}
              options={(accounts.data?.accounts ?? []).map((a) => ({
                value: a.id!,
                label: a.name || a.id!,
              }))}
              onChange={setAccountId}
            />
          }
          extra={
            <Space>
              <Button type="primary" onClick={() => setOrderOpen(true)} disabled={!currentId}>
                下单
              </Button>
              <Popconfirm
                title="重置这个账户？"
                description="所有持仓与成交记录会被清空，现金回到初始值。此操作不可撤销。"
                okText="重置"
                cancelText="取消"
                onConfirm={onReset}
              >
                <Button danger disabled={!currentId}>
                  重置账户
                </Button>
              </Popconfirm>
            </Space>
          }
        >
          <AsyncBoundary
            loading={portfolio.loading}
            error={portfolio.error}
            onRetry={portfolio.reload}
          >
            <AccountSummary portfolio={portfolio.data?.portfolio} />
          </AsyncBoundary>
        </Card>
      </AsyncBoundary>

      {disclaimer && <Alert type="warning" showIcon message="模拟交易" description={disclaimer} />}

      <Card title="持仓" size="small">
        <AsyncBoundary
          loading={portfolio.loading}
          error={portfolio.error}
          onRetry={portfolio.reload}
          isEmpty={!portfolio.loading && (portfolio.data?.portfolio?.positions ?? []).length === 0}
          emptyText="当前没有持仓"
        >
          <Table<PositionView>
            columns={positionColumns}
            dataSource={portfolio.data?.portfolio?.positions ?? []}
            rowKey={(row) => row.symbol!}
            size="middle"
            pagination={false}
          />
        </AsyncBoundary>
      </Card>

      <Card title="成交记录" size="small">
        <AsyncBoundary
          loading={trades.loading}
          error={trades.error}
          onRetry={trades.reload}
          isEmpty={!trades.loading && (trades.data?.items ?? []).length === 0}
          emptyText="还没有成交记录"
        >
          <Table<TradeView>
            columns={tradeColumns}
            dataSource={trades.data?.items ?? []}
            rowKey={(row) => String(row.id)}
            size="middle"
            scroll={{ x: 1000 }}
            pagination={false}
          />
        </AsyncBoundary>
      </Card>

      {currentId && (
        <PlaceOrderModal
          open={orderOpen}
          accountId={currentId}
          onClose={() => setOrderOpen(false)}
          onPlaced={refresh}
        />
      )}
    </Space>
  )
}
