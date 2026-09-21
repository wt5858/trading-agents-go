// Package domain_services 编排定时任务上下文的业务用例。
//
// 分层职责：
//   - 业务不变式属于 entities/，本层不重复判定；
//   - 事务与 SQL 属于 repositories/，本层不持有任何事务句柄，也不认识 *gorm.DB；
//   - application/ 只放 handler，所有编排都在这里（包括由 ticker 触发的编排）。
//
// 本层同时声明它所消费的外部端口（JobRunner）。按 Go 惯例由消费方声明接口：
// 本包只认这个签名，实现属于别的上下文，测试里可以直接换成返回固定结果的桩。
package domain_services

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Operator 是发起调用的身份。
//
// 刻意不复用 identity 上下文的 Claims：那会让定时任务上下文在编译期依赖身份上下文。
// 两个限界上下文之间只该传数据，不该共享类型。这与 analysis / report 的做法一致。
type Operator struct {
	UserID  uint64
	IsAdmin bool
}

// JobRunner 是「真正干活的那段逻辑」的端口，由各业务上下文实现，本上下文只消费。
//
// ===========================================================================
// 这个接口的形状就是整个上下文的隔离边界
// ===========================================================================
//
// 入参是不透明的 JobPayload，出参是一句摘要加一个条目数。调度上下文因此
// **完全不知道**行情长什么样、分析任务怎么提交、清理要删哪张表。
// 它只知道三件事：什么时候该跑、跑了多久、跑成功没有。
//
// 如果这里改成 `Run(ctx, req analysis.Request)` 之类的具体类型，scheduling 就会在
// 编译期依赖 analysis 与 stock，而那两个上下文将来任何一次重构都会波及调度器——
// 一个本该只关心时间的模块，会因为「分析请求多了一个字段」而需要跟着改。
//
// 运行器由组装根注册（Register），不是由本包 import 进来的。依赖方向因此始终是
// 「业务上下文 -> 调度上下文的接口」，而不是反过来。
//
// 返回的 summary/itemCount 会原样进入审计记录与 OnJobExecuted 事件：
// itemCount 是运维唯一能用来判断「这次跑出效果了吗」的数字——
// 一次「成功但同步了 0 条」和一次失败同样值得关注。
type JobRunner interface {
	// Kind 是这个运行器负责的任务种类，同时是注册时的路由键。
	Kind() value_objects.JobKind
	// Run 执行一次任务。ctx 已经按任务自身的 Timeout 派生过，实现必须尊重它。
	Run(ctx context.Context, payload value_objects.JobPayload) (summary string, itemCount int, err error)
}

// requireAdmin 是本层的权限判定。
//
// 定时任务的增删改查全是管理员操作：一条定时任务会按自己的节奏消耗上游配额、
// 占用并发名额、产生费用，它的影响面是全局的而不是某个用户的。
// 权限是「谁能调用」的问题，不是业务不变式，因此归本层而不是 entities/。
func requireAdmin(op Operator) error {
	if op.UserID == 0 {
		return custom_errors.Unauthorized("未登录")
	}
	if !op.IsAdmin {
		return custom_errors.Forbidden("定时任务管理仅限管理员")
	}
	return nil
}

// requireLogin 用于只读接口：任何登录用户都可以看定时任务跑得好不好，
// 但只有管理员能改。
func requireLogin(op Operator) error {
	if op.UserID == 0 {
		return custom_errors.Unauthorized("未登录")
	}
	return nil
}
