// 后端的小数一律以字符串传（定点小数，见 internal/helpers/decimalx），且不做展示层换算。
// report_handler.go 那条「不写 Confidence*100」的约束管的是不要重新推导业务事实，
// 不包括怎么显示，所以换算放在这里。

function parse(raw: string | number | null | undefined): number | null {
  if (raw == null || raw === '') return null
  const value = typeof raw === 'number' ? raw : Number.parseFloat(raw)
  return Number.isNaN(value) ? null : value
}

/** 置信度：后端是 0-1，显示成百分比——报告正文里写的也是「42%」，两处要一致。 */
export function formatConfidence(raw: string | number | null | undefined): string {
  const value = parse(raw)
  if (value === null) return '—'
  return `${Math.round(value * 100)}%`
}

/** 风险分：后端是 0-10。带上分母才知道 7 是高还是低。 */
export function formatRiskScore(raw: string | number | null | undefined): string {
  const value = parse(raw)
  if (value === null) return '—'
  return `${value.toFixed(1)} / 10`
}

/** 建议仓位：后端已经是 0-100 的百分比数值，不要再乘 100。 */
export function formatPosition(raw: string | number | null | undefined): string {
  const value = parse(raw)
  if (value === null) return '—'
  return `${Number(value.toFixed(2))}%`
}

/** 价格：后端存四位小数，但 A 股报价本身就是两位，多出来的只是噪音。 */
export function formatPrice(raw: string | number | null | undefined): string {
  const value = parse(raw)
  if (value === null) return '—'
  return value.toFixed(2)
}

/** 涨跌幅等：后端给的已经是百分数（"-5.36"），补个 % 号。 */
export function formatPercent(raw: string | number | null | undefined): string {
  const value = parse(raw)
  if (value === null) return '—'
  return `${value.toFixed(2)}%`
}
