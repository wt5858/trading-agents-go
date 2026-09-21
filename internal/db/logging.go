package db

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/event"
	"go.uber.org/zap"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/wt5858/trading-agents-go/config"
	"github.com/wt5858/trading-agents-go/internal/helpers/logger"
)

// ---------------------------------------------------------------------------
// 三个数据源的命令日志
//
// 共同的规矩，改任何一个适配器之前先读这里：
//
//  1. 日志器一律从 ctx 取。HTTP 中间件已经把 trace_id 烤进上下文日志器里，
//     所以从处理器一路传下来的 ctx 打出的每条 SQL / Redis / Mongo 命令
//     都自带链路号——这一层不需要知道 trace_id 是谁塞进去的，也不需要逐层传参。
//  2. cfg.Log.SQL 只管「成功的命令打不打」。失败与慢查询无论如何都打：
//     关掉命令日志是为了省日志量，不是为了把故障藏起来。
//  3. cfg.Log.SQLArgs 管「参数值留不留明文」。SQL 的绑定值、Redis 的命令参数、
//     Mongo 的命令文档是同一类东西——都是业务数据本身，生产默认全部脱敏。
// ---------------------------------------------------------------------------

// ctxLogger 取该条命令该用的日志器。
//
// 拿不到带 trace_id 的那个不是错误：worker 的定时任务、启动期迁移本来就不属于
// 任何一次请求，结果只是少一个字段。
//
// 退路是 fallback 而不是全局日志器。这一层是带着自己的 logger 被造出来的
// （newGormLogger / openRedis / openMongo 都收了一个），ctx 里没有就该用它——
// 退回全局等于那个参数形同虚设，而且 logger.Init 之前全局还是 Nop，
// 启动期的建连与迁移日志会一条不剩地消失。
func ctxLogger(ctx context.Context, fallback *zap.Logger) *zap.Logger {
	return logger.FromContextOr(ctx, fallback)
}

// ---------------------------------------------------------------------------
// MySQL / GORM
// ---------------------------------------------------------------------------

// gormZapLogger 把 GORM 的日志桥接到 zap，避免服务里出现两套日志格式。
type gormZapLogger struct {
	log           *zap.Logger
	slowThreshold time.Duration
	logSQL        bool
	logArgs       bool
}

// 编译期钉住 ParamsFilter：脱敏整个挂在这个接口上，哪天签名对不上了，
// GORM 只会安静地跳过它，SQL 里的参数值就又变回明文——那种缺陷没人会发现。
var _ gorm.ParamsFilter = (*gormZapLogger)(nil)

func newGormLogger(cfg config.Log, log *zap.Logger) gormlogger.Interface {
	return &gormZapLogger{
		log:           log,
		slowThreshold: 200 * time.Millisecond,
		logSQL:        cfg.SQL,
		logArgs:       cfg.SQLArgs,
	}
}

