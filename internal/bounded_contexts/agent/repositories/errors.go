// Package repositories 承载 agent 上下文的持久化实现。
//
// 本包是本上下文唯一知道 mongo-driver 存在的地方，也是唯一允许开启事务的地方。
// 上层拿到的永远是实体或值对象，dtos 里那些带 bson 标签的结构不会越过这道边界。
package repositories

import (
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/mongo"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// translateMongo 是技术错误到领域错误的边界。
//
// mongo.ErrNoDocuments 绝不允许漏给上层：漏出去之后 HTTP 层会把
// 「这只票今天还没算过指标」映射成 500，而它其实是一个完全正常的 404。
func translateMongo(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	subject := fmt.Sprintf(format, args...)
	switch {
	case errors.Is(err, mongo.ErrNoDocuments):
		return custom_errors.NotFound("%s 不存在", subject).Wrap(err)
	case mongo.IsDuplicateKeyError(err):
		return custom_errors.AlreadyExists("%s 已存在", subject).Wrap(err)
	default:
		return custom_errors.Internal("%s 数据库操作失败", subject).Wrap(err)
	}
}
