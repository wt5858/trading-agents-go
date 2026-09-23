import { App, Button, Card, Space, Table, Tabs, Tag, Tooltip, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'

import {
  disableProvider,
  enableProvider,
  listProviders,
  listSettings,
  reloadConfig,
  testProvider,
  type SanitizedProvider,
  type SanitizedSetting,
} from '../../../api/config'
import { ApiError } from '../../../api/errors'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import { TimeText } from '../../../components/TimeText'
import { useAsyncData } from '../../../hooks/useAsyncData'

function ProvidersTab() {
  const { message } = App.useApp()
  const { data, loading, error, reload } = useAsyncData(
    (signal) => listProviders({ pageSize: 100 }, signal),
    [],
  )

  const act = async (fn: () => Promise<unknown>, okText: string) => {
    try {
      await fn()
      message.success(okText)
      reload()
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '操作失败')
    }
  }

  const onTest = async (id: number) => {
    try {
      const probe = await testProvider(id)
      // 连通性探测的结果要说清楚是哪一种失败（网络不通 / 密钥无效 / 模型不存在），
      // 所以直接把后端的说明显示出来，不要压成「测试失败」。
      if (probe.ok) message.success(`连通正常${probe.model ? `（${probe.model}）` : ''}`)
      else message.error(probe.message || '连通性测试未通过')
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '测试失败')
    }
  }

  const columns: ColumnsType<SanitizedProvider> = [
    { title: '名称', dataIndex: 'name', width: 160 },
    { title: '类型', dataIndex: 'kind', width: 120 },
    { title: 'Base URL', dataIndex: 'baseUrl', ellipsis: true },
    {
      title: '密钥',
      width: 160,
      // 响应里的 apiKey 永远是掩码（sk-…wxyz），后端不返回明文。
      // 这里原样显示掩码，不提供「查看」按钮——没有可查看的东西。
      render: (_, row) =>
        row.hasApiKey ? (
          <code>{row.apiKey}</code>
        ) : (
          <Tag color="warning">未配置</Tag>
        ),
    },
    {
      title: '状态',
      width: 140,
      render: (_, row) => (
        <Space size={4}>
          <Tag color={row.enabled ? 'success' : 'default'}>{row.enabled ? '已启用' : '已停用'}</Tag>
          {row.enabled && !row.usable && (
            // 启用了但不可用是最容易被忽略的状态：列表上看着是「已启用」，
            // 实际调用时才会失败。单独标出来。
            <Tooltip title="已启用但缺少密钥或配置不完整，实际调用会失败">
              <Tag color="error">不可用</Tag>
            </Tooltip>
          )}
        </Space>
      ),
    },
    { title: '优先级', dataIndex: 'priority', width: 90 },
    {
      title: '操作',
      width: 200,
      render: (_, row) => (
        <Space size={4}>
          <Button size="small" onClick={() => onTest(row.id!)}>
            测试
          </Button>
          {row.enabled ? (
            <Button size="small" onClick={() => act(() => disableProvider(row.id!), '已停用')}>
              停用
            </Button>
          ) : (
            <Button size="small" onClick={() => act(() => enableProvider(row.id!), '已启用')}>
              启用
            </Button>
          )}
        </Space>
      ),
    },
  ]

  return (
    <AsyncBoundary
      loading={loading}
      error={error}
      onRetry={reload}
      isEmpty={!!data && data.items.length === 0}
      emptyText="还没有配置模型供应商"
    >
      <Table<SanitizedProvider>
        columns={columns}
        dataSource={data?.items ?? []}
        rowKey={(row) => String(row.id)}
        size="middle"
        scroll={{ x: 1100 }}
        pagination={false}
      />
    </AsyncBoundary>
  )
}

function SettingsTab() {
  const { data, loading, error, reload } = useAsyncData((signal) => listSettings(signal), [])

  const columns: ColumnsType<SanitizedSetting> = [
    { title: '键', dataIndex: 'key', width: 260, render: (v: string) => <code>{v}</code> },
    {
      title: '值',
      dataIndex: 'value',
      ellipsis: true,
      // secret 为真的项后端已经掩码化了，这里再标一下，免得有人以为能看到明文。
      render: (value: string, row) =>
        row.secret ? <Tag color="default">{value}（已脱敏）</Tag> : <code>{value}</code>,
    },
    { title: '作用域', dataIndex: 'scope', width: 110 },
    { title: '说明', dataIndex: 'description', ellipsis: true },
    {
      title: '更新时间',
      dataIndex: 'updatedAt',
      width: 140,
      render: (v?: string) => <TimeText value={v} />,
    },
  ]

  return (
    <AsyncBoundary
      loading={loading}
      error={error}
      onRetry={reload}
      isEmpty={!!data && data.length === 0}
      emptyText="没有配置项"
    >
      <Table<SanitizedSetting>
        columns={columns}
        dataSource={data ?? []}
        rowKey={(row) => row.key!}
        size="middle"
        scroll={{ x: 1000 }}
        pagination={false}
      />
    </AsyncBoundary>
  )
}

export function ConfigPage() {
  const { message } = App.useApp()

  return (
    <Card
      title="系统配置"
      extra={
        <Button
          onClick={async () => {
            try {
              await reloadConfig()
              message.success('配置已重新加载')
            } catch (err) {
              message.error(err instanceof ApiError ? err.message : '重载失败')
            }
          }}
        >
          重新加载配置
        </Button>
      }
    >
      <Typography.Paragraph type="secondary" style={{ fontSize: 12 }}>
        接口从不返回明文密钥，只返回掩码。要更换密钥请通过供应商的密钥接口写入。
      </Typography.Paragraph>

      <Tabs
        items={[
          { key: 'providers', label: '模型供应商', children: <ProvidersTab /> },
          { key: 'settings', label: '配置项', children: <SettingsTab /> },
        ]}
      />
    </Card>
  )
}
