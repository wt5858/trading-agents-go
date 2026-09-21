// Package idx 生成业务标识符。
package idx

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// New 生成无连字符的 UUID。
func New() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// Prefixed 生成带业务前缀和日期段的 ID，例如 task_20260915_a1b2c3d4e5f6。
// 前缀让日志和数据库里的 ID 自解释，日期段便于按天归档与排查。
func Prefixed(prefix string) string {
	return prefix + "_" + time.Now().Format("20060102") + "_" + New()[:12]
}

func TaskID() string    { return Prefixed("task") }
func BatchID() string   { return Prefixed("batch") }
func ReportID() string  { return Prefixed("rpt") }
func SessionID() string { return New() }
