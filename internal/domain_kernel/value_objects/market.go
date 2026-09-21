package value_objects

import (
	"regexp"
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Market 是市场值对象，决定代码规范化规则、货币与交易时段。
type Market string

const (
	MarketCN      Market = "CN" // A 股
	MarketHK      Market = "HK" // 港股
	MarketUS      Market = "US" // 美股
	MarketUnknown Market = ""
)

func (m Market) Valid() bool {
	switch m {
	case MarketCN, MarketHK, MarketUS:
		return true
	}
	return false
}

func (m Market) Currency() string {
	switch m {
	case MarketCN:
		return "CNY"
	case MarketHK:
		return "HKD"
	case MarketUS:
		return "USD"
	}
	return ""
}

// String 返回市场代码本身。有了它，Market 才能像其它值对象一样
// 直接参与字符串拼接与日志输出，调用点不用到处写 string(market) 强转。
func (m Market) String() string { return string(m) }

func (m Market) DisplayName() string {
	switch m {
	case MarketCN:
		return "A股"
	case MarketHK:
		return "港股"
	case MarketUS:
		return "美股"
	}
	return "未知市场"
}

var (
	reCNCode = regexp.MustCompile(`^\d{6}$`)
	reHKCode = regexp.MustCompile(`^\d{4,5}$`)
	reUSCode = regexp.MustCompile(`^[A-Z][A-Z.\-]{0,9}$`)
)

// ParseMarket 从原始股票代码推断市场，支持 600519.SH / 00700.HK / AAPL 等写法。
func ParseMarket(raw string) Market {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if code == "" {
		return MarketUnknown
	}
	if idx := strings.LastIndex(code, "."); idx >= 0 {
		switch code[idx+1:] {
		case "SH", "SZ", "BJ":
			return MarketCN
		case "HK":
			return MarketHK
		case "US", "O", "N":
			return MarketUS
		}
		code = code[:idx]
	}
	switch {
	case reCNCode.MatchString(code):
		return MarketCN
	case reHKCode.MatchString(code):
		return MarketHK
	case reUSCode.MatchString(code):
		return MarketUS
	}
	return MarketUnknown
}

// StockCode 是规范化后的股票代码值对象。Raw 保留用户输入，Symbol 为市场内规范代码。
type StockCode struct {
	Symbol string // 规范化代码：A股 600519，港股 00700，美股 AAPL
	Market Market
	Raw    string
}

// NewStockCode 规范化股票代码；市场留空时自动推断。
func NewStockCode(raw string, market Market) (StockCode, error) {
	trimmed := strings.ToUpper(strings.TrimSpace(raw))
	if trimmed == "" {
		return StockCode{}, custom_errors.Invalid("股票代码不能为空")
	}
	if !market.Valid() {
		market = ParseMarket(trimmed)
	}
	if !market.Valid() {
		return StockCode{}, custom_errors.Invalid("无法识别股票代码所属市场: %s", raw)
	}

	symbol := trimmed
	if idx := strings.LastIndex(symbol, "."); idx >= 0 {
		symbol = symbol[:idx]
	}

	switch market {
	case MarketCN:
		if !reCNCode.MatchString(symbol) {
			return StockCode{}, custom_errors.Invalid("A股代码必须为 6 位数字: %s", raw)
		}
	case MarketHK:
		if !reHKCode.MatchString(symbol) {
			return StockCode{}, custom_errors.Invalid("港股代码必须为 4-5 位数字: %s", raw)
		}
		// 港股统一补齐到 5 位，00700 与 700 视为同一只股票。
		for len(symbol) < 5 {
			symbol = "0" + symbol
		}
	case MarketUS:
		if !reUSCode.MatchString(symbol) {
			return StockCode{}, custom_errors.Invalid("美股代码格式非法: %s", raw)
		}
	}
	return StockCode{Symbol: symbol, Market: market, Raw: trimmed}, nil
}

// FullSymbol 返回带交易所后缀的代码，用于对接外部数据源。
func (c StockCode) FullSymbol() string {
	switch c.Market {
	case MarketCN:
		switch {
		case strings.HasPrefix(c.Symbol, "6"):
			return c.Symbol + ".SH"
		case strings.HasPrefix(c.Symbol, "4"), strings.HasPrefix(c.Symbol, "8"):
			return c.Symbol + ".BJ"
		default:
			return c.Symbol + ".SZ"
		}
	case MarketHK:
		return c.Symbol + ".HK"
	default:
		return c.Symbol
	}
}

func (c StockCode) String() string { return c.FullSymbol() }

func (c StockCode) IsZero() bool { return c.Symbol == "" }
