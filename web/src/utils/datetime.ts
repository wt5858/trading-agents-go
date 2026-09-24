// 后端给的是带时区偏移的 ISO-8601（'2026-09-24T00:51:49.191+08:00'）。
// 全部用原生 Intl，不引 dayjs / date-fns——只需要格式化和相对时间两件事。

const DATE_TIME = new Intl.DateTimeFormat('zh-CN', {
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
})

const SHORT = new Intl.DateTimeFormat('zh-CN', {
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
})

const RELATIVE = new Intl.RelativeTimeFormat('zh-CN', { numeric: 'auto' })

export function parseTime(raw: string | null | undefined): Date | null {
  if (!raw) return null
  const d = new Date(raw)
  return Number.isNaN(d.getTime()) ? null : d
}

/** 完整时间：2026-09-24 00:51:49。换成短横线是为了和交易日字段的写法对齐。 */
export function formatDateTime(raw: string | null | undefined): string {
  const d = parseTime(raw)
  if (!d) return '—'
  return DATE_TIME.format(d).replace(/\//g, '-')
}

/** 紧凑时间，用于表格：09-24 00:51 */
export function formatShort(raw: string | null | undefined): string {
  const d = parseTime(raw)
  if (!d) return '—'
  return SHORT.format(d).replace(/\//g, '-')
}

const MINUTE = 60_000
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

/**
 * 相对时间：3 分钟前 / 2 小时前 / 昨天。
 *
 * 超过 7 天退回绝对时间——「37 天前」要求读的人自己做减法，
 * 而那正是相对时间想省掉的事。
 */
export function formatRelative(raw: string | null | undefined): string {
  const d = parseTime(raw)
  if (!d) return '—'

  const diff = d.getTime() - Date.now()
  const abs = Math.abs(diff)

  if (abs < MINUTE) return '刚刚'
  if (abs < HOUR) return RELATIVE.format(Math.round(diff / MINUTE), 'minute')
  if (abs < DAY) return RELATIVE.format(Math.round(diff / HOUR), 'hour')
  if (abs < 7 * DAY) return RELATIVE.format(Math.round(diff / DAY), 'day')
  return formatShort(raw)
}

/** 毫秒时长：1234 → 1.2s，90000 → 1m30s。 */
export function formatDuration(ms: number | null | undefined): string {
  if (ms == null) return '—'
  if (ms < 1000) return `${ms} ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`
  const minutes = Math.floor(ms / 60_000)
  const seconds = Math.round((ms % 60_000) / 1000)
  return seconds ? `${minutes}m${seconds}s` : `${minutes}m`
}

/** 秒时长。同时接受字符串——durationSeconds 是 "12.34" 这种形态，etaSeconds 是数字。 */
export function formatSeconds(seconds: number | string | null | undefined): string {
  if (seconds == null || seconds === '') return '—'
  const value = typeof seconds === 'number' ? seconds : Number.parseFloat(seconds)
  if (Number.isNaN(value)) return '—'
  return formatDuration(value * 1000)
}
