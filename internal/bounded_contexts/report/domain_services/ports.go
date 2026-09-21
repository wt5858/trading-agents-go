// Package domain_services 编排报告上下文的业务用例。
//
// 分层职责：
//   - 业务不变式属于 entities/，本层不重复判定；
//   - 事务属于 repositories/，本层不持有任何事务句柄；
//   - application/ 只放 handler，所有编排都在这里。
//
// 本层同时声明它所消费的外部端口。按 Go 惯例由消费方声明接口：
// 本包只认这些签名，实现属于另一个上下文或组装根，测试里可以直接换成桩。
package domain_services

import (
	"context"

	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Operator 是发起调用的身份。
//
// 刻意不复用 identity 上下文的 Claims：那会让报告上下文在编译期依赖身份上下文。
// 两个限界上下文之间只该传数据，不该共享类型。
type Operator struct {
	UserID  uint64
	IsAdmin bool
}

// AnalysisResultProvider 是报告上下文向分析上下文索取「已完成分析结论」的窄端口。
//
// # 为什么需要这个端口
//
// 触发报告生成的 OnTaskCompleted 事件只带标量快照（代码、动作、置信度），不带整份
// Result——事件要能被序列化投递到进程外，塞十几段 markdown 进去是不现实的。
// 因此报告生成时必须回查一次结论。
//
// # 为什么是接口而不是直接注入 analysis 的 TaskRepository
//
// 直接注入等于让报告上下文在编译期依赖另一个上下文的仓储实现，连它的表结构变更
// 都会波及过来。声明成窄接口后，本包只依赖 analysis 的**值对象**（一份不可变快照，
// 跨上下文传递是安全的），适配器由组装根提供。
//
// 返回的是 Result 值对象而不是 Task 实体：跨聚合只按标识与值对象引用。
type AnalysisResultProvider interface {
	// ResultOf 返回指定任务的分析结论。
	// 任务不存在、或尚未产出结论时返回 custom_errors.NotFound。
	ResultOf(ctx context.Context, taskID string) (*analysis_vo.Result, error)
}

// requireLogin 是本层最基本的身份判定。
// 权限是「谁能调用」的问题，不是业务不变式，因此归本层而不是 entities/。
func requireLogin(op Operator) error {
	if op.UserID == 0 {
		return custom_errors.Unauthorized("未登录")
	}
	return nil
}
