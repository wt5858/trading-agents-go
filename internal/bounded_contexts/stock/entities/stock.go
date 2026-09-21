// Package entities 承载股票上下文的全部业务不变式。
// 本包不感知 HTTP、数据库与事务——那些分别属于 application/ 与 repositories/。
package entities

import (
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_events"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Stock 是股票上下文唯一的聚合根，也是全系统所有行情/财务/资讯数据的身份锚点。
//
// 它之所以是实体而不是值对象：有稳定标识（Code = market + symbol）、有生命周期
// （上市 → 在市 → 退市 → 恢复上市），而且同一只票的名称、行业、市值会随时间变化，
// 但它始终是「同一只票」。Quote / Kline / Financial / News / SocialPost 没有这层语义，
// 它们是某个时刻的观测快照，因此归 value_objects/。
//
// 字段导出、约束靠意图明确的方法收敛：与 identity 的 User 保持同一范式。
// TotalMV / CircMV 的写入一律走 UpdateMarketValue，那里是市值不变式的唯一归属地。
type Stock struct {
	domain_event.EventRecorder

	ID       uint64
	Code     shared_vo.StockCode
	Name     string
	Industry string
	Area     string
	ListDate *time.Time
	Delisted bool
	// TotalMV / CircMV 的单位统一为**元**，换算由各数据源在进入本层之前完成。
	//
	// 曾经这里写的是「单位随数据源」，那在只有一个源时还能忍，多源之后不行：
	// 选股的 market_cap 过滤直接拿这一列跨市场排序比较，一个源给万元、
	// 另一个给元，差 1e4 的两行放在一起，任何阈值筛选都会静默失真——
	// 不报错，只是结果不对。领域层仍不做换算，只保证「非负」与
	// 「流通市值 <= 总市值」这两条与单位无关的不变式。
	//
	// 0 的含义是「本数据源不提供」，不是「市值为零」。仓储的 upsert 据此
	// 决定要不要覆盖已有值，见 stock_repository.go 的 stockOnConflict。
	//
	// 这两个值一律由数据源直接给出并落库，领域层绝不用「股价 × 股本」自行推导：
	// 推导出来的市值与数据源口径必然对不上，跨源对账时无从判断谁是对的。
	TotalMV decimal.Decimal
	CircMV  decimal.Decimal
	// Source 记录该行数据来自哪个数据源，跨源对账时必须知道。
	Source    string
	UpdatedAt time.Time
}

// ListParams 是新建股票主数据的入参。
// 用结构体而不是一串位置参数，是因为字段多且同类型相邻（Name/Industry/Area/Source 都是 string），
// 位置参数写错顺序编译器不会报错，线上才会发现行业列里存的是地区。
type ListParams struct {
	Code     shared_vo.StockCode
	Name     string
	Industry string
	Area     string
	ListDate *time.Time
	Delisted bool
	TotalMV  decimal.Decimal
	CircMV   decimal.Decimal
	Source   string
}

// List 登记一只股票。这是股票进入系统的唯一入口。
//
// 刻意不在这里登记事件：上市不是本系统做出的决策，只是从数据源观察到的既成事实，
// 全市场同步一次会产生几千条这样的「事件」，对消费方毫无价值。
// 真正需要通知下游的是退市/恢复上市，见 Delist / Relist。
func List(p ListParams) (*Stock, error) {
	if p.Code.IsZero() {
		return nil, custom_errors.Invalid("股票代码不能为空")
	}
	if !p.Code.Market.Valid() {
		return nil, custom_errors.Invalid("无法识别股票 %s 所属市场", p.Code.Symbol)
	}
	s := &Stock{
		Code:      p.Code,
		Name:      strings.TrimSpace(p.Name),
		Industry:  strings.TrimSpace(p.Industry),
		Area:      strings.TrimSpace(p.Area),
		ListDate:  p.ListDate,
		Delisted:  p.Delisted,
		Source:    p.Source,
		UpdatedAt: time.Now(),
	}
	if err := s.setMarketValue(p.TotalMV, p.CircMV); err != nil {
		return nil, err
	}
	return s, nil
}

// Symbol / FullSymbol / Market 是 Code 的转发访问器。
// 单独存一份 market 字段迟早会和 Code.Market 不一致，所以只保留派生读法。
func (s *Stock) Symbol() string           { return s.Code.Symbol }
func (s *Stock) FullSymbol() string       { return s.Code.FullSymbol() }
func (s *Stock) Market() shared_vo.Market { return s.Code.Market }

// IsAnalyzable 判定该标的当前是否可以作为分析任务的输入。
//
// 做成实体方法而不是让调用方各自写 `if !s.Delisted`：
// 「什么样的票不能分析」是业务规则，规则变化（比如将来要排除 ST 股）时只应该改这一处。
func (s *Stock) IsAnalyzable() bool { return !s.Delisted }

// UpdateProfile 更新资料。只覆盖非空字段——数据源经常在某次返回里漏掉行业或地区，
// 用空串覆盖等于把已有信息擦掉。
func (s *Stock) UpdateProfile(name, industry, area string) {
	if v := strings.TrimSpace(name); v != "" {
		s.Name = v
	}
	if v := strings.TrimSpace(industry); v != "" {
		s.Industry = v
	}
	if v := strings.TrimSpace(area); v != "" {
		s.Area = v
	}
	s.UpdatedAt = time.Now()
}

// UpdateMarketValue 刷新市值。数值一律来自数据源，本方法只负责守住不变式。
func (s *Stock) UpdateMarketValue(totalMV, circMV decimal.Decimal) error {
	if err := s.setMarketValue(totalMV, circMV); err != nil {
		return err
	}
	s.UpdatedAt = time.Now()
	return nil
}

// setMarketValue 是市值不变式的唯一归属地，构造与更新都走它。
func (s *Stock) setMarketValue(totalMV, circMV decimal.Decimal) error {
	if totalMV.IsNegative() || circMV.IsNegative() {
		return custom_errors.Invalid("市值不能为负数")
	}
	// 流通市值大于总市值在任何口径下都是数据错误，放过去会让后续的
	// 「流通占比」算出大于 1 的值，进而污染选股因子。
	if totalMV.IsPositive() && circMV.GreaterThan(totalMV) {
		return custom_errors.Invalid("流通市值不能大于总市值")
	}
	s.TotalMV = totalMV
	s.CircMV = circMV
	return nil
}

// MarkSyncedFrom 记录本次数据来自哪个源。同步链路降级到备用源时，
// 这一列是排查「为什么今天的数据口径变了」的唯一线索。
func (s *Stock) MarkSyncedFrom(source string) {
	if source == "" {
		return
	}
	s.Source = source
	s.UpdatedAt = time.Now()
}

// Delist 标记退市。这是真正的领域决策：退市会让该标的上的分析任务、
// 定时同步、自选股提醒全部失去意义，所以要登记事件通知下游。
//
// 这里的重复退市判断只是快速失败，真正的并发保证在仓储的 UPDATE 谓词里：
// 两个请求同时退市同一只票时，内存中的判断都会通过，只有带 delisted = false
// 条件的那条 UPDATE 才能分出胜负。
func (s *Stock) Delist() error {
	if s.Delisted {
		return custom_errors.Conflict("股票 %s 已处于退市状态", s.Code.FullSymbol())
	}
	s.Delisted = true
	s.UpdatedAt = time.Now()
	s.AddDomainEvent(domain_events.NewOnStockDelisted(
		s.ID, s.Code.Symbol, string(s.Code.Market), s.Name))
	return nil
}

// Relist 恢复上市。存在的理由：数据源误报退市是常态
// （停牌、重组期间的 list_status 会短暂变成 D），必须有一条撤销路径。
func (s *Stock) Relist() error {
	if !s.Delisted {
		return custom_errors.Conflict("股票 %s 未处于退市状态", s.Code.FullSymbol())
	}
	s.Delisted = false
	s.UpdatedAt = time.Now()
	s.AddDomainEvent(domain_events.NewOnStockRelisted(s.ID, s.Code.Symbol, string(s.Code.Market)))
	return nil
}
