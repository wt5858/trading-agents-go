import { Card, Collapse, Space, Statistic, Tag, Typography } from 'antd'

import { Markdown } from '../../../components/Markdown'
import { formatSeconds } from '../../../utils/datetime'
import type { DecisionChainView } from '../../../types/api'

/**
 * 立场到颜色。
 *
 * 七个取值来自实际响应，对应决策链的三层角色：
 *   analysis                    分析师陈述（不表态）
 *   bullish / bearish           多空辩论
 *   conservative / aggressive / neutral   风控三方
 *   arbiter                     研究经理与风控经理的裁决
 *
 * 多空沿用 A 股的红涨绿跌；风控的激进/保守用暖冷对比，一眼能看出哪一方在推仓位。
 * 匹配不上的取值退回默认灰，不抛错——后端加了新角色时页面不该白屏。
 */
const STANCE_COLOR: Record<string, string> = {
  bullish: 'red',
  bearish: 'green',
  aggressive: 'volcano',
  conservative: 'cyan',
  neutral: 'default',
  arbiter: 'purple',
  analysis: 'blue',
}

export function DecisionChain({ chain }: { chain: DecisionChainView }) {
  const links = [...(chain.links ?? [])].sort((a, b) => (a.seq ?? 0) - (b.seq ?? 0))

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Space size={48} wrap>
        <Statistic title="最终结论" value={chain.verdict?.actionText || '-'} />
        <Statistic title="耗时" value={formatSeconds(chain.durationSeconds)} />
        {/* usage 来自 value_objects.TokenUsage，它的 JSON 标签是 snake_case，
            而同一个响应里 ChainLinkView 的字段是 camelCase。两边不一致是后端的现状，
            照着生成的类型写即可——别顺手改成 totalTokens，那样编译不过。 */}
        <Statistic title="消耗 token" value={chain.usage?.total_tokens ?? '-'} />
      </Space>

      {chain.failed && (
        <Typography.Text type="danger">失败原因：{chain.failReason || '未知'}</Typography.Text>
      )}

      <Collapse
        // 默认全部收起。一条链有十几个智能体，每个的 content 都是整段分析文字，
        // 全展开的话页面会长到滚不到底。
        items={links.map((link) => ({
          key: String(link.seq),
          label: (
            <Space wrap>
              <Typography.Text strong>{link.agentName || link.agent}</Typography.Text>
              {link.phase && <Tag>{link.phase}</Tag>}
              {link.stance && (
                <Tag color={STANCE_COLOR[link.stance] ?? 'default'}>
                  {link.stanceName || link.stance}
                </Tag>
              )}
              {link.failed && <Tag color="error">失败</Tag>}
            </Space>
          ),
          children: (
            <Space direction="vertical" size={8} style={{ width: '100%' }}>
              {link.claim && (
                <Typography.Paragraph strong style={{ marginBottom: 4 }}>
                  {link.claim}
                </Typography.Paragraph>
              )}
              {/* 智能体的输出是 Markdown——标题、列表、加粗、表格都有。
                  当纯文本显示会把 ** 和 ### 原样摆在页面上。 */}
              {link.content ? (
                <Markdown>{link.content}</Markdown>
              ) : (
                <Typography.Text type="secondary">
                  {link.failReason || '（无内容）'}
                </Typography.Text>
              )}
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {link.durationSeconds ? `耗时 ${formatSeconds(link.durationSeconds)}` : ''}
                {link.totalTokens ? ` · ${link.totalTokens.toLocaleString()} tokens` : ''}
                {link.costUsd ? ` · $${link.costUsd}` : ''}
              </Typography.Text>
            </Space>
          ),
        }))}
      />

      {links.length === 0 && (
        <Card size="small">
          <Typography.Text type="secondary">这次分析没有产生决策链记录。</Typography.Text>
        </Card>
      )}
    </Space>
  )
}
