import { useState } from 'react'
import { useLocation, useNavigate, Navigate } from 'react-router-dom'
import { Alert, App, Button, Card, Form, Input, Typography } from 'antd'

import { useAuth } from '../../../contexts/AuthContext'
import { ApiError } from '../../../api/errors'

interface LoginForm {
  username: string
  password: string
}

export function LoginPage() {
  const { login, user, bootstrapping } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const { message } = App.useApp()
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // 已登录的人不该再看到登录页（比如手输 /login，或者登录后点了后退）。
  if (!bootstrapping && user) return <Navigate to="/" replace />

  const from = (location.state as { from?: string } | null)?.from ?? '/'

  const onFinish = async (values: LoginForm) => {
    setSubmitting(true)
    setError(null)
    try {
      await login(values.username, values.password)
      message.success('登录成功')
      // replace：别在历史里留下登录页，否则登录后按后退会退回这里。
      navigate(from, { replace: true })
    } catch (err) {
      // 登录失败的原因要当场显示在表单上，而不是飘一个会自己消失的 toast——
      // 用户需要对着提示改输入。
      setError(err instanceof ApiError ? err.message : '登录失败，请稍后重试')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        padding: 16,
      }}
    >
      <Card style={{ width: '100%', maxWidth: 380 }}>
        <Typography.Title level={3} style={{ marginTop: 0, textAlign: 'center' }}>
          TradingAgents
        </Typography.Title>

        {error && (
          <Alert
            type="error"
            message={error}
            showIcon
            style={{ marginBottom: 16 }}
            closable
            onClose={() => setError(null)}
          />
        )}

        <Form<LoginForm> layout="vertical" onFinish={onFinish} disabled={submitting}>
          <Form.Item
            label="用户名"
            name="username"
            rules={[{ required: true, message: '请输入用户名' }]}
          >
            <Input autoComplete="username" autoFocus />
          </Form.Item>

          <Form.Item
            label="密码"
            name="password"
            rules={[{ required: true, message: '请输入密码' }]}
          >
            <Input.Password autoComplete="current-password" />
          </Form.Item>

          <Form.Item style={{ marginBottom: 0 }}>
            <Button type="primary" htmlType="submit" block loading={submitting}>
              登录
            </Button>
          </Form.Item>
        </Form>
      </Card>
    </div>
  )
}
