import {Tooltip} from 'antd'

import {formatDateTime, formatRelative, formatShort, parseTime} from '../utils/datetime'

interface Props {
  value: string | null | undefined
  /** relative：3 分钟前｜short：09-24 00:51｜full：完整时间 */
  mode?: 'relative' | 'short' | 'full'
}

/** 统一的时间显示。相对/紧凑模式下悬停能看到完整时间——读着舒服和保留精度不必二选一。 */
export function TimeText({ value, mode = 'relative' }: Props) {
  const parsed = parseTime(value)
  if (!parsed) return <span>—</span>

  const text =
    mode === 'full'
      ? formatDateTime(value)
      : mode === 'short'
        ? formatShort(value)
        : formatRelative(value)

  if (mode === 'full') return <span>{text}</span>

  return (
    <Tooltip title={formatDateTime(value)}>
      <time dateTime={parsed.toISOString()}>{text}</time>
    </Tooltip>
  )
}
