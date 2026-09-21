package llm

import (
	"strings"

	"github.com/shopspring/decimal"

	agentvo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

// Price 是每百万 token 的美元单价。
//
// 单价用字符串字面量而不是 float64 字面量：2.50 写成 float64 时，
// 内存里其实是 2.5000000000000004 这类值，单价一旦不精确，
// 乘上几百万 token 之后的成本估算就会带上一个说不清来源的尾差。
type Price struct {
	InputPerM  decimal.Decimal
	OutputPerM decimal.Decimal
}

// price 构造一条报价。
func price(in, out string) Price {
	return Price{InputPerM: decimalx.MustParse(in), OutputPerM: decimalx.MustParse(out)}
}

// prices 是常用模型的报价表。这是一份会过期的快照，只用于给用户一个成本量级，
// 不做对账；对不上的模型一律按 0 计，不猜。
var prices = map[string]Price{
	"gpt-4o":            price("2.50", "10.00"),
	"gpt-4o-mini":       price("0.15", "0.60"),
	"deepseek-chat":     price("0.27", "1.10"),
	"deepseek-reasoner": price("0.55", "2.19"),
	"qwen-plus":         price("0.40", "1.20"),
	"qwen-max":          price("1.60", "6.40"),
	"claude-sonnet-4-6": price("3.00", "15.00"),
	"claude-opus-4-8":   price("5.00", "25.00"),
	"gemini-2.0-flash":  price("0.10", "0.40"),
	"glm-4":             price("0.14", "0.14"),
}

// perMillion 是 token 计价的分母。
var perMillion = decimal.NewFromInt(1_000_000)

// CostOf 估算一次调用的美元成本。未知模型返回 0。
//
// 结果按金额精度取整：这个数会被逐次累加进 Usage.CostUSD，
// 不取整的话每次调用都会往累加值里带进十几位无意义的小数，
// 几百次调用后账单字符串会长得没法看，而多出来的位数一位都不可信。
func CostOf(model string, u agentvo.Usage) decimal.Decimal {
	p, ok := lookupPrice(model)
	if !ok {
		return decimal.Zero
	}
	in := decimal.NewFromInt(int64(u.PromptTokens)).Mul(p.InputPerM).DivRound(perMillion, 8)
	out := decimal.NewFromInt(int64(u.CompletionTokens)).Mul(p.OutputPerM).DivRound(perMillion, 8)
	return decimalx.RoundMoney(in.Add(out))
}

// lookupPrice 先精确匹配，再退化为最长前缀匹配，
// 这样 "gpt-4o-2024-08-06" 能命中 gpt-4o，而 "gpt-4o-mini" 仍优先命中自己。
func lookupPrice(model string) (Price, bool) {
	key := strings.ToLower(strings.TrimSpace(model))
	// 剥掉 "provider/model" 的路由前缀，报价只认模型本身。
	if i := strings.LastIndex(key, "/"); i >= 0 {
		key = key[i+1:]
	}
	if p, ok := prices[key]; ok {
		return p, true
	}

	best := ""
	for name := range prices {
		if strings.HasPrefix(key, name) && len(name) > len(best) {
			best = name
		}
	}
	if best == "" {
		return Price{}, false
	}
	return prices[best], true
}
