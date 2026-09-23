import { Tooltip } from 'antd'

import { formatDateTime, formatRelative, formatShort, parseTime } from '../utils/datetime'

interface Props {
  value: string | null | undefined
  /**
   * relative：3 分钟前（列表里看新旧）
   * short：09-24 00:51（表格里省宽度）
   * full：2026-09-24 00:51:49（详情页要精确）
   */
  mode?: 'relative' | 'short' | 'full'
}

/**
 * 统一的时间显示。
 *
 * 无论哪种模式，鼠标悬停都能看到完整时间——相对时间读着舒服但丢精度，
 * 排查问题时需要的恰恰是那个精度。两者不必二选一。
 */
export function TimeText({ value, mode = 'relative' }: Props) {
  const parsed = parseTime(value)
  if (!parsed) return <span>—</span>

  const text =
    mode === 'full' ? formatDateTime(value) : mode === 'short' ? formatShort(value) : formatRelative(value)

  // full 模式下 tooltip 和正文一模一样，挂了只是徒增一次悬停抖动。
  if (mode === 'full') return <span>{text}</span>

  return (
    <Tooltip title={formatDateTime(value)}>
      {/* time 标签带上机读的 dateTime，对屏幕阅读器和复制粘贴都更友好。 */}
      <time dateTime={parsed.toISOString()}>{text}</time>
    </Tooltip>
  )
}
