package repositories

import (
	"errors"
	"strings"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// translate is the boundary between technical and domain errors.
// Nothing above this layer ever sees a gorm or driver type.
func translate(err error, action string) error {
	if err == nil {
		return nil
	}
	// A domain error we raised ourselves passes straight through. Without this, any
	// NotFound/Conflict produced inside a transaction callback gets wrapped as Internal
	// on the way out — a 404 turns into a 500 and the caller loses its retry signal.
	// Every other context's translate opens with this; identity was the odd one out.
	var de *custom_errors.Error
	if errors.As(err, &de) {
		return de
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == 1062 {
		return custom_errors.AlreadyExists("%s失败：记录已存在", action).Wrap(err)
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return custom_errors.NotFound("%s失败：记录不存在", action).Wrap(err)
	}
	return custom_errors.Internal("%s失败", action).Wrap(err)
}

// escapeLike neutralises LIKE wildcards so a user-supplied "%" cannot turn a keyword
// search into a full table scan.
func escapeLike(s string) string {
	return strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(s)
}
