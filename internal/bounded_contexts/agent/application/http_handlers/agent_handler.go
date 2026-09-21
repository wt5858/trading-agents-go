// Package http_handlers 把 agent 上下文暴露成 HTTP 接口。
//
// 处理器只做三件事：绑定并做形状校验、调用 domain_service、渲染统一响应信封。
// 这里没有任何业务规则——分析的编排在 domain_services，不变式在 entities。
//
// 本上下文对外只开只读接口：发起分析的入口在 analysis 上下文
// （那里有任务、配额与队列），agent 只负责被它调用。
package http_handlers

import (
	"github.com/gin-gonic/gin"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

type AgentHandler struct {
	engine *domain_services.EngineService
}

func NewAgentHandler(engine *domain_services.EngineService) *AgentHandler {
	return &AgentHandler{engine: engine}
}

// Register 挂载路由。
//
// 两个入口都是读路径，不要求登录：智能体花名册是产品说明的一部分，
// 技术指标是公开行情的派生数据，加鉴权只会让前端为了画一张流程图先去换 token。
func (h *AgentHandler) Register(rg *gin.RouterGroup) {
	agents := rg.Group("/agents")
	agents.GET("/crew", h.Crew)
	agents.GET("/indicators/:code", h.Indicators)
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
