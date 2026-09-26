import {Alert, Card, Space, Statistic, Table, Tag, Tooltip, Typography} from 'antd'
import type {ColumnsType} from 'antd/es/table'

import {listEvaluations} from '../../../api/evaluation'
import {AsyncBoundary} from '../../../components/AsyncBoundary'
import {TimeText} from '../../../components/TimeText'
import {useAsyncData} from '../../../hooks/useAsyncData'
import type {ConfidenceIntervalView, EvaluationView} from '../../../types/api'

/**
 * 样本量的解释力门槛。
 *
 * 低于这个数时，二项比例的 95% 区间宽到几乎覆盖半个值域——
 * 30 个样本上的 58%，区间大约是 [40%, 74%]，与抛硬币无法区分。
 * 这不是「差不多可以看」，是「看不出任何东西」，因此要显式警告而不是让数字自己说话。
 */
const MIN_INTERPRETABLE_SAMPLES = 100

function pct(v?: string): string {
  if (v === undefined || v === null || v === '') return '-'
  const n = Number(v)
  if (Number.isNaN(n)) return '-'
  return `${(n * 100).toFixed(1)}%`
}

function ciText(ci?: ConfidenceIntervalView): string {
  if (!ci || (ci.lower === undefined && ci.upper === undefined)) return ''
  if (Number(ci.lower ?? 0) === 0 && Number(ci.upper ?? 0) === 0) return '样本不足'
  return `[${pct(ci.lower)}, ${pct(ci.upper)}]`
}

/**
 * 判断两个区间是否重叠。
 *
 * 重叠意味着在当前样本量下分不出高低——这是「样本不足」的结论，
 * 不是「两者打平」的结论。把这个判断放在界面上，是为了不让读者
 * 把一个无法区分的差距读成系统的优势。
 */
function overlaps(a?: ConfidenceIntervalView, b?: ConfidenceIntervalView): boolean {
  if (!a || !b) return false
  const aLo = Number(a.lower ?? 0)
  const aHi = Number(a.upper ?? 0)
  const bLo = Number(b.lower ?? 0)
  const bHi = Number(b.upper ?? 0)
  if (aHi === 0 && bHi === 0) return false
  return aLo <= bHi && bLo <= aHi
}

const columns: ColumnsType<EvaluationView> = [
  {
    title: '评估时间',
    width: 170,
    render: (_, row) => <TimeText value={row.createdAt} />,
  },
  {
    title: '区间',
    width: 200,
    render: (_, row) => (
      <Typography.Text type="secondary">
        {row.windowStart} ~ {row.windowEnd}
        <br />
        前瞻 {row.horizonDays} 日
      </Typography.Text>
    ),
  },
  {
    title: '样本',
    width: 130,
    render: (_, row) => (
      <Space direction="vertical" size={0}>
        {/* 参与评分数与扫到的总数必须并列。只报前者会让「三分之二样本被跳过」
            这件事完全看不出来，而那正是结论最大的限制条件。 */}
        <span>
          {row.scored} / {row.total}
        </span>
        {(row.scored ?? 0) < MIN_INTERPRETABLE_SAMPLES && (
          <Tag color="warning">样本偏少</Tag>
        )}
        {row.truncated && <Tag color="orange">样本被截断</Tag>}
      </Space>
    ),
  },
  {
    title: '方向一致率',
    width: 200,
    render: (_, row) => (
      <Space direction="vertical" size={0}>
        <Typography.Text strong>{pct(row.hitRate)}</Typography.Text>
        {/* 区间永远跟着一致率一起显示，没有只给点估计的余地。 */}
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          95% CI {ciText(row.ci)}
        </Typography.Text>
      </Space>
    ),
  },
  {
    title: '对照基线',
    render: (_, row) => (
      <Space direction="vertical" size={2}>
        {(row.baselines ?? []).map((b) => (
          <Space key={b.name} size={6}>
            <Typography.Text type="secondary">{b.name}</Typography.Text>
            <Typography.Text>{pct(b.hitRate)}</Typography.Text>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {ciText(b.ci)}
            </Typography.Text>
            {overlaps(row.ci, b.ci) && (
              <Tooltip title="两条区间重叠，说明在当前样本量下分不出高低。这是样本不足的结论，不是打平的结论。">
                <Tag color="default">无法区分</Tag>
              </Tooltip>
            )}
          </Space>
        ))}
        {(row.baselines ?? []).length === 0 && (
          <Typography.Text type="secondary">—</Typography.Text>
        )}
      </Space>
    ),
  },
  {
    title: '跳过原因',
    width: 220,
    render: (_, row) => {
      const entries = Object.entries(row.skipCounts ?? {})
      if (entries.length === 0) return <Typography.Text type="secondary">—</Typography.Text>
      return (
        <Space direction="vertical" size={0}>
          {entries.map(([reason, n]) => (
            <Typography.Text key={reason} type="secondary" style={{ fontSize: 12 }}>
              {SKIP_REASON_TEXT[reason] ?? reason}: {n}
            </Typography.Text>
          ))}
        </Space>
      )
    },
  },
]

/** 跳过原因的中文说明，与后端 SkipReason.DisplayName 保持一致。 */
const SKIP_REASON_TEXT: Record<string, string> = {
  no_direction: '系统未给出方向',
  no_base_price: '缺分析当日收盘价',
  no_forward_price: '前瞻窗口内行情不足',
  fetch_failed: '取行情失败',
}

export function EvaluationListPage() {
  const { data, loading, error, reload } = useAsyncData(
    (signal) => listEvaluations(50, signal),
    [],
  )

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Alert
        type="info"
        showIcon
        message="怎么读这张表"
        description={
          <Space direction="vertical" size={4}>
            <span>
              一致率单独一个数字说明不了任何事，必须连着样本量与置信区间一起看。
              对照基线用的是同一批样本，因此可以直接比——如果系统的区间与「无脑全买入」
              的区间大面积重叠，说明在当前样本量下分不出高低。
            </span>
            <span>
              本页数据仅用于研究与学习，不构成投资建议。过往表现不代表未来收益，
              投资有风险，可能损失本金。
            </span>
          </Space>
        }
      />

      <AsyncBoundary
        loading={loading}
        error={error}
        onRetry={reload}
        isEmpty={!!data && (data.items?.length ?? 0) === 0}
        emptyText="还没有跑过回测。先用 backfill 积累样本，再用 backtest 评分。"
      >
        <Space direction="vertical" size={16} style={{ width: '100%' }}>
          <Card size="small">
            <Space size={48} wrap>
              <Statistic title="评估次数" value={data?.total ?? 0} />
              <Statistic title="最近一次样本量" value={data?.items?.[0]?.scored ?? 0} />
            </Space>
          </Card>
          <Table<EvaluationView>
            rowKey={(row) => row.id ?? ''}
            columns={columns}
            dataSource={data?.items ?? []}
            pagination={false}
            size="small"
          />
        </Space>
      </AsyncBoundary>
    </Space>
  )
}