func (l *gormZapLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface { return l }

func (l *gormZapLogger) Info(ctx context.Context, msg string, args ...any) {
	ctxLogger(ctx, l.log).Sugar().Infof(msg, args...)
}

func (l *gormZapLogger) Warn(ctx context.Context, msg string, args ...any) {
	ctxLogger(ctx, l.log).Sugar().Warnf(msg, args...)
}

func (l *gormZapLogger) Error(ctx context.Context, msg string, args ...any) {
	ctxLogger(ctx, l.log).Sugar().Errorf(msg, args...)
}

// ParamsFilter 是 GORM 在把参数拼进日志用 SQL 之前给出的唯一插手点。
//
// 交给 Trace 的那个字符串已经是参数内联之后的结果，等日志器拿到手再想把值摘掉，
// 就只能靠正则猜哪一段是值——猜错一次就是把邮箱、口令哈希写进日志文件。
// 这里直接把参数列表清空，Explain 没有值可填就原样留下 ? 占位符，
// 于是脱敏发生在值和 SQL 拼起来之前，而不是之后。
//
// 它盖不住两处，别指望它是个安全边界：
//   - 调用方自己拼进 SQL 字符串的字面量。Raw("... WHERE email = 'a@b'")、
//     gorm.Expr 里的常量、fmt.Sprintf 出来的条件，在 stmt.SQL 里就是文本的一部分，
//     这里分辨不出哪段是值。避开的办法只有一条：条件一律用 ? 传参。
//   - Row() / Rows() 这条路。GORM 在那里临时换成自己的 Recorder 日志器去录 SQL，
//     录的时候不会回调 ParamsFilter，录下来的就是内联后的完整语句。
func (l *gormZapLogger) ParamsFilter(_ context.Context, sql string, params ...any) (string, []any) {
	if l.logArgs {
		return sql, params
	}
	return sql, nil
}

func (l *gormZapLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	elapsed := time.Since(begin)

	// fc() 要把参数逐个转成字符串再拼进 SQL，不便宜。所以先判断这条到底打不打，
	// 确定要打了再调——关掉 SQL 日志时省下的正是这部分开销。
	switch {
	case err != nil && !isRecordNotFound(err):
		sql, rows := fc()
		ctxLogger(ctx, l.log).Error("SQL 执行失败",
			zap.String("sql", sql), zap.Int64("rows", rows),
			zap.Duration("elapsed", elapsed), zap.Error(err))
	case elapsed > l.slowThreshold:
		sql, rows := fc()
		ctxLogger(ctx, l.log).Warn("慢查询",
			zap.String("sql", sql), zap.Int64("rows", rows), zap.Duration("elapsed", elapsed))
	case l.logSQL:
		// 成功的查询打 info 而不是 debug：默认级别就是 info，打在 debug 上等于
		// 「开了 log.sql 却什么也没看到」，而下一步就是有人去调全局级别，
		// 把每个库的调试输出一起放出来。
		sql, rows := fc()
		ctxLogger(ctx, l.log).Info("SQL",
			zap.String("sql", sql), zap.Int64("rows", rows), zap.Duration("elapsed", elapsed))
	}
}

// isRecordNotFound 把「没查到」排除在错误日志之外：它在 FindByX 里是正常分支，
// 打成 error 级别会把真正的故障淹没掉。
func isRecordNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// ---------------------------------------------------------------------------
// Redis
// ---------------------------------------------------------------------------

// redisZapHook 给每条 Redis 命令补上 trace_id 与耗时。
type redisZapHook struct {
	log     *zap.Logger
	logCmds bool
	logArgs bool
}

var _ redis.Hook = (*redisZapHook)(nil)

func newRedisHook(cfg config.Log, log *zap.Logger) *redisZapHook {
	return &redisZapHook{log: log, logCmds: cfg.SQL, logArgs: cfg.SQLArgs}
}

// DialHook 原样放行：建连接失败最终会以命令错误的形式从 ProcessHook 出来，
// 在这里再打一遍只会让同一次故障出现两条日志。
func (h *redisZapHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *redisZapHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		h.record(ctx, cmd, time.Since(start), err)
		return err
	}
}

func (h *redisZapHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		elapsed := time.Since(start)

		log := ctxLogger(ctx, h.log)
		// 管道的返回值只反映整批的收发结果，单条命令的错误挂在命令自己身上，
		// 所以要逐条取，否则「批成功、其中一条失败」会完全无声。
		for _, cmd := range cmds {
			if e := cmd.Err(); e != nil && !isRedisMiss(e) {
				log.Error("Redis 管道内命令失败",
					zap.String("cmd", h.describe(cmd)), zap.Error(e))
			}
		}
		if err != nil && !isRedisMiss(err) {
			log.Error("Redis 管道执行失败",
				zap.Int("cmds", len(cmds)), zap.Duration("elapsed", elapsed), zap.Error(err))
			return err
		}
		if h.logCmds {
			// 整批一条日志，不逐条打：耗时本来就是整批的，拆到每条上是假精度。
			log.Info("Redis 管道",
				zap.Int("cmds", len(cmds)), zap.Duration("elapsed", elapsed))
		}
		return err
	}
}

func (h *redisZapHook) record(ctx context.Context, cmd redis.Cmder, elapsed time.Duration, err error) {
	switch {
	case err != nil && !isRedisMiss(err):
		ctxLogger(ctx, h.log).Error("Redis 命令失败",
			zap.String("cmd", h.describe(cmd)), zap.Duration("elapsed", elapsed), zap.Error(err))
	case h.logCmds:
		ctxLogger(ctx, h.log).Info("Redis",
			zap.String("cmd", h.describe(cmd)), zap.Duration("elapsed", elapsed))
	}
}

// isRedisMiss 把 redis.Nil 挡在错误日志外。
//
// 理由和 isRecordNotFound 一样：缓存没命中是 Get 的正常分支，每次未命中都打一条
// error，真正的连接故障就会被埋在里面。
func isRedisMiss(err error) bool {
	return errors.Is(err, redis.Nil)
}

