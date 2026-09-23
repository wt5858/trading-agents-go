import { useState } from 'react'
import { Alert, App, Form, Input, InputNumber, Modal, Radio } from 'antd'

import { placeOrder, type OrderSide } from '../../../api/paper'
import { ApiError } from '../../../api/errors'

interface Props {
  open: boolean
  accountId: string
  onClose: () => void
  onPlaced: () => void
}

interface FormValues {
  code: string
  side: OrderSide
  /** 留空表示按最新行情成交，见 PlaceOrderRequest.price 的说明。 */
  price?: number
  quantity: number
  fee?: number
}

export function PlaceOrderModal({ open, accountId, onClose, onPlaced }: Props) {
  const [form] = Form.useForm<FormValues>()
  const { message } = App.useApp()
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const onOk = async () => {
    let values: FormValues
    try {
      values = await form.validateFields()
    } catch {
      return
    }

    setSubmitting(true)
    setError(null)
    try {
      // 价格、数量、手续费在请求体里都是字符串——后端用定点小数算钱，
      // 走 JSON number 会引入浮点误差。这里统一转字符串，留空的字段传 undefined
      // （JSON.stringify 会把它整个去掉），由后端落到默认行为。
      await placeOrder(accountId, {
        code: values.code,
        side: values.side,
        price: values.price == null ? undefined : String(values.price),
        quantity: String(values.quantity),
        fee: values.fee == null ? undefined : String(values.fee),
      })
      message.success('已成交')
      form.resetFields()
      onClose()
      onPlaced()
    } catch (err) {
      // 余额不足、持仓不够这类拒绝都带着后端的具体说明，原样显示比「下单失败」有用得多。
      setError(err instanceof ApiError ? err.message : '下单失败')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Modal
      open={open}
      title="模拟下单"
      onOk={onOk}
      onCancel={onClose}
      okText="提交"
      cancelText="取消"
      confirmLoading={submitting}
      destroyOnHidden
    >
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="这是模拟交易，不涉及真实资金与真实撮合。"
      />

      {error && <Alert type="error" message={error} showIcon style={{ marginBottom: 16 }} />}

      <Form<FormValues> form={form} layout="vertical" initialValues={{ side: 'buy' }}>
        <Form.Item label="标的代码" name="code" rules={[{ required: true, message: '请输入代码' }]}>
          <Input placeholder="600519.SH" />
        </Form.Item>

        <Form.Item label="方向" name="side" rules={[{ required: true }]}>
          <Radio.Group>
            <Radio.Button value="buy">买入</Radio.Button>
            <Radio.Button value="sell">卖出</Radio.Button>
          </Radio.Group>
        </Form.Item>

        <Form.Item label="价格" name="price" extra="留空表示按最新行情成交">
          <InputNumber min={0} step={0.01} style={{ width: '100%' }} />
        </Form.Item>

        <Form.Item
          label="数量"
          name="quantity"
          rules={[{ required: true, message: '请输入数量' }]}
          extra="A 股按手计价时请自行换算成股数"
        >
          <InputNumber min={1} step={100} style={{ width: '100%' }} />
        </Form.Item>

        <Form.Item label="手续费" name="fee" extra="留空则由后端按默认费率计算">
          <InputNumber min={0} step={0.01} style={{ width: '100%' }} />
        </Form.Item>
      </Form>
    </Modal>
  )
}
