package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/config"
	"github.com/wt5858/trading-agents-go/internal/helpers/logger"
)

// observed 造一个把日志收进内存的 logger，供断言字段用。
func observed() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)
	return zap.New(core), logs
}

func fakeSQL() (string, int64) { return "SELECT * FROM users WHERE id = 1", 1 }

// TestTraceIDReachesSQLLog 钉住这次改动的核心承诺：HTTP 请求带进来的 trace_id
// 要出现在 SQL 日志行上。
//
// 以前断的就是这一环——Trace 的签名是 _ context.Context，上下文直接丢掉，
// 于是无论上游怎么标记，SQL 日志都是匿名的。
func TestTraceIDReachesSQLLog(t *testing.T) {
	log, logs := observed()
	l := newGormLogger(config.Log{SQL: true, SQLArgs: true}, log)

	// NewContext 先把可观测的 logger 当作底座塞进去，WithTraceID 再在它上面加字段。
	// 少了这一步，WithTraceID 会以全局日志器为底——而测试里 Init 没跑，全局是 Nop，
	// 于是断言的不是「带没带 trace_id」，而是「有没有日志」。
	ctx := logger.WithTraceID(logger.NewContext(context.Background(), log), "trace-abc-123")
	l.Trace(ctx, time.Now(), fakeSQL, nil)

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("期望 1 条日志，实际 %d 条", len(entries))
	}
	got := entries[0].ContextMap()[logger.TraceIDField]
	if got != "trace-abc-123" {
		t.Fatalf("SQL 日志里的 %s = %v，期望 trace-abc-123", logger.TraceIDField, got)
	}
}

// TestSQLLoggedAtInfo 成功查询必须打在 info 上。
//
// 打在 debug 上编译和测试都不会有意见，但默认级别是 info，
// 结果就是「开了 log.sql 却一行都看不到」——这正是这次要修的现象。
func TestSQLLoggedAtInfo(t *testing.T) {
	log, logs := observed()
	l := newGormLogger(config.Log{SQL: true, SQLArgs: true}, log)

	l.Trace(context.Background(), time.Now(), fakeSQL, nil)

	if n := logs.FilterLevelExact(zap.InfoLevel).Len(); n != 1 {
		t.Fatalf("期望 1 条 info 级 SQL 日志，实际 %d 条", n)
	}
}

// TestSQLOffStillLogsFailures 关掉 log.sql 只该让成功的查询闭嘴。
//
// 把失败和慢查询一起关掉的开关是个陷阱：真出故障时，
// 「日志里什么都没有」会被读成「这里没问题」。
func TestSQLOffStillLogsFailures(t *testing.T) {
	off := config.Log{SQL: false, SQLArgs: true}

	t.Run("成功查询不打", func(t *testing.T) {
		log, logs := observed()
		newGormLogger(off, log).Trace(context.Background(), time.Now(), fakeSQL, nil)
		if n := logs.Len(); n != 0 {
			t.Fatalf("期望 0 条，实际 %d 条", n)
		}
	})

	t.Run("执行失败仍要打", func(t *testing.T) {
		log, logs := observed()
		newGormLogger(off, log).Trace(context.Background(), time.Now(), fakeSQL, errors.New("boom"))
		if n := logs.FilterLevelExact(zap.ErrorLevel).Len(); n != 1 {
			t.Fatalf("期望 1 条 error，实际 %d 条", n)
		}
	})

	t.Run("慢查询仍要打", func(t *testing.T) {
		log, logs := observed()
		// begin 往前推 1s，必然超过 200ms 阈值。
		newGormLogger(off, log).Trace(context.Background(), time.Now().Add(-time.Second), fakeSQL, nil)
		if n := logs.FilterLevelExact(zap.WarnLevel).Len(); n != 1 {
			t.Fatalf("期望 1 条 warn 慢查询，实际 %d 条", n)
		}
	})
}

// TestRecordNotFoundIsNotAnError 「没查到」在 FindByX 里是正常分支，
// 打成 error 会把真正的故障淹掉。
func TestRecordNotFoundIsNotAnError(t *testing.T) {
	log, logs := observed()
	l := newGormLogger(config.Log{SQL: false}, log)

	// 包一层，顺带钉住 isRecordNotFound 用的是 errors.Is 而不是 ==。
	l.Trace(context.Background(), time.Now(), fakeSQL, fmtWrap(gorm.ErrRecordNotFound))

	if n := logs.FilterLevelExact(zap.ErrorLevel).Len(); n != 0 {
		t.Fatalf("ErrRecordNotFound 不该打成 error，实际 %d 条", n)
	}
}

func fmtWrap(err error) error { return errWrap{err} }

type errWrap struct{ error }

func (e errWrap) Unwrap() error { return e.error }

// TestParamsFilterRedacts 脱敏必须发生在参数和 SQL 拼起来**之前**。
//
// GORM 交给 Trace 的字符串已经是内联后的结果，那时再想摘掉值只能靠正则猜，
// 猜错一次就是把邮箱或口令哈希写进日志。
func TestParamsFilterRedacts(t *testing.T) {
	const sql = "SELECT * FROM users WHERE email = ? AND status = ?"
	args := []any{"someone@example.com", 1}

	t.Run("生产档丢弃参数", func(t *testing.T) {
		l := newGormLogger(config.Log{SQL: true, SQLArgs: false}, zap.NewNop()).(*gormZapLogger)
		gotSQL, gotArgs := l.ParamsFilter(context.Background(), sql, args...)
		if gotArgs != nil {
			t.Fatalf("期望参数被清空，实际 %v", gotArgs)
		}
		if gotSQL != sql {
			t.Fatalf("SQL 模板不该被改写，实际 %q", gotSQL)
		}
	})

	t.Run("开发档保留参数", func(t *testing.T) {
		l := newGormLogger(config.Log{SQL: true, SQLArgs: true}, zap.NewNop()).(*gormZapLogger)
		_, gotArgs := l.ParamsFilter(context.Background(), sql, args...)
		if len(gotArgs) != 2 {
			t.Fatalf("期望保留 2 个参数，实际 %v", gotArgs)
		}
	})
}
