// Package domain_events 定义自选股上下文对外广播的领域事件。
//
// 三个事件全部由聚合根 WatchlistGroup 抛出，没有任何一个由子实体 WatchlistItem 抛出——
// 子实体不是根，它没有对外发声的资格，「某只票被加入了自选」在语义上永远是
// 「某个分组发生了变化」。这也是为什么 WatchlistItem 不嵌 EventRecorder。
package domain_events

import (
	"encoding/json"

	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
)

const (
	OnGroupCreatedEventName   = "watchlist.group_created"
	OnStockWatchedEventName   = "watchlist.stock_watched"
	OnStockUnwatchedEventName = "watchlist.stock_unwatched"
)

// OnGroupCreated 在新分组创建时抛出。
//
// GroupID 刻意不在这里：分组 ID 是自增主键，创建事件在聚合内部产生时
// 这个 ID 还不存在（要等 INSERT 回填）。硬塞一个 0 进去比不带更糟——
// 消费方会拿着 0 去查库。需要 ID 的消费方应当订阅落库之后的路径。
type OnGroupCreated struct {
	domain_event.BaseDomainEvent
	UserID    uint64 `json:"userId"`
	GroupName string `json:"groupName"`
}

func NewOnGroupCreated(userID uint64, groupName string) *OnGroupCreated {
	return &OnGroupCreated{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		UserID:          userID,
		GroupName:       groupName,
	}
}

func (e *OnGroupCreated) Name() string { return OnGroupCreatedEventName }

func (e *OnGroupCreated) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnStockWatched 在一只股票被加入某个分组时抛出。
//
// 载荷只有标识与代码，没有 WatchlistItem 实体本身：领域事件跨上下文传播，
// 携带实体等于把子实体的指针交到了根的控制范围之外。
// 下游（行情订阅、盘中提醒）需要的也正是「谁、在哪个分组、盯了哪只票」。
type OnStockWatched struct {
	domain_event.BaseDomainEvent
	UserID  uint64 `json:"userId"`
	GroupID uint64 `json:"groupId"`
	Symbol  string `json:"symbol"` // 带市场后缀的完整代码，如 600519.SH
	Market  string `json:"market"`
}

func NewOnStockWatched(userID, groupID uint64, symbol, market string) *OnStockWatched {
	return &OnStockWatched{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		UserID:          userID,
		GroupID:         groupID,
		Symbol:          symbol,
		Market:          market,
	}
}

func (e *OnStockWatched) Name() string { return OnStockWatchedEventName }

func (e *OnStockWatched) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnStockUnwatched 在一只股票被移出某个分组时抛出。
//
// 消费方（比如行情订阅）不能仅凭这一条事件就退订该标的：同一个用户可能在
// 别的分组里还盯着它。事件描述的是「这个分组里没有它了」这一件事实，
// 「还要不要订阅」是消费方自己的聚合问题。
type OnStockUnwatched struct {
	domain_event.BaseDomainEvent
	UserID  uint64 `json:"userId"`
	GroupID uint64 `json:"groupId"`
	Symbol  string `json:"symbol"`
	Market  string `json:"market"`
}

func NewOnStockUnwatched(userID, groupID uint64, symbol, market string) *OnStockUnwatched {
	return &OnStockUnwatched{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		UserID:          userID,
		GroupID:         groupID,
		Symbol:          symbol,
		Market:          market,
	}
}

func (e *OnStockUnwatched) Name() string { return OnStockUnwatchedEventName }

func (e *OnStockUnwatched) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}
