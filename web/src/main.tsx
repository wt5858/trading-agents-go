import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { ConfigProvider, App as AntdApp } from 'antd'
import zhCN from 'antd/locale/zh_CN'

import { App } from './App'
import './index.css'

const container = document.getElementById('root')
if (!container) {
  throw new Error('#root 不存在——index.html 被改坏了')
}

// AntdApp 包一层不是可选的：antd v5 的 message/notification/Modal 静态方法
// 拿不到 ConfigProvider 的主题与 locale，只有通过 App.useApp() 取到的实例才行。
// 全局提示的样式与页面主题脱节，症状就出在少了这一层。
createRoot(container).render(
  <StrictMode>
    <ConfigProvider locale={zhCN}>
      <AntdApp>
        <App />
      </AntdApp>
    </ConfigProvider>
  </StrictMode>,
)
