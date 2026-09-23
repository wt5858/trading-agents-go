package dtos

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// sampleColumns 是一条评分样本的落库形态，内嵌在评估文档的 samples 数组里。
// 内嵌的理由与 agent_runs 的 turns 一样：样本脱离它所属的那次评估没有意义，
// 而单文档写入让整个聚合的原子性由存储引擎直接保证。
type sampleColumns struct {
	RunID       string `bson:"run_id"`
	codeColumns `bson:",inline"`
	TradeDate   string `bson:"trade_date"`

	Action     string          `bson:"action"`
	Confidence decimal.Decimal `bson:"confidence"`
	Predicted  string          `bson:"predicted"`

	BasePrice    decimal.Decimal `bson:"base_price"`
	ForwardPrice decimal.Decimal `bson:"forward_price"`
	ForwardDate  string          `bson:"forward_date,omitempty"`
	// ReturnPct 是由两个价格相除推出的派生量，算一次存一份。
	// 所有读路径（统计、报表、命令行输出）读的都是这一列，永不重算。
	ReturnPct decimal.Decimal `bson:"return_pct"`
	Actual    string          `bson:"actual"`

	Hit        bool   `bson:"hit"`
	SkipReason string `bson:"skip_reason,omitempty"`
}

func sampleColumnsOf(s value_objects.EvalSample) sampleColumns {
	forwardDate := ""
	if !s.ForwardDate.IsZero() {
		forwardDate = s.ForwardDate.String()
	}
	return sampleColumns{
		RunID:       s.RunID,
		codeColumns: codeColumnsOf(s.Code),
		TradeDate:   s.TradeDate.String(),

		Action:     s.Action.String(),
		Confidence: s.Confidence,
		Predicted:  s.Predicted.String(),

		BasePrice:    s.BasePrice,
		ForwardPrice: s.ForwardPrice,
		ForwardDate:  forwardDate,
		ReturnPct:    s.ReturnPct,
		Actual:       s.Actual.String(),

		Hit:        s.Hit,
		SkipReason: string(s.SkipReason),
	}
}

func (c sampleColumns) toDomain() value_objects.EvalSample {
	var forwardDate shared_vo.TradeDate
	if c.ForwardDate != "" {
		forwardDate = shared_vo.MustTradeDate(c.ForwardDate)
	}
	return value_objects.EvalSample{
		RunID:     c.RunID,
		Code:      c.codeColumns.toDomain(),
		TradeDate: shared_vo.MustTradeDate(c.TradeDate),

		Action:     analysis_vo.Action(c.Action),
		Confidence: c.Confidence,
		Predicted:  value_objects.Direction(c.Predicted),

		BasePrice:    c.BasePrice,
		ForwardPrice: c.ForwardPrice,
		ForwardDate:  forwardDate,
		ReturnPct:    c.ReturnPct,
		Actual:       value_objects.Direction(c.Actual),

		Hit:        c.Hit,
		SkipReason: value_objects.SkipReason(c.SkipReason),
	}
}

type actionStatColumns struct {
	Action  string          `bson:"action"`
	Scored  int             `bson:"scored"`
	Hits    int             `bson:"hits"`
	HitRate decimal.Decimal `bson:"hit_rate"`
}

// statsColumns 是汇总统计的落库形态。
//
// 它整份都是派生量，本可以从 samples 现算。存下来是因为这份数字会被拿去
// 跨时间对比「这次改动之后系统变好了没有」，而重算依赖的是当时那版判定口径——
// 口径一改，历史报告的数字就跟着变了，对比也就无从谈起。
type statsColumns struct {
	Total      int                 `bson:"total"`
	Scored     int                 `bson:"scored"`
	Skipped    int                 `bson:"skipped"`
	Hits       int                 `bson:"hits"`
	HitRate    decimal.Decimal     `bson:"hit_rate"`
	SkipCounts map[string]int      `bson:"skip_counts"`
	ByAction   []actionStatColumns `bson:"by_action"`
	// Truncated 必须跟着一致率一起存：一份基于被砍过的样本算出来的一致率，
	// 事后从库里读出来时长得和全量的一模一样。
	Truncated bool `bson:"truncated"`
}

