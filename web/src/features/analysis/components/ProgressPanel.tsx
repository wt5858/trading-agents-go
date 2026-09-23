import { Alert, Badge, Progress, Space, Steps, Typography } from 'antd'

import type { ProgressView } from '../../../types/api'

interface Props {
  progress: ProgressView | null
  connected: boolean
  streamError: string | null
  /** 任务处于终态时不显示「实时连接」徽标。 */
  live: boolean
}

/** 把后端的字符串百分比解析成数字。解析不出来当 0，不要让 NaN 流进 Progress。 */
function toPercent(raw: string | undefined): number {
  const value = Number.parseFloat(raw ?? '0')
  return Number.isNaN(value) ? 0 : Math.round(value)
}

export function ProgressPanel({ progress, connected, streamError, live }: Props) {
  const percent = toPercent(progress?.percent)
  const steps = progress?.steps ?? []

  // 当前步：优先用后端给的 currentIndex，它是权威。
  // 没有时退回「第一个没 done 的」；全部 done 时 findIndex 返回 -1，
  // 这种情况要指到末尾之后，否则步骤条会在任务跑完时回跳到第一步。
  const firstPending = steps.findIndex((s) => !s.done)
  const current = progress?.currentIndex ?? (firstPending === -1 ? steps.length : firstPending)

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      {streamError && (
        <Alert
          type="warning"
          showIcon
          message="进度更新中断"
          description={`${streamError}。任务仍在后端继续跑，刷新页面可重新连上。`}
        />
      )}

      <Space align="center" size={16} wrap>
        <Progress
          type="circle"
          percent={percent}
          size={80}
          status={steps.some((s) => s.failed) ? 'exception' : undefined}
        />
        <Space direction="vertical" size={2}>
          <Typography.Text strong>{progress?.message || '等待调度'}</Typography.Text>
          <Typography.Text type="secondary">
            {progress?.doneSteps ?? 0} / {progress?.totalSteps ?? 0} 步
            {progress?.etaSeconds ? ` · 预计还需 ${progress.etaSeconds} 秒` : ''}
          </Typography.Text>
          {live && (
            <Badge
              status={connected ? 'processing' : 'default'}
              text={connected ? '实时连接中' : '连接中…'}
            />
          )}
        </Space>
      </Space>

      {steps.length > 0 && (
        <Steps
          direction="vertical"
          size="small"
          current={current}
          items={steps.map((step) => ({
            title: step.name || step.key,
            description: step.detail,
            status: step.failed ? 'error' : step.done ? 'finish' : undefined,
          }))}
        />
      )}
    </Space>
  )
}
