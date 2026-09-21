// Package domain_services 编排模拟交易上下文的业务用例。
//
// 分层职责：
//   - 业务不变式属于 entities/（资金够不够、持仓够不够、平均成本怎么算），本层不重复判定；
//   - 事务属于 repositories/，本层不持有任何事务句柄；
//   - application/ 只放 handler，所有编排都在这里。
//
// 本层同时声明它所消费的外部端口（QuoteReader）。按 Go 惯例由消费方声明接口：
// 本包只认这个签名，实现属于 stock 上下文，由 DI 适配；测试里换成返回固定报价的桩即可。
package domain_services

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Operator 是发起调用的身份。
//
// 刻意不复用 identity 上下文的 Claims：那会让模拟交易在编译期依赖身份上下文。
// 两个限界上下文之间只该传数据，不该共享类型。
type Operator struct {
	UserID  uint64
	IsAdmin bool
}

// QuoteReader 是本上下文消费的行情端口。
//
// # 为什么只有批量方法，没有 LatestQuote(单个)
//
// 给持仓估值是「一个组合一次估值」的动作：30 只持仓必须是一次调用，
// 而不是 30 次。只要这个接口上存在单标的方法，迟早会有人写成
// `for _, p := range positions { quotes.LatestQuote(p.Code) }`——
// 那是 30 次网络往返，而且随组合规模线性恶化。
// 接口里压根不提供那个方法，是让这种写法无法出现的最简单办法。
//
// 返回切片而不是 map：调用方对「缺失的标的」有不同的处理策略
// （估值回退成本价、下单则直接报错），由调用方自己按需索引。
type QuoteReader interface {
	LatestQuotes(ctx context.Context, codes []shared_vo.StockCode) ([]value_objects.LiveQuote, error)
}

// requireLogin 是本层最基本的身份判定。
// 权限是「谁能调用」的问题，不是业务不变式，因此归本层而不是 entities/。
func requireLogin(op Operator) error {
	if op.UserID == 0 {
		return custom_errors.Unauthorized("未登录")
	}
	return nil
}

// requireOwnership 判定调用者能否操作这个账户。
//
// # 越权一律返回 NotFound，不返回 Forbidden
//
// Forbidden 等于向调用方确认「这个 ID 确实存在，只是不属于你」。
// 攻击者拿这个差异就能把接口当成账户 ID 的存在性探测器，
// 顺着枚举出别人有几个模拟账户。对无权访问者而言，「不存在」和「无权访问」
// 应当是同一个回答。
func requireOwnership(op Operator, account *entities.PaperAccount) error {
	if account == nil {
		return custom_errors.NotFound("模拟账户不存在")
	}
	if account.UserID == op.UserID || op.IsAdmin {
		return nil
	}
	return custom_errors.NotFound("模拟账户(id=%s) 不存在", account.ID)
}
