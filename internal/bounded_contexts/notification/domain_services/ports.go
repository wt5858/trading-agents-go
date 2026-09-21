// Package domain_services 编排站内通知上下文的业务用例。
//
// 分层职责：
//   - 业务不变式属于 entities/，本层不重复判定；
//   - 事务属于 repositories/，本层不持有任何事务句柄；
//   - application/ 只放 handler（HTTP 与领域事件两种入口），所有编排都在这里。
//
// 本上下文是别的上下文的**消费者**，因此它有一个别处没有的特点：
// 写入路径的调用方不是用户，而是事件处理器。这也是 Notify 不收 Operator 的原因，
// 见 notification_service.go。
package domain_services

import (
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Operator 是发起调用的身份。
//
// 刻意不复用 identity 上下文的 Claims：那会让通知上下文在编译期依赖身份上下文。
// 两个限界上下文之间只该传数据，不该共享类型。怎么认证、Claims 存在哪个 key 里，
// 是组装根（internal/server）的事。
type Operator struct {
	UserID  uint64
	IsAdmin bool
}

// requireLogin 是本层最基本的身份判定。
// 权限是「谁能调用」的问题，不是业务不变式，因此归本层而不是 entities/。
func requireLogin(op Operator) error {
	if op.UserID == 0 {
		return custom_errors.Unauthorized("未登录")
	}
	return nil
}
