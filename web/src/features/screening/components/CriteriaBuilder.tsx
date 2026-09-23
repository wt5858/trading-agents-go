import { Button, Input, Select, Space } from 'antd'
import { DeleteOutlined, PlusOutlined } from '@ant-design/icons'

import type { FieldView } from '../../../api/screening'
import type { components } from '../../../types/api.generated'

/** 与 screening.CriterionRequest 完全一致，包括 operator 的枚举与 values 的字符串类型。 */
export type Criterion = components['schemas']['screening.CriterionRequest']
type Operator = Criterion['operator']

interface Props {
  fields: FieldView[]
  value: Criterion[]
  onChange: (next: Criterion[]) => void
}

/**
 * 条件行编辑器。
 *
 * # 两条来自后端契约的硬约束
 *
 * 1. values 一律是字符串数组，不是数字。后端用定点小数比价（internal/helpers/decimalx），
 *    走 JSON number 会让 12.35 这类阈值在往返后变成 12.349999999999999 —— 一个
 *    「筛不出本该筛出的标的」的沉默 bug。所以这里用 Input 直接存字符串，
 *    不经过 InputNumber 转一道。
 * 2. 取值个数由比较符的元数决定：between 恰好 2 个，in/not_in 至少 1 个，其余恰好 1 个。
 *    元数由后端字段字典里的 minValues/maxValues 给出，前端不硬编码。
 *
 * 字段与算子同样全部来自字典。硬编码的代价不是维护麻烦，而是「后端加了一个字段，
 * 前端筛不出来」这种沉默的功能缺失。
 */
export function CriteriaBuilder({ fields, value, onChange }: Props) {
  const fieldOf = (name: string) => fields.find((f) => f.name === name)
  const operatorOf = (fieldName: string, op: string) =>
    fieldOf(fieldName)?.operators?.find((o) => o.value === op)

  const patch = (index: number, next: Partial<Criterion>) => {
    onChange(value.map((c, i) => (i === index ? { ...c, ...next } : c)))
  }

  /** 按算子元数准备空的取值槽位。 */
  const slotsFor = (minValues: number | undefined) =>
    new Array(Math.max(minValues ?? 1, 1)).fill('')

  const addRow = () => {
    const first = fields[0]
    if (!first?.name) return
    const op = first.operators?.[0]
    onChange([
      ...value,
      {
        field: first.name,
        operator: (op?.value ?? 'eq') as Operator,
        values: slotsFor(op?.minValues),
      },
    ])
  }

  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      {value.map((c, index) => {
        const field = fieldOf(c.field)
        const op = operatorOf(c.field, c.operator)
        const slots = Math.max(op?.minValues ?? 1, 1)

        return (
          <Space key={index} wrap>
            <Select
              aria-label="筛选字段"
              style={{ width: 200 }}
              value={c.field}
              options={fields.map((f) => ({ value: f.name!, label: f.label || f.name! }))}
              onChange={(name) => {
                // 换字段就要换算子：算子是每个字段自己的，旧算子在新字段上多半非法。
                const nextOp = fieldOf(name)?.operators?.[0]
                patch(index, {
                  field: name,
                  operator: (nextOp?.value ?? 'eq') as Operator,
                  values: slotsFor(nextOp?.minValues),
                })
              }}
            />

            <Select
              aria-label="比较算子"
              style={{ width: 140 }}
              value={c.operator}
              options={(field?.operators ?? []).map((o) => ({
                value: o.value!,
                label: o.label || o.value!,
              }))}
              onChange={(next) => {
                const nextOp = operatorOf(c.field, next)
                patch(index, {
                  operator: next as Operator,
                  values: slotsFor(nextOp?.minValues),
                })
              }}
            />

            {Array.from({ length: slots }, (_, slot) => (
              <Input
                key={slot}
                aria-label={`${field?.label || c.field} 的第 ${slot + 1} 个取值`}
                style={{ width: 140 }}
                // inputMode 只影响移动端弹出的键盘，不做校验——
                // 合法性由后端判定，前端抢着校验只会和它的规则打架。
                inputMode="decimal"
                addonAfter={field?.unit || undefined}
                value={c.values[slot] ?? ''}
                onChange={(e) => {
                  const values = [...c.values]
                  values[slot] = e.target.value
                  patch(index, { values })
                }}
              />
            ))}

            <Button
              danger
              icon={<DeleteOutlined />}
              aria-label="删除这一条筛选条件"
              onClick={() => onChange(value.filter((_, i) => i !== index))}
            />
          </Space>
        )
      })}

      <Button icon={<PlusOutlined />} onClick={addRow} disabled={fields.length === 0}>
        添加条件
      </Button>
    </Space>
  )
}
