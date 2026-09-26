// Package http_handlers 把 agent 上下文暴露成 HTTP 接口。
//
// 处理器只做三件事：绑定并做形状校验、调用 domain_service、渲染统一响应信封。
// 这里没有任何业务规则——分析的编排在 domain_services，不变式在 entities。
//
// 本上下文对外只开只读接口：发起分析的入口在 analysis 上下文
// （那里有任务、配额与队列），agent 只负责被它调用。
package http_handlers

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

type AgentHandler struct {
	engine *domain_services.EngineService
	evals  *repositories.EvaluationRepository
}

func NewAgentHandler(
	engine *domain_services.EngineService,
	evals *repositories.EvaluationRepository,
) *AgentHandler {
	return &AgentHandler{engine: engine, evals: evals}
}

// Register 挂载路由。
//
// 全部是读路径，不要求登录：智能体花名册是产品说明的一部分，
// 技术指标是公开行情的派生数据，加鉴权只会让前端为了画一张流程图先去换 token。
// 回测评估同理——它是这个项目对外交代「这套东西准不准」的地方，
// 藏在登录后面等于没有。
func (h *AgentHandler) Register(rg *gin.RouterGroup) {
	agents := rg.Group("/agents")
	agents.GET("/crew", h.Crew)
	agents.GET("/indicators/:code", h.Indicators)
	agents.GET("/evaluations", h.Evaluations)
	agents.GET("/track-record/:code", h.TrackRecord)
}

// crewView 是智能体花名册的对外视图。
//
// 两个字段本可以用 gin.H 写完，但那样它就不在 OpenAPI 里——
// 响应体的形状只要没有类型，文档就只能靠人手写，而手写的那份迟早和代码分家。
//
// Members 直接复用 domain_services.CrewProfile：它是 Roster 的返回类型，
// 在这里再抄一份字段只会多出一处需要同步的地方。
type crewView struct {
	Members []domain_services.CrewProfile `json:"members"`
	Total   int                           `json:"total" example:"12"`
} // @name agent.CrewView

// Crew 返回全体成员的画像，供前端画分析流程图与工具授权表。
//
// @Summary  智能体阵容
// @Tags     智能体
// @Produce  json
// @Success  200 {object} response.Envelope{data=crewView}
// @Router   /agents/crew [get]
func (h *AgentHandler) Crew(c *gin.Context) {
	roster := h.engine.Roster()
	response.OK(c, crewView{Members: roster, Total: len(roster)})
}

// Indicators 读取已落库的技术指标快照。
//
// 这是一条纯读接口：查不到就返回 404，不会顺手算一份。
// 指标是乘除派生量，在读路径上现算会让这个接口的返回值和分析报告里的数字对不上，
// 而两个数字都以「MA20」的名义出现在用户面前。
//
// @Summary  技术指标快照
// @Tags     智能体
// @Produce  json
// @Param    code   path  string true  "标的代码" example(600519)
// @Param    market query string false "市场：CN/HK/US，留空按代码推断"
// @Param    period query string false "K 线周期：daily/weekly/monthly，默认 daily"
// @Param    date   query string false "交易日 YYYY-MM-DD，默认取当天；当天无行情则退到最近一个已算过的交易日"
// @Success  200 {object} response.Envelope{data=indicatorView}
// @Failure  400 {object} response.Envelope "代码、周期或交易日不合法"
// @Failure  404 {object} response.Envelope "该标的尚无已落库的指标快照"
// @Failure  503 {object} response.Envelope "技术指标仓储未就绪"
// @Router   /agents/indicators/{code} [get]
func (h *AgentHandler) Indicators(c *gin.Context) {
	snapshot, err := h.engine.Indicators(
		c.Request.Context(),
		c.Param("code"),
		c.Query("market"),
		c.Query("period"),
		c.Query("date"),
	)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toIndicatorView(snapshot))
}

// confidenceIntervalView 是置信区间的对外形态。
type confidenceIntervalView struct {
	Lower decimal.Decimal `json:"lower" swaggertype:"string" example:"0.4038"`
	Upper decimal.Decimal `json:"upper" swaggertype:"string" example:"0.5962"`
	Level decimal.Decimal `json:"level" swaggertype:"string" example:"0.95"`
} // @name agent.ConfidenceIntervalView

