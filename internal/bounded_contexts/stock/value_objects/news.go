package value_objects

import (
	"time"

	"github.com/shopspring/decimal"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

// Sentiment 是情感分值对象，取值收敛在 [-1, 1]。
//
// 做成 VO 的理由：情感分来自三处（数据源自带、LLM 打分、规则模型），
// 各自的量纲不同，历史上出现过 0~100 的分数直接写进来，把加权情绪算成了 3000。
// 构造即钳位，越界数据在边界上就被压回合法区间。
//
// 从 `type Sentiment float64` 改成包装结构体，是因为 decimal.Decimal 不能做
// 具名类型的底层类型再挂方法。零值仍然可用：decimal 的零值就是 0，
// 所以 Sentiment{} 表示中性，不需要额外的构造调用。
type Sentiment struct{ v decimal.Decimal }

// 钳位边界。用 var 而不是 const：decimal 是结构体，不能做常量。
var (
	SentimentMin = Sentiment{v: decimal.NewFromInt(-1)}
	SentimentMax = Sentiment{v: decimal.NewFromInt(1)}

	// 中性带边界 ±0.2。
	sentimentPositiveFloor = decimalx.MustParse("0.2")
	sentimentNegativeCeil  = decimalx.MustParse("-0.2")
)

// NewSentiment 钳位而不是报错：情感分是辅助信号，
// 一个越界的分值不值得让整条资讯被丢弃。
func NewSentiment(v decimal.Decimal) Sentiment {
	switch {
	case v.LessThan(SentimentMin.v):
		return SentimentMin
	case v.GreaterThan(SentimentMax.v):
		return SentimentMax
	default:
		return Sentiment{v: v}
	}
}

// Value 返回原始数值。
func (s Sentiment) Value() decimal.Decimal { return s.v }

// String 渲染情感分，供提示词与接口层使用。
func (s Sentiment) String() string { return decimalx.FormatRatio(s.v) }

// IsPositive / IsNegative 用 ±0.2 作为中性带边界：
// 各家打分模型在 0 附近的抖动都在这个量级内，不设死区会把噪声当信号。
func (s Sentiment) IsPositive() bool { return s.v.GreaterThanOrEqual(sentimentPositiveFloor) }
func (s Sentiment) IsNegative() bool { return s.v.LessThanOrEqual(sentimentNegativeCeil) }

// News 是一条与标的相关的资讯。
type News struct {
	Code    shared_vo.StockCode
	Title   string
	Content string
	Source  string
	// URL 是资讯的去重依据：标题会被各家源改写、PublishedAt 有秒级漂移，只有 URL 稳定。
	URL         string
	PublishedAt time.Time
	Sentiment   Sentiment
}

// HasNaturalKey 判定自然键 (symbol, url) 是否完整。
// 没有 URL 的条目必须被丢弃，否则会 upsert 出一条 url="" 的黑洞文档，
// 把所有无 URL 资讯合并成一条。
func (n News) HasNaturalKey() bool { return !n.Code.IsZero() && n.URL != "" }

func (n News) Symbol() string { return n.Code.Symbol }

// SocialPost 是一条社交媒体舆情。
type SocialPost struct {
	Code        shared_vo.StockCode
	Platform    string
	Author      string
	Content     string
	Sentiment   Sentiment
	Engagement  int64
	PublishedAt time.Time
}

// HasNaturalKey 判定自然键 (symbol, platform, published_at) 是否完整。
// 社交平台不给稳定的贴文 ID，只能用这个三元组近似去重。
func (p SocialPost) HasNaturalKey() bool {
	return !p.Code.IsZero() && p.Platform != "" && !p.PublishedAt.IsZero()
}

func (p SocialPost) Symbol() string { return p.Code.Symbol }
