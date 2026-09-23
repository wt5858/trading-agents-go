import { useState } from 'react'
import { Link } from 'react-router-dom'
import { App, Button, Card, Col, Input, Popconfirm, Row, Space, Table } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { DeleteOutlined, PlusOutlined } from '@ant-design/icons'

import { addItem, listGroups, listItems, removeItem, type ItemView } from '../../../api/watchlist'
import { ApiError } from '../../../api/errors'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import { useAsyncData } from '../../../hooks/useAsyncData'
import { GroupSidebar } from '../components/GroupSidebar'

/** 涨跌色：A 股习惯红涨绿跌，与股票详情页保持一致。 */
function trendColor(raw: string | undefined): string | undefined {
  const value = Number.parseFloat(raw ?? '')
  if (Number.isNaN(value) || value === 0) return undefined
  return value > 0 ? '#cf1322' : '#3f8600'
}

export function WatchlistPage() {
  const { message } = App.useApp()
  const [activeId, setActiveId] = useState<number | null>(null)
  const [newCode, setNewCode] = useState('')

  const groups = useAsyncData((signal) => listGroups(signal), [])

  // 没选中分组时默认取第一个。放在渲染期算而不是用 effect 去 setState，
  // 免得多一次渲染，也免得「刚加载完还没选中」这个中间态闪一下空列表。
  const currentId = activeId ?? groups.data?.[0]?.id ?? null

  const items = useAsyncData(
    (signal) => (currentId ? listItems(currentId, signal) : Promise.resolve(null)),
    [currentId],
  )
  const itemRows = items.data?.items ?? []

  const onAddItem = async () => {
    const code = newCode.trim()
    if (!code || !currentId) return
    try {
      await addItem(currentId, code)
      setNewCode('')
      items.reload()
      groups.reload() // itemCount 要跟着变
      message.success('已加入自选')
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '添加失败')
    }
  }

  const onRemove = async (code: string) => {
    if (!currentId) return
    try {
      await removeItem(currentId, code)
      items.reload()
      groups.reload()
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '移除失败')
    }
  }

  const columns: ColumnsType<ItemView> = [
    {
      title: '代码',
      dataIndex: 'symbol',
      width: 130,
      render: (symbol: string) => <Link to={`/stocks/${symbol}`}>{symbol}</Link>,
    },
    { title: '现价', dataIndex: 'price', width: 100, render: (v?: string) => v ?? '-' },
    {
      title: '涨跌幅',
      dataIndex: 'changePct',
      width: 110,
      render: (v?: string) => <span style={{ color: trendColor(v) }}>{v ? `${v}%` : '-'}</span>,
    },
    {
      title: '加入后收益',
      dataIndex: 'gainPctSinceAdded',
      width: 130,
      render: (v?: string) => <span style={{ color: trendColor(v) }}>{v ? `${v}%` : '-'}</span>,
    },
    { title: '备注', dataIndex: 'note', ellipsis: true, render: (v?: string) => v || '—' },
    {
      title: '操作',
      width: 80,
      render: (_, row) => (
        <Popconfirm title="从自选中移除？" onConfirm={() => onRemove(row.symbol!)}>
          <Button size="small" danger icon={<DeleteOutlined />} aria-label={`移除 ${row.symbol}`} />
        </Popconfirm>
      ),
    },
  ]

  return (
    <Row gutter={16}>
      <Col xs={24} md={6}>
        <GroupSidebar
          groups={groups}
          currentId={currentId}
          onSelect={setActiveId}
          onDeleted={(id) => {
            if (currentId === id) setActiveId(null)
          }}
        />
      </Col>

      <Col xs={24} md={18}>
        <Card
          size="small"
          title={groups.data?.find((g) => g.id === currentId)?.name ?? '自选股'}
          extra={
            <Space.Compact>
              <Input
                aria-label="添加股票代码"
                placeholder="代码，如 600519.SH"
                value={newCode}
                onChange={(e) => setNewCode(e.target.value)}
                onPressEnter={onAddItem}
                style={{ width: 200 }}
                disabled={!currentId}
              />
              <Button
                icon={<PlusOutlined />}
                onClick={onAddItem}
                disabled={!currentId}
                aria-label="加入自选"
              />
            </Space.Compact>
          }
        >
          <AsyncBoundary
            loading={items.loading}
            error={items.error}
            onRetry={items.reload}
            isEmpty={!!items.data && itemRows.length === 0}
            emptyText={currentId ? '这个分组还没有股票' : '先创建一个分组'}
          >
            <Table<ItemView>
              columns={columns}
              dataSource={itemRows}
              rowKey={(row) => row.symbol!}
              size="middle"
              pagination={false}
            />
          </AsyncBoundary>
        </Card>
      </Col>
    </Row>
  )
}