// baselineView 是对照基线的对外形态。
type baselineView struct {
	Name    string                 `json:"name" example:"无脑全买入"`
	Hits    int                    `json:"hits" example:"31"`
	HitRate decimal.Decimal        `json:"hitRate" swaggertype:"string" example:"0.5167"`
	CI      confidenceIntervalView `json:"ci"`
} // @name agent.BaselineView

// evaluationView 是一次回测评估的对外形态。
//
// 一致率、样本量、置信区间三者在这个结构里是绑在一起的，没有只给其中一个的余地。
// 这是刻意的：单独一个「58%」无法解释——n=30 时它的区间大约是 [40%, 74%]，
// 与抛硬币无法区分；而基线告诉你同一批样本上不动脑子能拿到多少。
// 三个数字缺任何一个，读者都会高估这个系统。
type evaluationView struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	// Window 是被评估的分析所在的交易日区间。
	WindowStart string `json:"windowStart" example:"2026-03-02"`
	WindowEnd   string `json:"windowEnd" example:"2026-06-30"`
	HorizonDays int    `json:"horizonDays" example:"7"`
	// FlatBandPct 是横盘判定带宽，属于判定口径的一部分。
	// 露出来是因为两份一致率不同的报告，可能只是带宽调过。
	FlatBandPct decimal.Decimal `json:"flatBandPct" swaggertype:"string" example:"1"`

	Total   int `json:"total" example:"120"`
	Scored  int `json:"scored" example:"60"`
	Skipped int `json:"skipped" example:"60"`
	Hits    int `json:"hits" example:"35"`

	HitRate decimal.Decimal        `json:"hitRate" swaggertype:"string" example:"0.5833"`
	CI      confidenceIntervalView `json:"ci"`
	// Baselines 与 hitRate 用的是同一批样本，因此可以直接比。
	Baselines []baselineView `json:"baselines"`
	// SkipCounts 是各跳过原因的条数。
	//
	// 必须露出来：「一致率 70%」与「一致率 70%，但三分之二样本因缺行情被跳过」
	// 是两个完全不同的结论，而后者才是真相。
	SkipCounts map[string]int `json:"skipCounts"`
	// Truncated 表示区间里还有运行没被取进这次评估，带着时间上的选择偏差。
	Truncated bool `json:"truncated" example:"false"`
} // @name agent.EvaluationView

type evaluationListView struct {
	Items []evaluationView `json:"items"`
	Total int              `json:"total" example:"3"`
} // @name agent.EvaluationListView

