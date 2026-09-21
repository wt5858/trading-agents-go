// Package logger 封装 zap，提供全局与带请求上下文的日志器。
package logger

import (
	"context"
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/wt5858/trading-agents-go/config"
)

type ctxKey struct{}

var global = zap.NewNop()

// Init 依据配置初始化全局日志器。
func Init(cfg config.Log) (*zap.Logger, error) {
	level := zapcore.InfoLevel
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		level = zapcore.InfoLevel
	}

	encCfg := zap.NewProductionEncoderConfig()
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.EncodeLevel = zapcore.CapitalLevelEncoder

	var encoder zapcore.Encoder
	if cfg.Format == "json" {
		encoder = zapcore.NewJSONEncoder(encCfg)
	} else {
		encCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		encoder = zapcore.NewConsoleEncoder(encCfg)
	}

	syncers := []zapcore.WriteSyncer{zapcore.AddSync(os.Stdout)}
	if cfg.File != "" {
		f, err := os.OpenFile(cfg.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		syncers = append(syncers, zapcore.AddSync(f))
	}

	core := zapcore.NewCore(encoder, zapcore.NewMultiWriteSyncer(syncers...), level)
	l := zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))
	global = l
	zap.ReplaceGlobals(l)
	return l, nil
}

// L 返回全局日志器。
func L() *zap.Logger { return global }

// With 把带字段的日志器塞进 context，供跨层传递 request_id / task_id。
//
// 注意它是在**当前**上下文日志器的基础上加字段，ctx 里没有就以全局那个为底。
// 也就是说 Init 之前调用它，得到的是一个 Nop 派生出来的日志器——
// 写起来没有任何异常，只是后面什么都不会输出。需要指定底座时用 NewContext。
func With(ctx context.Context, fields ...zap.Field) context.Context {
	return context.WithValue(ctx, ctxKey{}, FromContext(ctx).With(fields...))
}

// NewContext 把指定的日志器放进 context，作为后续 With / WithTraceID 的底座。
//
// 有两个用处：给某个组件挂一个带固定字段的日志器（而不是全靠全局那个），
// 以及在测试里塞一个可观测的 logger——否则 Init 没跑时全局是 Nop，
// 断言「这条日志带没带 trace_id」根本无从下手。
func NewContext(ctx context.Context, l *zap.Logger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext 取出上下文日志器，没有则退回全局。
func FromContext(ctx context.Context) *zap.Logger {
	return FromContextOr(ctx, nil)
}

// FromContextOr 取上下文日志器，没有则退回调用方给的那个，再没有才退回全局。
//
// 存在的理由是 FromContext 的退路太硬：它只认全局日志器，于是任何「我手上已经
// 有一个配好的 logger」的调用方——基础设施适配器几乎全是这样——都会在 ctx 不带
// 日志器时把自己那个悄悄丢掉。这在进程里看不出问题（全局那个也是初始化过的），
// 但只要 Init 还没跑，全局就还是 Nop，日志直接消失得无声无息。
func FromContextOr(ctx context.Context, fallback *zap.Logger) *zap.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKey{}).(*zap.Logger); ok && l != nil {
			return l
		}
	}
	if fallback != nil {
		return fallback
	}
	return global
}

// ---------------------------------------------------------------------------
// 链路追踪
//
// trace_id 在 context 里存两份：一份已经烤进上下文日志器的字段里（打日志用），
// 一份是裸字符串（要把它塞进 AMQP 消息头时用）。
//
// 只存日志器不够——zap.Logger 没有公开的字段读取接口，要往消息头里塞的时候
// 拿不出那个值；只存裸串则意味着每个打日志的地方都得记得自己带上它，
// 而「记得带」这件事一定会在某个新写的分支里漏掉。
// ---------------------------------------------------------------------------

// TraceIDField 是 trace_id 在日志里的字段名。跨进程查同一条链路时按它检索。
const TraceIDField = "trace_id"

type traceIDKey struct{}

// WithTraceID 同时写入裸 trace_id 与带该字段的上下文日志器。
//
// 这是整条链路的入口，只有两个调用点：HTTP 中间件在请求进来时调一次，
// MQ 消费者在收到消息时拿消息头里的值调一次。此后任何拿到这个 ctx 的代码——
// 领域服务、仓储、直到 GORM 的回调——打出来的日志都自动带上它，不需要逐层传参。
func WithTraceID(ctx context.Context, traceID string) context.Context {
	if traceID == "" {
		return ctx
	}
	ctx = context.WithValue(ctx, traceIDKey{}, traceID)
	return With(ctx, zap.String(TraceIDField, traceID))
}

// TraceIDFromContext 取裸 trace_id，没有则返回空串。
//
// 空串不是错误：worker 里的定时任务不由任何 HTTP 请求触发，本来就没有上游链路。
// 调用方该做的是不写这个头，而不是补一个假的。
func TraceIDFromContext(ctx context.Context) string {
	if s, ok := ctx.Value(traceIDKey{}).(string); ok {
		return s
	}
	return ""
}
