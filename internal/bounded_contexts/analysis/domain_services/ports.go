// Package domain_services 编排分析上下文的业务用例。
//
// 分层职责：
//   - 业务不变式属于 entities/，本层不重复判定；
//   - 事务属于 repositories/，本层不持有任何事务句柄；
//   - application/ 只放 handler，所有编排都在这里。
//
// 本层同时声明它所消费的外部端口（Engine / ProgressReporter）。
// 按 Go 惯例由消费方声明接口：本包只认这些签名，实现属于另一个上下文，
// 测试里可以直接换成返回固定结果的桩。
package domain_services

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Operator 是发起调用的身份。
//
// 刻意不复用 identity 上下文的 Claims：那会让分析上下文在编译期依赖身份上下文。
// 两个限界上下文之间只该传数据，不该共享类型。
type Operator struct {
	UserID  uint64
	IsAdmin bool
}

// ProgressReporter 是引擎向外汇报进度的窄接口。
//
// 声明成这两个方法、而不是把 ProgressPublisher 整个交给引擎：引擎只需要
// 「说一声第几步完成了」。给它发布器就等于给了它写 Redis 的能力，
// 也给了它绕过 Task 聚合直接改进度的机会——那样推送出去的进度会和库里分叉。
type ProgressReporter interface {
	Step(key, detail string)
	StepFailed(key, detail string)
}

// Engine 是多智能体分析引擎端口，由 agent 上下文实现，本上下文只消费。
//
// 它收 Request、回 Result，全程不碰 Task 聚合：任务的状态机不该被引擎左右，
// 引擎只负责「给定输入算出结论」这一件事。
//
// runID 是本次运行的身份，传任务 ID。它不是 Task 聚合的引用，只是一个字符串——
// 跨限界上下文只传 ID 与值对象，把 Task 递进去就等于把状态机也递了进去。
// 引擎拿它给运行轨迹当主键，好让「这个任务当时到底怎么跑的」可以被反查；
// 没有对应任务的调用方（脚本、回测）传空串，此时轨迹不落库。
type Engine interface {
	Run(ctx context.Context, runID string, req value_objects.Request, reporter ProgressReporter) (*value_objects.Result, error)
}

// DecisionChainReader 是决策链读取端口，由 agent 上下文实现，本上下文只消费。
//
// 单独声明一个窄接口而不是把这个方法挂到 Engine 上：Engine 的语义是
// 「给定输入算出结论」，它是一条写路径。把一个读查询塞进去，
// 任何需要读决策链的地方就都拿到了发起一次分析的能力。
type DecisionChainReader interface {
	// DecisionChain 按运行 ID（即任务 ID）取一次分析的决策链。
	// 轨迹不存在时回 NotFound——深度浅、跑得早的任务可能根本没有轨迹。
	DecisionChain(ctx context.Context, runID string) (*value_objects.DecisionChain, error)
}

// Limits 是并发额度配置。0 表示该维度不限流。
type Limits struct {
	PerUser int
	Global  int
}

// requireLogin 是本层最基本的身份判定。
// 权限是「谁能调用」的问题，不是业务不变式，因此归本层而不是 entities/。
func requireLogin(op Operator) error {
	if op.UserID == 0 {
		return custom_errors.Unauthorized("未登录")
	}
	return nil
}
