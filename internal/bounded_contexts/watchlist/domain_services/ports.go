// Package domain_services 编排自选股上下文的业务用例。
//
// 分层职责：
//   - 业务不变式属于 entities/，本层不重复判定；
//   - 事务属于 repositories/，本层不持有任何事务句柄，签名里也不会出现 *gorm.DB；
//   - application/ 只放 handler，所有编排都在这里。
//
// 本层还有一条自选股上下文特有的纪律：**绝不直接触碰子实体**。
// 「把某只票的备注改掉」要写成 group.UpdateItemNote(...)，而不是
// item.Note = ... —— 后者在编译期就走不通（子实体的修改方法不导出），
// 这不是靠自觉，是聚合设计本身给的保证。
//
// 本层同时声明它所消费的外部端口（QuoteReader）。按 Go 惯例由消费方声明接口：
// 本包只认这些签名，实现属于另一个上下文，测试里可以直接换成返回固定结果的桩。
package domain_services

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Operator 是发起调用的身份。
//
// 刻意不复用 identity 上下文的 Claims：那会让自选股上下文在编译期依赖身份上下文。
// 两个限界上下文之间只该传数据，不该共享类型。怎么认证、Claims 存在哪个 key 里，
// 是组装根（internal/di、internal/server）的事。
type Operator struct {
	UserID  uint64
	IsAdmin bool
}

// QuoteReader 是自选股列表所需的行情端口。
//
// ===========================================================================
// 为什么它只有一个**批量**方法
// ===========================================================================
//
// 自选股列表天生就是「一次几十到两百只票」的场景。只要这个接口上存在一个
// LatestQuote(ctx, code) 这样的单只方法，调用方就一定会在 for 里调用它——
// 那是一个 200 只票的看板刷新一次打出 200 次往返，落到限流的行情源上
// （Tushare 按分钟计、Finnhub 60 次/分钟）就是几秒钟内把配额烧光。
//
// 所以这里**只提供批量方法**，一个单只方法都不留。这不是不信任调用方，
// 是让错误的写法压根写不出来：拿着一个只收切片的接口，你很难把它用成 N 次调用。
// 同样的理由让 stock 上下文的 MarketDataRepository 只有批量写入方法。
//
// # 实现在哪
//
// stock 上下文的 MarketDataRepository.LatestQuotes / StockService.LatestQuotes
// 提供能力，但它们的签名与这里并不完全一致（一个返回 stock 的 Quote 读模型，
// 一个还额外返回未命中的代码）。由 internal/di 写一个薄适配器把它们接到这个端口上——
// 端口属于消费方，适配属于组装根，两边都不必为对方让步。
//
// # 契约
//
// 返回的行情条数可以少于请求的代码数：没有行情数据的标的（新股、停牌、
// 数据源尚未覆盖）直接不出现在结果里，而不是返回一条价格为 0 的占位。
// 自选股列表把这种情况显示成「暂无行情」，那比显示 0.00 元诚实得多。
type QuoteReader interface {
	LatestQuotes(ctx context.Context, codes []shared_vo.StockCode) ([]value_objects.QuoteSnapshot, error)
}

// requireLogin 是本层最基本的身份判定。
// 权限是「谁能调用」的问题，不是业务不变式，因此归本层而不是 entities/。
func requireLogin(op Operator) error {
	if op.UserID == 0 {
		return custom_errors.Unauthorized("未登录")
	}
	return nil
}
