import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Alert, App, Form, InputNumber, Modal, Select } from 'antd'

import { getCrew, toAnalystOptions } from '../../../api/agent'
import { submitTask } from '../../../api/analysis'
import { ApiError } from '../../../api/errors'
import { useAsyncData } from '../../../hooks/useAsyncData'

interface Props {
  open: boolean
  code: string
  market?: string
  onClose: () => void
}

interface FormValues {
  depth: number
  analysts?: string[]
}

export function SubmitAnalysisModal({ open, code, market, onClose }: Props) {
  const [form] = Form.useForm<FormValues>()
  const navigate = useNavigate()
  const { message } = App.useApp()
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // 只在弹窗打开时才拉阵容——列表页上每一行都挂一个这个组件的话，
  // 不加这个判断就会在进页面时打出一堆重复请求。
  const { data: crew } = useAsyncData((signal) => (open ? getCrew(signal) : Promise.resolve(null)), [
    open,
  ])

  const onOk = async () => {
    let values: FormValues
    try {
      values = await form.validateFields()
    } catch {
      return // 校验没过，antd 自己会把错误标在字段上
    }

    setSubmitting(true)
    setError(null)
    try {
      const task = await submitTask({
        code,
        market,
        depth: values.depth,
        analysts: values.analysts,
      })
      message.success('已提交，正在排队')
      onClose()
      // 直接跳详情页：分析要跑几分钟，用户此刻最想看的是进度。
      navigate(`/analysis/tasks/${task.id}`)
    } catch (err) {
      // 配额超限要单独说清楚。统一提示「提交失败」的话，用户会反复重试，
      // 而正确的动作是去把在跑的任务取消掉。
      if (err instanceof ApiError && err.isQuotaExceeded) {
        setError(`${err.message}——请先到「分析任务」里取消一个在跑的任务`)
      } else {
        setError(err instanceof ApiError ? err.message : '提交失败，请稍后重试')
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Modal
      open={open}
      title={`提交分析：${code}`}
      onOk={onOk}
      onCancel={onClose}
      okText="提交"
      cancelText="取消"
      confirmLoading={submitting}
      destroyOnHidden
    >
      {error && (
        <Alert type="error" message={error} showIcon style={{ marginBottom: 16 }} />
      )}

      <Form<FormValues> form={form} layout="vertical" initialValues={{ depth: 3 }}>
        <Form.Item
          label="分析深度"
          name="depth"
          extra="层数越多辩论轮次越多，耗时和花费同步上升"
          rules={[{ required: true, message: '请填写分析深度' }]}
        >
          <InputNumber min={1} max={5} style={{ width: '100%' }} />
        </Form.Item>

        <Form.Item label="参与的分析师" name="analysts" extra="留空则由后端按默认阵容选择">
          <Select
            mode="multiple"
            allowClear
            placeholder="默认全部"
            options={toAnalystOptions(crew)}
          />
        </Form.Item>
      </Form>
    </Modal>
  )
}
