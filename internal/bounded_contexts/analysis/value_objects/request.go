package value_objects

import (
	"strings"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Depth 是研究深度值对象。它只决定两件事：**启用哪些阶段**，以及**默认几位分析师**。
//
// 它不改变辩论轮数——多空辩论在任何深度下都是固定的一轮三人
// （多头、空头、研究经理），见 entities.NewPlan。此处曾注明「深度决定辩论轮次」、
// 且把 5 描述成「多轮辩论」，那是不存在的行为；照着它解释实验结果会得出错误结论。
//
// 各档的真实构成（阶段条件见下面两个谓词，分析师人数见 DefaultAnalystsFor）：
//
//	1     分析师(2) → 交易员
//	2     分析师(4) → 多空辩论 → 交易员
//	3     分析师(4) → 多空辩论 → 交易员 → 三视角风控 + 风控经理终裁
//	4, 5  阶段与 3 完全相同，只是默认分析师扩到 6 位
//
// 注意 2 与 3 的默认分析师阵容是同一批四位，因此这两档之间**唯一的差别就是风控阶段**。
type Depth int

const (
	DepthQuick      Depth = 1 // 分析师 + 交易决策
	DepthStandard   Depth = 3 // 再加多空辩论与风控阶段
	DepthExhaustive Depth = 5 // 阶段同 3，分析师阵容扩到 6 位
)

func (d Depth) Valid() bool { return d >= 1 && d <= 5 }

func (d Depth) Int() int { return int(d) }

// IncludesRiskPhase 深度 >= 3 才跑风控评估阶段。
func (d Depth) IncludesRiskPhase() bool { return d >= 3 }

// IncludesDebate 深度 >= 2 才跑多空辩论。
func (d Depth) IncludesDebate() bool { return d >= 2 }

// Request 是提交分析时的入参值对象。
//
// Code / TradeDate 用 shared_vo 的值对象而非裸 string：交易日在本系统里跨了四种外部格式
// （Tushare 用 YYYYMMDD、Mongo 用 YYYY-MM-DD、前端用 ISO、内部比较用字典序），
// 股票代码同样要按市场规范化。裸 string 传递时格式错配只会在运行期炸，且无处收敛校验。
type Request struct {
	Code      shared_vo.StockCode
	TradeDate shared_vo.TradeDate
	Depth     Depth
	Analysts  []string // 启用的分析师 ID，空表示按深度取默认集合
	LLMModel  string   // 留空走系统默认模型
}

// NewRequest 是 Request 的唯一构造入口：校验 + 补默认值 + 去重。
//
// 做成构造函数而不是可导出字段 + Validate()，是为了让「未校验的 Request」
// 无法在系统里流通——调用方拿到的每一个 Request 都已经是合法的。
func NewRequest(code shared_vo.StockCode, tradeDate shared_vo.TradeDate, depth Depth, analysts []string, llmModel string) (Request, error) {
	if code.IsZero() {
		return Request{}, custom_errors.Invalid("股票代码不能为空")
	}
	if !depth.Valid() {
		// 深度越界不报错而是回落到标准档：它是体验参数而非业务契约，
		// 为一个滑杆越界就拒绝整次提交，收益远小于代价。
		depth = DepthStandard
	}
	// 未指定交易日的语义是「最新交易日」，在这里就固化成具体日期，
	// 否则同一个任务在跨零点重试时会分析到不同的一天。
	tradeDate = tradeDate.OrToday()

	picked := normalizeAnalysts(analysts)
	if len(picked) == 0 {
		picked = DefaultAnalystsFor(depth)
	}

	return Request{
		Code:      code,
		TradeDate: tradeDate,
		Depth:     depth,
		Analysts:  picked,
		LLMModel:  strings.TrimSpace(llmModel),
	}, nil
}

// RehydrateRequest 从持久化数据重建，不做校验。
//
// 库里的数据是既成事实：用校验构造器去解析它，会让一条历史脏数据
// 把整个任务列表接口打挂。校验属于写入路径。
func RehydrateRequest(code shared_vo.StockCode, tradeDate shared_vo.TradeDate, depth Depth, analysts []string, llmModel string) Request {
	return Request{
		Code:      code,
		TradeDate: tradeDate,
		Depth:     depth,
		Analysts:  append([]string(nil), analysts...),
		LLMModel:  llmModel,
	}
}

func (r Request) IsZero() bool { return r.Code.IsZero() }

// AnalystIDs 返回分析师集合的副本。
// 返回副本而非内部切片，否则调用方一次 append 就能从外部改掉「不可变」的值对象。
func (r Request) AnalystIDs() []string { return append([]string(nil), r.Analysts...) }

// WithModel 返回替换了模型的新值对象。VO 不可变，因此是返回新值而不是原地修改。
func (r Request) WithModel(model string) Request {
	out := r
	out.Analysts = r.AnalystIDs()
	out.LLMModel = strings.TrimSpace(model)
	return out
}

// DefaultAnalystsFor 按深度返回默认分析师集合。
func DefaultAnalystsFor(d Depth) []string {
	switch {
	case d <= 1:
		return []string{"market", "fundamentals"}
	case d <= 3:
		return []string{"market", "fundamentals", "news", "sentiment"}
	default:
		return []string{"market", "fundamentals", "news", "sentiment", "sector", "index"}
	}
}

// normalizeAnalysts 去空白、去重并保序。
// 去重是必要的：重复的分析师 ID 会在 Progress 里生成两个同 key 的步骤，
// 而 Advance 只认第一个，进度会永远卡在 N-1 步。
func normalizeAnalysts(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