// Evaluations 列出最近的回测评估。
//
// 这条接口的存在理由是可见性：在它之前，全部评测结论只能由作者在自己的终端里
// 敲一条 `backtest` 命令看到——一个别人无法验证、也无法引用的数字。
//
// @Summary  回测评估列表
// @Tags     智能体
// @Produce  json
// @Param    limit query int false "返回条数，默认 20，最大 100"
// @Success  200 {object} response.Envelope{data=evaluationListView}
// @Failure  503 {object} response.Envelope "回测评估仓储未就绪"
// @Router   /agents/evaluations [get]
func (h *AgentHandler) Evaluations(c *gin.Context) {
	if h.evals == nil {
		response.Fail(c, custom_errors.Unavailable("回测评估不可用"))
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	items, err := h.evals.ListRecent(c.Request.Context(), limit)
	if err != nil {
		response.Fail(c, err)
		return
	}
	views := make([]evaluationView, 0, len(items))
	for _, e := range items {
		views = append(views, toEvaluationView(e))
	}
	response.OK(c, evaluationListView{Items: views, Total: len(views)})
}

func toEvaluationView(e *entities.Evaluation) evaluationView {
	st := e.Stats()
	skips := make(map[string]int, len(st.SkipCounts))
	for k, v := range st.SkipCounts {
		if v > 0 {
			skips[string(k)] = v
		}
	}
	bases := make([]baselineView, 0, len(st.Baselines))
	for _, b := range st.Baselines {
		bases = append(bases, baselineView{
			Name: b.Name, Hits: b.Hits, HitRate: b.HitRate, CI: toCIView(b.CI),
		})
	}
	return evaluationView{
		ID:          e.ID,
		CreatedAt:   e.CreatedAt,
		WindowStart: e.Window.Start.String(),
		WindowEnd:   e.Window.End.String(),
		HorizonDays: e.HorizonDays,
		FlatBandPct: e.FlatBandPct,

		Total:   st.Total,
		Scored:  st.Scored,
		Skipped: st.Skipped,
		Hits:    st.Hits,

		HitRate:    st.HitRate,
		CI:         toCIView(st.HitRateCI),
		Baselines:  bases,
		SkipCounts: skips,
		Truncated:  st.Truncated,
	}
}

func toCIView(c value_objects.ConfidenceInterval) confidenceIntervalView {
	return confidenceIntervalView{Lower: c.Lower, Upper: c.Upper, Level: c.Level}
}

// trackRecordSampleView 是战绩里的一条。
type trackRecordSampleView struct {
	TradeDate string          `json:"tradeDate" example:"2026-03-02"`
	Action    string          `json:"action" example:"buy"`
	ReturnPct decimal.Decimal `json:"returnPct" swaggertype:"string" example:"-8.32"`
	Hit       bool            `json:"hit" example:"false"`
} // @name agent.TrackRecordSampleView

// trackRecordView 是系统对某只标的的历史战绩。
//
// 它回答的是一个别处答不上的问题：「这套系统以前对这只票说过什么，后来对了吗」。
// 只有同时握着历史决策轨迹与真实前瞻行情的系统才给得出这个答案。
type trackRecordView struct {
	Symbol string `json:"symbol" example:"600519"`
	// Scored 是已评分的历史建议数，Hits 是其中方向判对的次数。
	// 两个数一起给，因为 3 战 2 胜与 300 战 200 胜是完全不同的事。
	Scored  int             `json:"scored" example:"12"`
	Hits    int             `json:"hits" example:"7"`
	HitRate decimal.Decimal `json:"hitRate" swaggertype:"string" example:"0.5833"`
	// CI 是命中率的 95% 置信区间。单只标的的样本量通常极小，
	// 不给区间的话「命中率 67%」会被当成一个可信的数字。
	CI      confidenceIntervalView  `json:"ci"`
	Samples []trackRecordSampleView `json:"samples"`
} // @name agent.TrackRecordView

// TrackRecord 返回系统对某只标的的历史战绩。
//
// @Summary  标的历史战绩
// @Tags     智能体
// @Produce  json
// @Param    code path string true "标的代码" example(600519)
// @Success  200 {object} response.Envelope{data=trackRecordView}
// @Failure  503 {object} response.Envelope "回测评估仓储未就绪"
// @Router   /agents/track-record/{code} [get]
func (h *AgentHandler) TrackRecord(c *gin.Context) {
	if h.evals == nil {
		response.Fail(c, custom_errors.Unavailable("回测评估不可用"))
		return
	}
	rec, err := h.evals.TrackRecordOf(c.Request.Context(), c.Param("code"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	samples := make([]trackRecordSampleView, 0, len(rec.Samples))
	for _, s := range rec.Samples {
		samples = append(samples, trackRecordSampleView{
			TradeDate: s.TradeDate, Action: s.Action, ReturnPct: s.ReturnPct, Hit: s.Hit,
		})
	}
	rate := decimal.Zero
	if rec.Scored > 0 {
		rate = decimal.NewFromInt(int64(rec.Hits)).
			DivRound(decimal.NewFromInt(int64(rec.Scored)), 4)
	}
	response.OK(c, trackRecordView{
		Symbol:  rec.Symbol,
		Scored:  rec.Scored,
		Hits:    rec.Hits,
		HitRate: rate,
		CI:      toCIView(value_objects.WilsonInterval(rec.Hits, rec.Scored)),
		Samples: samples,
	})
}

// indicatorView 是技术指标的对外视图。
//
// 单独定义而不是直接序列化实体：实体的字段是领域概念，
// 直接抛出去等于把内部结构变成公开 API，之后任何一次重命名都是破坏性变更。
type indicatorView struct {
	Symbol     string `json:"symbol" example:"600519.SH"`
	Market     string `json:"market" example:"CN"`
	Period     string `json:"period" example:"daily"`
	TradeDate  string `json:"tradeDate" example:"2026-09-17"`
	Source     string `json:"source"`
	ComputedAt string `json:"computedAt" example:"2026-09-17 15:30:00"`
	Indicators any    `json:"indicators"`
} // @name agent.IndicatorView

func toIndicatorView(s *entities.IndicatorSnapshot) *indicatorView {
	if s == nil {
		return nil
	}
	return &indicatorView{
		Symbol:     s.Code.FullSymbol(),
		Market:     string(s.Code.Market),
		Period:     s.Period.String(),
		TradeDate:  s.TradeDate.String(),
		Source:     s.Source,
		ComputedAt: s.ComputedAt.Format("2006-01-02 15:04:05"),
		Indicators: s.Indicators,
	}
}
