// 后端给的时间一律是带时区偏移的 ISO-8601（'2026-09-24T00:51:49.191+08:00'），
// 直接显示对人不友好，而且带着毫秒和时区尾巴，在表格里还特别占宽度。
//
// 这里全部用原生 Intl，不引 dayjs / date-fns：
// 需要的只是「格式化 + 相对时间」两件事，Intl.DateTimeFormat 与
// Intl.RelativeTimeFormat 都是标准 API，为此多背一个库不划算。
// （antd 内部确实带了 dayjs，但那是它的实现细节，直接 import 一个传递依赖，
// 等于把自己绑在别人的升级节奏上。）

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

/** 解析后端时间。空值与非法值都返回 null，由调用方决定显示成什么。 */
export function parseTime(raw: string | null | undefined): Date | null {
  if (!raw) return null
  const d = new Date(raw)
  return Number.isNaN(d.getTime()) ? null : d
}

/** 完整时间，用于 tooltip 和详情页：2026-09-24 00:51:49 */
export function formatDateTime(raw: string | null | undefined): string {
  const d = parseTime(raw)
  if (!d) return '—'
  // zh-CN 的 formatToParts 默认会拼成 "2026/09/24 00:51:49"，
  // 这里统一成短横线，和后端日志、交易日字段（2026-09-24）的写法对齐。
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
 * 超过 7 天就退回绝对时间——「37 天前」这种说法要求读的人自己做减法，
 * 而那正是相对时间本来想省掉的事。近处用相对（一眼看出新旧），
 * 远处用绝对（一眼看出是哪天）。
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

/** 毫秒时长：1234 → 1.2s，90000 → 1m30s。表格里比裸毫秒数好读得多。 */
export function formatDuration(ms: number | null | undefined): string {
  if (ms == null) return '—'
  if (ms < 1000) return `${ms} ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`
  const minutes = Math.floor(ms / 60_000)
  const seconds = Math.round((ms % 60_000) / 1000)
  return seconds ? `${minutes}m${seconds}s` : `${minutes}m`
}

/**
 * 秒时长，后端有些字段给的是秒（durationSeconds、etaSeconds）。
 *
 * 同时接受字符串：这个后端的小数一律走字符串（见 decimalx 那套约定），
 * durationSeconds 就是 "12.34" 这种形态，而 etaSeconds 是数字。
 * 两种都收，省得每个调用点自己判一遍。
 */
export function formatSeconds(seconds: number | string | null | undefined): string {
  if (seconds == null || seconds === '') return '—'
  const value = typeof seconds === 'number' ? seconds : Number.parseFloat(seconds)
  if (Number.isNaN(value)) return '—'
  return formatDuration(value * 1000)
}
