import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import {Typography} from 'antd'

import './markdown.css'

/**
 * 渲染报告正文与决策链里的 Markdown。
 *
 * 正文是大模型生成的不可信输入，而 react-markdown **默认不渲染原生 HTML**——
 * 这是选它而不是 marked 的主要理由（后者必须再接一层 DOMPurify）。
 * 不要为了渲染 HTML 去加 rehype-raw，那等于把这道保护拆了。
 *
 * 下面逐个覆写组件是为了对齐 antd 的排版；不覆写的话夹在 Card 中间会很突兀。
 */
export function Markdown({ children }: { children: string }) {
  return (
    <div className="ta-markdown">
      <ReactMarkdown
        // 报告正文里有表格，不开 GFM 会渲染成一堆竖线。
        remarkPlugins={[remarkGfm]}
        components={{
          // 小节标题已由外层 Card 承担，正文里的 # 往下降两级。
          h1: ({ children }) => (
            <Typography.Title level={4} style={{ marginTop: 16 }}>
              {children}
            </Typography.Title>
          ),
          h2: ({ children }) => (
            <Typography.Title level={5} style={{ marginTop: 16 }}>
              {children}
            </Typography.Title>
          ),
          h3: ({ children }) => (
            <Typography.Text strong style={{ display: 'block', marginTop: 12 }}>
              {children}
            </Typography.Text>
          ),
          p: ({ children }) => (
            <Typography.Paragraph style={{ marginBottom: 12 }}>{children}</Typography.Paragraph>
          ),
          code: ({ children }) => <Typography.Text code>{children}</Typography.Text>,
          a: ({ href, children }) => (
            <Typography.Link href={href} target="_blank" rel="noopener noreferrer">
              {children}
            </Typography.Link>
          ),
          // 用原生 table 配 CSS，不转 antd Table：后者要 columns + dataSource，
          // 而这里拿到的是已经解析成 React 节点的 thead/tbody，转换不回去。
          table: ({ children }) => (
            <div className="ta-markdown-table">
              <table>{children}</table>
            </div>
          ),
        }}
      >
        {children}
      </ReactMarkdown>
    </div>
  )
}
