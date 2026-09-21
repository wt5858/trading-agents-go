// Package domain_services 编排选股筛选上下文的业务用例。
//
// 分层职责：
//   - 业务不变式属于 entities/，本层不重复判定；
//   - 事务属于 repositories/，本层不持有任何事务句柄，签名里也不会出现 *gorm.DB；
//   - application/ 只放 handler，所有编排都在这里。
//
// 本层还有一条本上下文特有的纪律：**绝不直接触碰子实体**。
// 「改一条筛选条件的取值」要写成 template.UpdateCriterion(...)，而不是
// criterion.Spec = ... —— 后者在编译期就走不通（子实体的修改方法不导出），
// 这不是靠自觉，是聚合设计本身给的保证。
//
// 本层同时声明它所消费的外部端口（StockScreener）。按 Go 惯例由消费方声明接口：
// 本包只认这些签名，实现属于 repositories/，测试里可以直接换成返回固定结果的桩。
package domain_services

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Operator 是发起调用的身份。
//
// 刻意不复用 identity 上下文的 Claims：那会让选股上下文在编译期依赖身份上下文。
// 两个限界上下文之间只该传数据，不该共享类型。怎么认证、Claims 存在哪个 key 里，
// 是组装根（internal/di、internal/server）的事。
//
// # 关于命名：它和 value_objects.CompareOperator 不是一回事
//
// 本上下文里「operator」这个词天然有两个意思：发起调用的**操作者**，
// 以及筛选条件里的**比较符**。两者都叫 Operator 会让 `op` 这个变量名
// 在不同文件里指代不同的东西，读代码时必须回头看类型才能确定。
//
// 分法是：本层这个保持全项目统一的 Operator（analysis / watchlist 都叫这个，
// 改名反而会让跨上下文阅读变别扭），比较符那个叫 CompareOperator。
// 带前缀的是后来者，而且 Compare 这个前缀本身就说明了它比较的是什么。
type Operator struct {
	UserID  uint64
	IsAdmin bool
}

// StockScreener 是执行一次筛选的端口，由 repositories.StockScreener 实现。
//
// ===========================================================================
// 这个接口的形状就是「把过滤下推到数据库」这条规则的表达
// ===========================================================================
//
// 它收一个 ScreenQuery（条件 + 排序 + 条数），回一个已经筛好、排好、
// 截断好的结果集。**它没有任何一个「把股票池给我」的方法**——
// 没有 AllStocks()，没有 StocksByMarket()，没有 LatestQuotes(codes)。
//
// 这是刻意的，而且是本接口最重要的设计：只要这里存在一个能拿到股票列表的方法，
// 调用方就一定会写出「取全部股票 → 在 Go 里 for 一遍 if」的实现，
// 而那是一个 5000 只票的筛选跑几十秒、并发几个用户就 OOM 的实现。
// 接口上压根不给那条路，正确的实现就是唯一能写出来的实现。
//
// 同理，排序和条数也进了入参而不是留给调用方在内存里做：
// 「在内存里排序再截断」的前提就是先把全部命中行拉回来，
// 那样 limit 就只是一次浪费之后的装饰。
//
// 实现怎么把条件翻译成一条索引化的查询、跨存储的条件怎么求交，
// 是实现的事，本层一概不知道——本层甚至不知道数据落在 MySQL 还是 Mongo。
type StockScreener interface {
	Screen(ctx context.Context, query value_objects.ScreenQuery) (value_objects.ScreeningResultSet, error)
}

// requireLogin 是本层最基本的身份判定。
// 权限是「谁能调用」的问题，不是业务不变式，因此归本层而不是 entities/。
func requireLogin(op Operator) error {
	if op.UserID == 0 {
		return custom_errors.Unauthorized("未登录")
	}
	return nil
}
