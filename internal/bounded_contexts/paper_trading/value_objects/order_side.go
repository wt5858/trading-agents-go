package value_objects

import (
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// OrderSide 是买卖方向。
//
// 用值对象而不是裸 string：方向决定了一次下单走 Buy 还是 Sell 这两条
// 完全不同的资金路径，把 "buy"/"BUY"/"b" 的兼容逻辑散落在接口层，
// 迟早会有一个分支把 "B" 当成未知方向、静默当作卖出处理。
// 规范化只发生在这一个构造点。
type OrderSide string

const (
	SideBuy  OrderSide = "buy"
	SideSell OrderSide = "sell"
)

// NewOrderSide 解析买卖方向。未知方向直接报错而不是退化成某个默认值：
// 方向猜错的代价是把一次买入执行成卖出，没有任何「合理默认」可言。
func NewOrderSide(raw string) (OrderSide, error) {
	switch OrderSide(strings.ToLower(strings.TrimSpace(raw))) {
	case SideBuy:
		return SideBuy, nil
	case SideSell:
		return SideSell, nil
	}
	return "", custom_errors.Invalid("未知的买卖方向: %s（只接受 buy / sell）", raw)
}

// RehydrateOrderSide 从库里读回，不做校验。
// 已经落库的行是既成事实，把它们再过一遍校验只会让一行脏数据
// 打挂整个成交历史列表。校验属于写路径。
func RehydrateOrderSide(raw string) OrderSide { return OrderSide(raw) }

func (s OrderSide) String() string { return string(s) }

func (s OrderSide) Valid() bool { return s == SideBuy || s == SideSell }

func (s OrderSide) IsBuy() bool { return s == SideBuy }

func (s OrderSide) IsSell() bool { return s == SideSell }

func (s OrderSide) DisplayName() string {
	switch s {
	case SideBuy:
		return "买入"
	case SideSell:
		return "卖出"
	}
	return string(s)
}
