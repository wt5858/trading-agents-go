import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { Typography } from 'antd'

import './markdown.css'

/**
 * 渲染报告正文里的 Markdown。
 *
 * # 安全
 *
 * 正文是大模型生成的，属于不可信输入。react-markdown **默认不渲染原生 HTML**
 * （要显式挂 rehype-raw 才会），所以正文里就算混进 `<img onerror=...>` 也只会被当成
 * 普通文本显示，不会执行。这是选它而不是 marked 的主要理由——marked 直接吐 HTML，
 * 必须再接一层 DOMPurify，而「必须记得接」的安全措施迟早会漏掉一次。
 *
 * 不要为了渲染 HTML 而加 rehype-raw，那等于把这道保护拆了。
 *
 * # 为什么要逐个覆写组件
 *
 * 不覆写的话渲染出来的是浏览器默认样式的 h1/p/ul，字号和间距与 antd 的排版
 * 完全不在一个体系里，夹在 Card 中间很突兀。这里把块级元素映射到 antd 的
 * Typography，表格与列表交给 markdown.css，视觉上就和页面其余部分一致了。
 */
export function Markdown({ children }: { children: string }) {
  return (
    <div className="ta-markdown">
      <ReactMarkdown
        // GFM 提供表格、删除线、任务列表。报告正文里确实有表格，
        // 不开这个插件的话表格会渲染成一堆竖线。
        remarkPlugins={[remarkGfm]}
        components={{
          // 小节标题已经由外层 Card 的 title 承担了，正文里的 # 往下降两级，
          // 免得出现「卡片标题比卡片里的标题还小」。
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
            // 正文里的链接指向外部，新标签页打开；noreferrer 防止目标页通过
            // window.opener 反向操作本页。
            <Typography.Link href={href} target="_blank" rel="noopener noreferrer">
              {children}
            </Typography.Link>
          ),
          // 表格用原生 table 配 CSS，不转 antd Table：后者要的是
          // columns + dataSource 的结构化数据，而这里拿到的是已经解析成
          // React 节点的 thead/tbody，转换不回去。
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
