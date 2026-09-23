// 后端的小数一律以字符串传（定点小数，见 internal/helpers/decimalx），
// 而且不做任何展示层换算——report_handler.go 里有一段注释专门解释为什么
// 接口层不写 Confidence*100：那组数字是生成当时固化的事实，读路径重算会让
// 同一份报告每刷新一次都可能给出不同的值。
//
// 那条约束管的是「不要重新推导业务事实」，不包括「怎么显示」。
// 把 0.42 显示成 42% 是纯粹的呈现，底层事实一个字节都没变——
// 所以换算放在这里，而不是去求后端改接口。

/** 解析后端的定点小数字符串。空值和非法值都返回 null。 */
function parse(raw: string | number | null | undefined): number | null {
  if (raw == null || raw === '') return null
  const value = typeof raw === 'number' ? raw : Number.parseFloat(raw)
  return Number.isNaN(value) ? null : value
}

/**
 * 置信度：后端是 0-1（见 report.Confidence 的字段注释），显示成百分比。
 *
 * 直接显示 0.4200 的问题不只是难读——同一个页面的报告正文里写的是「置信度：42%」，
 * 两个数字放在一起会让人以为是两回事。
 */
export function formatConfidence(raw: string | number | null | undefined): string {
  const value = parse(raw)
  if (value === null) return '—'
  return `${Math.round(value * 100)}%`
}

/** 风险分：后端是 0-10，越高越危险。带上分母才知道 7 是高还是低。 */
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

/**
 * 价格：截到两位小数。
 *
 * 后端存的是四位（4.6140），那个精度对计算有意义，对看盘的人没有——
 * A 股报价本身就是两位。多出来的两个零只是噪音。
 */
export function formatPrice(raw: string | number | null | undefined): string {
  const value = parse(raw)
  if (value === null) return '—'
  return value.toFixed(2)
}

/** 涨跌幅等百分比字段：后端给的已经是百分数（"-5.36"），补个 % 号。 */
export function formatPercent(raw: string | number | null | undefined): string {
  const value = parse(raw)
  if (value === null) return '—'
  return `${value.toFixed(2)}%`
}
