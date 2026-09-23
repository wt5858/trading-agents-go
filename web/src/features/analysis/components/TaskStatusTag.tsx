import { Tag } from 'antd'

// 状态到颜色的映射收在一处：列表页、详情页、报告页都要显示它，
// 各写各的必然会出现同一个状态在不同页面是不同颜色的情况。
const STATUS_COLOR: Record<string, string> = {
  queued: 'default',
  running: 'processing',
  completed: 'success',
  failed: 'error',
  canceled: 'warning',
}

export function TaskStatusTag({ status, text }: { status?: string; text?: string }) {
  if (!status) return <Tag>未知</Tag>
  return <Tag color={STATUS_COLOR[status] ?? 'default'}>{text || status}</Tag>
}
