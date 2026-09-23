import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { App, Button, Card, Descriptions, Popconfirm, Space, Tabs, Typography } from 'antd'

import {
  cancelTask,
  getDecisionChain,
  getTask,
  isTerminalStatus,
} from '../../../api/analysis'
import { ApiError } from '../../../api/errors'
import { AsyncBoundary } from '../../../components/AsyncBoundary'
import { TimeText } from '../../../components/TimeText'
import { useAsyncData } from '../../../hooks/useAsyncData'
import { DecisionChain } from '../components/DecisionChain'
import { ProgressPanel } from '../components/ProgressPanel'
import { TaskStatusTag } from '../components/TaskStatusTag'
import { useTaskProgress } from '../hooks/useTaskProgress'

export function TaskDetailPage() {
  const { id = '' } = useParams<{ id: string }>()
  const { message } = App.useApp()
  const [canceling, setCanceling] = useState(false)

  const task = useAsyncData((signal) => getTask(id, signal), [id])
  const status = task.data?.status
  const terminal = isTerminalStatus(status)

  const { progress, connected, streamError, finished } = useTaskProgress(id, status)

  // 流结束意味着任务到了终态，但流里只有进度、没有最终结论——重新拉一次详情。
  // 没有这一步的症状是进度条走到 100% 之后状态标签还停在「运行中」。
  useEffect(() => {
    if (finished && !terminal) task.reload()
    // task.reload 是 useCallback 出来的稳定引用，但把整个 task 放进依赖会每次渲染都变。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [finished, terminal])

  // 决策链只在任务终结后才有意义，跑到一半拉只会拿到半条链。
  const chain = useAsyncData(
    (signal) => (terminal ? getDecisionChain(id, signal) : Promise.resolve(null)),
    [id, terminal],
  )

  const onCancel = async () => {
    setCanceling(true)
    try {
      await cancelTask(id)
      message.success('已取消')
      task.reload()
    } catch (err) {
      message.error(err instanceof ApiError ? err.message : '取消失败')
    } finally {
      setCanceling(false)
    }
  }

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Card>
        <AsyncBoundary loading={task.loading} error={task.error} onRetry={task.reload}>
          <Space
            style={{ width: '100%', justifyContent: 'space-between', alignItems: 'flex-start' }}
            wrap
          >
            <Space direction="vertical" size={8}>
              <Typography.Title level={4} style={{ margin: 0 }}>
                <Link to={`/stocks/${task.data?.symbol}`}>{task.data?.symbol}</Link>{' '}
                <TaskStatusTag status={status} text={task.data?.statusText} />
              </Typography.Title>
              <Descriptions size="small" column={{ xs: 1, sm: 2, md: 3 }}>
                <Descriptions.Item label="任务号">{task.data?.id}</Descriptions.Item>
                <Descriptions.Item label="交易日">{task.data?.tradeDate ?? '-'}</Descriptions.Item>
                <Descriptions.Item label="深度">{task.data?.depth ?? '-'}</Descriptions.Item>
                <Descriptions.Item label="提交于">
                  <TimeText value={task.data?.createdAt} mode="full" />
                </Descriptions.Item>
                <Descriptions.Item label="结束于">
                  <TimeText value={task.data?.finishedAt} mode="full" />
                </Descriptions.Item>
                <Descriptions.Item label="分析师">
                  {task.data?.analysts?.join('、') || '默认阵容'}
                </Descriptions.Item>
              </Descriptions>
              {task.data?.error && (
                <Typography.Text type="danger">失败原因：{task.data.error}</Typography.Text>
              )}
            </Space>

            {!terminal && (
              <Popconfirm
                title="取消这个任务？"
                description="已经消耗的额度不会退回。"
                okText="取消任务"
                cancelText="再想想"
                onConfirm={onCancel}
              >
                <Button danger loading={canceling}>
                  取消任务
                </Button>
              </Popconfirm>
            )}
          </Space>
        </AsyncBoundary>
      </Card>

      <Card>
        <Tabs
          items={[
            {
              key: 'progress',
              label: '执行进度',
              children: (
                <ProgressPanel
                  // 任务已终结时用详情里的进度快照，流那边不会再推了。
                  progress={progress ?? task.data?.progress ?? null}
                  connected={connected}
                  streamError={streamError}
                  live={!terminal}
                />
              ),
            },
            {
              key: 'chain',
              label: '决策链',
              children: terminal ? (
                <AsyncBoundary
                  loading={chain.loading}
                  error={chain.error}
                  onRetry={chain.reload}
                  isEmpty={!chain.loading && !chain.error && !chain.data}
                  emptyText="没有决策链记录"
                >
                  {chain.data && <DecisionChain chain={chain.data} />}
                </AsyncBoundary>
              ) : (
                <Typography.Text type="secondary">
                  任务跑完后这里会显示每个智能体的观点与最终结论。
                </Typography.Text>
              ),
            },
          ]}
        />
      </Card>
    </Space>
  )
}
