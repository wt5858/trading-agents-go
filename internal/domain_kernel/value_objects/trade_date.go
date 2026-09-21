package value_objects

import (
	"time"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

const dateLayout = "2006-01-02"

// TradeDate 是交易日值对象。
//
// 把它从裸 string 提升为 VO 的理由：交易日在本系统里跨了四种外部格式
// （Tushare 用 YYYYMMDD、Mongo 用 YYYY-MM-DD 字符串、前端用 ISO、内部比较用字典序），
// 用裸 string 传递时格式错配只会在运行期炸，且无处收敛校验。
type TradeDate struct {
	v string // 规范形式 YYYY-MM-DD；零值表示未指定
}

// NewTradeDate 解析并校验交易日。空串返回零值 TradeDate 而非错误，
// 因为「未指定交易日」是合法输入，语义为取最新交易日。
func NewTradeDate(s string) (TradeDate, error) {
	if s == "" {
		return TradeDate{}, nil
	}
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return TradeDate{}, custom_errors.Invalid("交易日格式须为 YYYY-MM-DD: %s", s)
	}
	return TradeDate{v: t.Format(dateLayout)}, nil
}

// MustTradeDate 用于已知合法的内部构造（如从数据库读回）。
func MustTradeDate(s string) TradeDate {
	d, err := NewTradeDate(s)
	if err != nil {
		return TradeDate{}
	}
	return d
}

func TradeDateOf(t time.Time) TradeDate { return TradeDate{v: t.Format(dateLayout)} }

func Today() TradeDate { return TradeDateOf(time.Now()) }

func (d TradeDate) IsZero() bool { return d.v == "" }

func (d TradeDate) String() string { return d.v }

// OrToday 在未指定时退化为今天，供需要具体日期的下游使用。
func (d TradeDate) OrToday() TradeDate {
	if d.IsZero() {
		return Today()
	}
	return d
}

// Compact 返回 YYYYMMDD 形式，Tushare 等国内数据源要这个格式。
func (d TradeDate) Compact() string {
	if d.IsZero() {
		return ""
	}
	return d.v[0:4] + d.v[5:7] + d.v[8:10]
}

func (d TradeDate) Time() (time.Time, bool) {
	if d.IsZero() {
		return time.Time{}, false
	}
	t, err := time.Parse(dateLayout, d.v)
	return t, err == nil
}

// Before 按字典序比较。规范化后的 YYYY-MM-DD 字典序即时间序，无需解析。
func (d TradeDate) Before(o TradeDate) bool { return d.v < o.v }

func (d TradeDate) AddDays(n int) TradeDate {
	t, ok := d.Time()
	if !ok {
		return d
	}
	return TradeDateOf(t.AddDate(0, 0, n))
}

func (d TradeDate) MarshalText() ([]byte, error) { return []byte(d.v), nil }

func (d *TradeDate) UnmarshalText(b []byte) error {
	parsed, err := NewTradeDate(string(b))
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// DateRange 是闭区间交易日范围值对象。
type DateRange struct {
	Start TradeDate
	End   TradeDate
}

// NewDateRange 构造日期范围。两端都为空时默认取近半年——
// 这是各分析师取数的通用窗口，避免调用方每次都重复这个决定。
func NewDateRange(start, end string) (DateRange, error) {
	s, err := NewTradeDate(start)
	if err != nil {
		return DateRange{}, err
	}
	e, err := NewTradeDate(end)
	if err != nil {
		return DateRange{}, err
	}
	if s.IsZero() && e.IsZero() {
		now := Today()
		return DateRange{Start: now.AddDays(-182), End: now}, nil
	}
	if e.IsZero() {
		e = Today()
	}
	if s.IsZero() {
		s = e.AddDays(-182)
	}
	if e.Before(s) {
		return DateRange{}, custom_errors.Invalid("起始日期不能晚于结束日期")
	}
	return DateRange{Start: s, End: e}, nil
}

// Days 把区间摊成逐天的自然日序列（闭区间，含首尾）。
//
// 它给的是**自然日**，不是交易日：本包不知道任何市场的休市安排，硬编一份节假日表
// 在这里只会过期。调用方若在乎休市，自己按市场跳过——按天拉数据的场景里，
// 多问一个休市日的代价只是一次返回空集的调用，而漏掉一个交易日是数据缺口。
//
// 区间非法（任一端为空、或起点晚于终点）时返回 nil 而不是报错：
// 调用方拿到空序列自然什么都不做，这比逼每个调用点写一次错误分支更合适。
func (r DateRange) Days() []TradeDate {
	start, ok := r.Start.Time()
	if !ok {
		return nil
	}
	end, ok := r.End.Time()
	if !ok {
		return nil
	}
	if end.Before(start) {
		return nil
	}
	days := make([]TradeDate, 0, int(end.Sub(start).Hours()/24)+1)
	for d := r.Start; !end.Before(mustTime(d)); d = d.AddDays(1) {
		days = append(days, d)
	}
	return days
}

// mustTime 只在 Days 内部使用：那里的每个值都由 AddDays 从一个已验证的日期推出来，
// 不可能解析失败。返回零值让循环条件立刻为假，而不是 panic——
// 这段代码跑在同步的热路径上，宁可提前结束也不要把整个进程带走。
func mustTime(d TradeDate) time.Time {
	t, _ := d.Time()
	return t
}

// LastNDays 构造截至今日的近 n 个自然日区间。
func LastNDays(n int) DateRange {
	if n <= 0 {
		n = 30
	}
	now := Today()
	return DateRange{Start: now.AddDays(-n), End: now}
}