// describe 决定一条 Redis 命令在日志里留下多少信息。
//
// 脱敏时只留命令名和参数个数，键名也一起去掉——键名不是元数据，
// Set("session:"+email, token) 这一条里，键和值都是业务数据。
// 留下的信息够回答「这段时间打了多少次 GET、慢不慢」，不够复原打的是谁的数据。
func (h *redisZapHook) describe(cmd redis.Cmder) string {
	if h.logArgs {
		// cmd.String() 连命令的返回值一起带出来，只有明文模式下才用。
		return cmd.String()
	}
	args := len(cmd.Args()) - 1 // 第一个是命令名本身
	if args < 0 {
		args = 0
	}
	return cmd.Name() + " (" + strconv.Itoa(args) + " args)"
}

// ---------------------------------------------------------------------------
// MongoDB
// ---------------------------------------------------------------------------

// mongoCmdMonitor 把驱动的命令事件翻成日志。
//
// 一条命令拆成两个事件：集合名只在 started 里（藏在命令文档的第一个字段），
// 耗时只在 succeeded/failed 里。要在同一行日志里同时给出两者，就得把 started
// 这一半按 RequestID 暂存起来，等结束事件到了再合并。
type mongoCmdMonitor struct {
	log     *zap.Logger
	logCmds bool
	logArgs bool

	// inflight 只装在途命令。驱动保证每个 started 后面恰好跟一个 succeeded
	// 或 failed，两边都会 LoadAndDelete，所以这里的量级是并发数而不是请求总数。
	inflight sync.Map // int64(RequestID) -> mongoCmdStart
}

type mongoCmdStart struct {
	collection string
	command    string // 仅在允许明文时填
}

func newMongoMonitor(cfg config.Log, log *zap.Logger) *event.CommandMonitor {
	m := &mongoCmdMonitor{log: log, logCmds: cfg.SQL, logArgs: cfg.SQLArgs}
	return &event.CommandMonitor{
		Started:   m.started,
		Succeeded: m.succeeded,
		Failed:    m.failed,
	}
}

func (m *mongoCmdMonitor) started(_ context.Context, evt *event.CommandStartedEvent) {
	// 即使关掉了命令日志也要记：失败日志还要用集合名，而集合名只有这里有。
	start := mongoCmdStart{collection: mongoCollection(evt)}
	if m.logArgs {
		// CommandStartedEvent.Command 是完整的 BSON 文档，字段值全在里面——
		// 和内联参数后的 SQL 是同一类东西，按 SQLArgs 处理，不是按 SQL 处理。
		//
		// 必须在这里就转成字符串：bson.Raw 指的是驱动的网络缓冲区，
		// 回调返回之后那块内存会被回收复用，留着引用读到的是下一条命令的字节。
		start.command = evt.Command.String()
	}
	m.inflight.Store(evt.RequestID, start)
}

func (m *mongoCmdMonitor) succeeded(ctx context.Context, evt *event.CommandSucceededEvent) {
	m.finish(ctx, &evt.CommandFinishedEvent, "")
}

func (m *mongoCmdMonitor) failed(ctx context.Context, evt *event.CommandFailedEvent) {
	m.finish(ctx, &evt.CommandFinishedEvent, evt.Failure)
}

func (m *mongoCmdMonitor) finish(ctx context.Context, evt *event.CommandFinishedEvent, failure string) {
	// 无论打不打日志都要取走：留在 map 里就是泄漏。
	var start mongoCmdStart
	if v, ok := m.inflight.LoadAndDelete(evt.RequestID); ok {
		start, _ = v.(mongoCmdStart)
	}
	if failure == "" && !m.logCmds {
		return
	}

	fields := []zap.Field{
		zap.String("cmd", evt.CommandName),
		zap.String("db", evt.DatabaseName),
		zap.String("collection", start.collection),
		zap.Duration("elapsed", evt.Duration),
	}
	if start.command != "" {
		fields = append(fields, zap.String("command", start.command))
	}
	if failure != "" {
		ctxLogger(ctx, m.log).Error("Mongo 命令失败", append(fields, zap.String("failure", failure))...)
		return
	}
	ctxLogger(ctx, m.log).Info("Mongo", fields...)
}

// mongoCollection 从命令文档里取集合名。
//
// Mongo 的线协议里命令文档的第一个字段就是命令本身，它的值是集合名——
// find / insert / update / aggregate 这些集合级命令都成立。ping、listDatabases
// 这类库级命令那个位置放的是 1，取不到字符串就留空，不值得为它编一个假名字。
// 驱动对认证类命令会把整个文档抹掉，那时同样取不到，也是留空。
func mongoCollection(evt *event.CommandStartedEvent) string {
	elem, err := evt.Command.IndexErr(0)
	if err != nil {
		return ""
	}
	name, ok := elem.Value().StringValueOK()
	if !ok {
		return ""
	}
	return name
}