func statsColumnsOf(s value_objects.EvaluationStats) statsColumns {
	skips := make(map[string]int, len(s.SkipCounts))
	for k, v := range s.SkipCounts {
		skips[string(k)] = v
	}
	rows := make([]actionStatColumns, 0, len(s.ByAction))
	for _, a := range s.ByAction {
		rows = append(rows, actionStatColumns{
			Action: a.Action.String(), Scored: a.Scored, Hits: a.Hits, HitRate: a.HitRate,
		})
	}
	return statsColumns{
		Total: s.Total, Scored: s.Scored, Skipped: s.Skipped, Hits: s.Hits,
		HitRate: s.HitRate, SkipCounts: skips, ByAction: rows,
		Truncated: s.Truncated,
	}
}

func (c statsColumns) toDomain() value_objects.EvaluationStats {
	skips := make(map[value_objects.SkipReason]int, len(c.SkipCounts))
	for k, v := range c.SkipCounts {
		skips[value_objects.SkipReason(k)] = v
	}
	rows := make([]value_objects.ActionStat, 0, len(c.ByAction))
	for _, a := range c.ByAction {
		rows = append(rows, value_objects.ActionStat{
			Action: analysis_vo.Action(a.Action), Scored: a.Scored, Hits: a.Hits, HitRate: a.HitRate,
		})
	}
	return value_objects.EvaluationStats{
		Total: c.Total, Scored: c.Scored, Skipped: c.Skipped, Hits: c.Hits,
		HitRate: c.HitRate, SkipCounts: skips, ByAction: rows,
		Truncated: c.Truncated,
	}
}

// EvaluationDto 对应 agent_evaluations 集合：一次回测一份文档。
type EvaluationDto struct {
	ID        string    `bson:"_id"`
	CreatedAt time.Time `bson:"created_at"`

	WindowStart string `bson:"window_start"`
	WindowEnd   string `bson:"window_end"`
	HorizonDays int    `bson:"horizon_days"`
	// FlatBandPct 是判定口径的一部分，必须跟着结果一起存，
	// 否则两份一致率不同的报告无从分辨是系统变了还是口径变了。
	FlatBandPct decimal.Decimal `bson:"flat_band_pct"`

	Samples []sampleColumns `bson:"samples"`
	Stats   statsColumns    `bson:"stats"`
}

func FromDomainEvaluation(e *entities.Evaluation) *EvaluationDto {
	samples := e.Samples()
	rows := make([]sampleColumns, 0, len(samples))
	for _, s := range samples {
		rows = append(rows, sampleColumnsOf(s))
	}
	return &EvaluationDto{
		ID:          e.ID,
		CreatedAt:   e.CreatedAt.Truncate(time.Millisecond),
		WindowStart: e.Window.Start.String(),
		WindowEnd:   e.Window.End.String(),
		HorizonDays: e.HorizonDays,
		FlatBandPct: e.FlatBandPct,
		Samples:     rows,
		Stats:       statsColumnsOf(e.Stats()),
	}
}

func (dto EvaluationDto) ToDomain() *entities.Evaluation {
	samples := make([]value_objects.EvalSample, 0, len(dto.Samples))
	for _, s := range dto.Samples {
		samples = append(samples, s.toDomain())
	}
	return entities.RehydrateEvaluation(
		dto.ID,
		dto.CreatedAt,
		shared_vo.DateRange{
			Start: shared_vo.MustTradeDate(dto.WindowStart),
			End:   shared_vo.MustTradeDate(dto.WindowEnd),
		},
		dto.HorizonDays,
		dto.FlatBandPct,
		samples,
		dto.Stats.toDomain(),
	)
}
