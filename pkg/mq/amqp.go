// Package mq 是本服务自己的 AMQP（RabbitMQ）客户端。
//
// # 为什么自己写
//
// 它刻意不依赖任何私有仓库：一个只被内部网络托管的包，会让这个项目在任何
// 拿不到那个网络的地方（新同事的机器、CI 的干净容器、开源出去之后）直接构建失败。
// 这里需要的能力其实很薄——声明拓扑、发消息、起消费者、失败重投——
// 薄到自己写一份的成本低于长期背着一个取不到的依赖。
//
// # 它提供什么
//
//	声明式拓扑   交换机与队列写在配置里，启动时一次性声明，代码里只出现队列名
//	发布确认     发出去的消息等 broker 确认，publish 返回 nil 就代表 broker 收下了
//	有界重试     处理失败先延迟重投，超过次数进死信队列，绝不原地打转
//	断线自愈     连接或信道断开时重连、重新声明拓扑、重新拉起全部消费者
//
// # 它刻意不提供什么
//
// 没有事务、没有消费端排序保证、没有 exactly-once。投递语义是**至少一次**，
// 因此每个处理器都必须自己幂等——这不是本包的疏漏，而是分布式消息的事实，
// 任何声称帮你解决了它的抽象都只是把问题藏在了别处。
package mq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
)

// Handler 处理一条消息。
//
// 返回 nil 表示处理完毕，消息被确认；返回错误表示本次没处理成功，
// 消息会被延迟重投，超过次数后进死信队列。
//
// 关键约定：**处理器必须幂等**。投递是至少一次的，重连补投、重试、
// 消费者被杀都会让同一条消息到达不止一次。
//
// 另一条同样要紧的约定：**能确定重来也不会成功的消息，应当返回 nil 而不是错误**。
// 报文解不开、字段缺失、引用的记录已经不存在——这些重投一万次也是同样的结果，
// 返回错误只会让它在重试队列和主队列之间兜圈子，直到进死信队列为止。
// 记一条日志然后返回 nil，才是正确处置。
type Handler func(ctx context.Context, message string) error

// Options 是客户端级别的默认值，可被单个队列的配置覆盖。
type Options struct {
	Logger *zap.Logger

	// TraceIDFromContext 取出 ctx 里的链路 id，发布时写进消息头；
	// WithTraceID 把消息头里的链路 id 还原进 ctx，消费时交给处理器。
	// 两者合起来让 api 进程里的一次 HTTP 请求与 worker 进程里真正干活的那段日志
	// 共用一个 trace_id。
	//
	// 做成钩子而不是直接调用，唯一的原因是**导入环**：这两件事的实现在
	// internal/helpers/logger，而 internal/helpers/logger -> config -> pkg/mq，
	// 本包直接导入它编译不过（不是风格取舍，是 go build 明确拒绝）。
	// 钩子签名刻意与那两个函数逐字一致，装配处写
	// opts.TraceIDFromContext = logger.TraceIDFromContext 即可，不需要包装函数。
	//
	// 不装配则整条链路静默退化成「不传 trace_id」：消息头不写、消费端不注入，
	// 投递、重试、死信语义一概不变。可观测性缺失不该让消息发不出去。
	TraceIDFromContext func(ctx context.Context) string
	WithTraceID        func(ctx context.Context, traceID string) context.Context

	// MaxRetries 是队列没有单独配置时的默认重投次数上限。
	MaxRetries int
	// RetryDelay 是队列没有单独配置时的默认重投间隔。
	RetryDelay time.Duration

	// ReconnectDelay 是断线后两次重连尝试之间的等待。
	ReconnectDelay time.Duration
	// PublishTimeout 是等待 broker 确认的上限。
	PublishTimeout time.Duration
}

func (o Options) normalized() Options {
	if o.Logger == nil {
		o.Logger = zap.NewNop()
	}
	if o.TraceIDFromContext == nil {
		o.TraceIDFromContext = func(context.Context) string { return "" }
	}
	if o.WithTraceID == nil {
		o.WithTraceID = func(ctx context.Context, _ string) context.Context { return ctx }
	}
	if o.MaxRetries <= 0 {
		o.MaxRetries = 5
	}
	if o.RetryDelay <= 0 {
		o.RetryDelay = 30 * time.Second
	}
	if o.ReconnectDelay <= 0 {
		o.ReconnectDelay = 5 * time.Second
	}
	if o.PublishTimeout <= 0 {
		o.PublishTimeout = 10 * time.Second
	}
	return o
}

// headerRetryCount 记录一条消息已经被重投了几次。
//
// 存在消息头里而不是消息体里：重试是传输层的事，不该逼着每一种业务报文
// 都在自己的结构里留一个和业务无关的字段。
const headerRetryCount = "x-retry-count"

// headerDeadReason 记录一条消息最终被判死的原因，写在死信消息头上。
// 死信队列里躺着一条没有任何上下文的报文，是排查时最无从下手的情况。
const headerDeadReason = "x-dead-reason"

// HeaderTraceID 携带触发这条消息的那次调用的链路 id。
//
// 跨进程的链路就断在这个头上：api 发布、worker 消费，没有它，
// 一次 HTTP 请求和它真正引发的那段后台处理在日志里是两件互不相干的事。
//
// 与同族的 x- 头不同，它是导出的——死信队列里的消息要靠它回溯来源，
// 翻 DLQ 的运维工具和跨进程的集成测试都得按名字读这个头。
const HeaderTraceID = "x-trace-id"

// LogFieldTraceID 是链路 id 在本包日志里的字段名。
//
// 它是 internal/helpers/logger.TraceIDField 的一份拷贝，两处必须同值。
// 本包因导入环引用不到那个常量（见 Options 上的说明），而字段名一旦分叉，
// worker 的日志就再也检索不进同一条链路——这正是它值得一条注释的原因。
const LogFieldTraceID = "trace_id"

// headerRetryScope 记录这条消息重投时只需要重跑哪几个处理器。
//
// ===========================================================================
// 它解决的问题
// ===========================================================================
//
// 一条消息可能被多个处理器消费（一个领域事件既要发通知又要生成报告）。
// 其中一个失败时，如果整条消息重投，**所有处理器都会重跑**——
// 已经成功的那些被迫再来一遍，靠幂等兜着。而幂等只是「不出错」，不是「没代价」：
// 重跑一次报告生成意味着又一次完整的数据库往返，重跑一次外部通知可能真的再发一遍。
//
// 有了这个头，失败的处理器把自己的名字写进来，重投时其余的直接跳过。
const headerRetryScope = "x-retry-scope"

// PartialFailure 让处理器告诉客户端：这条消息只有一部分没处理成功。
//
// 返回它（而不是一个普通 error）之后，重投的消息会带上 Scopes，
// 处理器可以据此只重跑失败的那部分。
//
// 返回普通 error 的语义不变：整条消息重投，全部重跑。
type PartialFailure struct {
	// Scopes 是需要重跑的处理器标识。空切片等价于普通错误（全部重跑）。
	Scopes []string
	Err    error
}

func (p *PartialFailure) Error() string {
	if p.Err == nil {
		return "mq: 部分处理器失败: " + strings.Join(p.Scopes, ",")
	}
	return p.Err.Error()
}

func (p *PartialFailure) Unwrap() error { return p.Err }

type retryScopeKey struct{}

// RetryScopesFrom 返回本次投递需要重跑的处理器范围。
//
// 返回空切片表示「全部」——首次投递，或上一次的失败没有给出范围。
// 处理器据此决定跳过哪些子处理器；拿不到范围时必须全部执行，
// 宁可重复（处理器本来就必须幂等）也不能漏掉。
func RetryScopesFrom(ctx context.Context) []string {
	scopes, _ := ctx.Value(retryScopeKey{}).([]string)
	return scopes
}

// WithRetryScopes 把重跑范围放进 context。
//
// 正常链路由本包在投递时调用。导出是给两类调用方用的：
// 想在没有 broker 的情况下测试「重投只重跑失败者」的单元测试，
// 以及将来若换掉消息中间件，新传输层需要能构造出同样的 ctx。
func WithRetryScopes(ctx context.Context, scopes []string) context.Context {
	if len(scopes) == 0 {
		return ctx
	}
	return context.WithValue(ctx, retryScopeKey{}, scopes)
}

// scopesOfHeader 解析消息头里的重跑范围。
// 解析不出来就当「全部」——宁可重复也不能漏。
func scopesOfHeader(headers map[string]any) []string {
	raw, ok := headers[headerRetryScope].(string)
	if !ok || raw == "" {
		return nil
	}
	var scopes []string
	if err := json.Unmarshal([]byte(raw), &scopes); err != nil {
		return nil
	}
	return scopes
}

// scopesOfError 从错误里取出重跑范围。不是 PartialFailure 就返回 nil（全部重跑）。
func scopesOfError(err error) []string {
	var partial *PartialFailure
	if errors.As(err, &partial) {
		return partial.Scopes
	}
	return nil
}

// AMQP 是一个可以被多个 goroutine 共用的客户端。
type AMQP struct {
	info ConnectionInfo
	opts Options

	// topoMu 保护拓扑。拓扑在 InitiateSubscriber 里写一次，
	// 之后被每个消费者 goroutine 与每次重连读取。
	topoMu    sync.RWMutex
	exchanges []Exchange
	queues    map[string]Queue

	// connMu 串行化「拿连接」这件事，保证 N 个消费者同时发现断线时只会重连一次。
	connMu sync.Mutex
	conn   *amqp.Connection
	pubCh  *amqp.Channel

	// ctx 是客户端的生命周期。Close 取消它，全部消费者循环随之退出。
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New 建一个客户端。此时还不会连接——连接发生在第一次声明拓扑或第一次发布时。
//
// 延迟连接是有意的：让一个只发消息的进程（比如只跑 HTTP 的 API 副本）
// 不会因为启动瞬间 broker 还没就绪而起不来。
func New(info ConnectionInfo, opts Options) *AMQP {
	ctx, cancel := context.WithCancel(context.Background())
	return &AMQP{
		info:   info.normalized(),
		opts:   opts.normalized(),
		queues: make(map[string]Queue),
		ctx:    ctx,
		cancel: cancel,
	}
}

// InitiateSubscriber 声明全部交换机与队列。
//
// 名字沿用团队既有服务的叫法，便于对照阅读；它做的事情是「把配置里的拓扑
// 建出来」，与要不要起消费者无关——只发消息的进程同样应该调用它，
// 否则第一条消息可能发往一个还不存在的交换机而被静默丢弃。
//
// 声明是幂等的：AMQP 的 declare 在参数一致时等价于一次存在性检查。
// 参数不一致时 broker 会报错，而这正是我们要的——它意味着有人改了队列参数
// 却没有迁移旧队列，静默接受只会让两个副本对同一个队列有两套理解。
func (a *AMQP) InitiateSubscriber(exchanges []Exchange, queues []Queue) error {
	a.topoMu.Lock()
	a.exchanges = exchanges
	a.queues = make(map[string]Queue, len(queues))
	for _, q := range queues {
		if q.Name == "" {
			a.topoMu.Unlock()
			return fmt.Errorf("mq: 队列声明缺少名字")
		}
		a.queues[q.Name] = q.normalized(a.opts)
	}
	a.topoMu.Unlock()

	ch, err := a.openChannel()
	if err != nil {
		return err
	}
	defer func() { _ = ch.Close() }()

	return a.declareTopology(ch)
}

// declareTopology 建出交换机、主队列、重试队列与死信队列。
//
// 重试队列没有消费者：消息在里面躺够 TTL，由 broker 通过死信路由自动送回主队列。
// 这是 RabbitMQ 里实现「延迟重投」最朴素的办法——不需要插件，也不需要在
// 应用里维护一个定时器，而定时器恰恰是最容易随进程一起丢掉的东西。
func (a *AMQP) declareTopology(ch *amqp.Channel) error {
	a.topoMu.RLock()
	exchanges := append([]Exchange(nil), a.exchanges...)
	queues := make([]Queue, 0, len(a.queues))
	for _, q := range a.queues {
		queues = append(queues, q)
	}
	a.topoMu.RUnlock()

	for _, e := range exchanges {
		if err := ch.ExchangeDeclare(e.Name, e.kind(), e.Durable, false, false, false, nil); err != nil {
			return fmt.Errorf("mq: 声明交换机 %s 失败: %w", e.Name, err)
		}
	}

	for _, q := range queues {
		if _, err := ch.QueueDeclare(q.Name, q.Durable, false, false, false, nil); err != nil {
			return fmt.Errorf("mq: 声明队列 %s 失败: %w", q.Name, err)
		}
		if q.Exchange != "" {
			if err := ch.QueueBind(q.Name, q.RoutingKey, q.Exchange, false, nil); err != nil {
				return fmt.Errorf("mq: 绑定队列 %s 到 %s 失败: %w", q.Name, q.Exchange, err)
			}
		}

		// 重试队列：躺满 TTL 之后按死信路由回到主队列。
		// 主队列若没有绑定（只靠默认交换机直投），死信就直接投回队列名。
		retryArgs := amqp.Table{"x-message-ttl": int32(q.RetryDelay.Milliseconds())}
		if q.Exchange != "" {
			retryArgs["x-dead-letter-exchange"] = q.Exchange
			retryArgs["x-dead-letter-routing-key"] = q.RoutingKey
		} else {
			retryArgs["x-dead-letter-exchange"] = ""
			retryArgs["x-dead-letter-routing-key"] = q.Name
		}
		if _, err := ch.QueueDeclare(q.RetryName(), q.Durable, false, false, false, retryArgs); err != nil {
			return fmt.Errorf("mq: 声明重试队列 %s 失败: %w", q.RetryName(), err)
		}

		// 死信队列是终点，没有 TTL 也没有死信路由：进来的消息就该一直躺着等人看。
		if _, err := ch.QueueDeclare(q.DLQName(), q.Durable, false, false, false, nil); err != nil {
			return fmt.Errorf("mq: 声明死信队列 %s 失败: %w", q.DLQName(), err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 连接
// ---------------------------------------------------------------------------

// connection 返回一个可用连接，必要时重新拨号并重建拓扑。
//
// 整个方法在 connMu 下串行：N 个消费者同时发现断线是常态，
// 不串行就会同时拨出 N 条连接，而其中 N-1 条会立刻变成泄漏。
func (a *AMQP) connection() (*amqp.Connection, error) {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	return a.connectionLocked()
}

func (a *AMQP) connectionLocked() (*amqp.Connection, error) {
	if a.conn != nil && !a.conn.IsClosed() {
		return a.conn, nil
	}
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}

	conn, err := amqp.Dial(a.info.URL())
	if err != nil {
		return nil, fmt.Errorf("mq: 连接 %s 失败: %w", a.info.SafeURL(), err)
	}
	a.conn = conn
	// 发布信道跟着连接一起作废，这里一并清掉，下次发布时重建。
	a.pubCh = nil

	a.opts.Logger.Info("已连接消息队列", zap.String("broker", a.info.SafeURL()))

	// 新连接上必须重新声明拓扑：断线可能是 broker 重启，
	// 而一个刚重启的 broker 上什么都没有（非持久化部署尤其如此）。
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("mq: 打开信道失败: %w", err)
	}
	defer func() { _ = ch.Close() }()
	if err := a.declareTopology(ch); err != nil {
		return nil, err
	}
	return conn, nil
}

func (a *AMQP) openChannel() (*amqp.Channel, error) {
	conn, err := a.connection()
	if err != nil {
		return nil, err
	}
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("mq: 打开信道失败: %w", err)
	}
	return ch, nil
}

// publishChannel 返回开启了发布确认的共享信道。
func (a *AMQP) publishChannel() (*amqp.Channel, error) {
	a.connMu.Lock()
	defer a.connMu.Unlock()

	if a.pubCh != nil && !a.pubCh.IsClosed() {
		return a.pubCh, nil
	}
	conn, err := a.connectionLocked()
	if err != nil {
		return nil, err
	}
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("mq: 打开发布信道失败: %w", err)
	}
	// 开启发布确认：没有它，Publish 只代表「写进了本地 socket 缓冲」，
	// broker 崩溃时那条消息就凭空消失了，而调用方以为自己发成功了。
	// 本服务用消息驱动定时任务，「以为发出去了其实没有」等于一次触发被吞掉。
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return nil, fmt.Errorf("mq: 开启发布确认失败: %w", err)
	}
	a.pubCh = ch
	return ch, nil
}

// ---------------------------------------------------------------------------
// 发布
// ---------------------------------------------------------------------------

// PublishDirectMessageWithContext 往指定交换机发一条消息，并等待 broker 确认。
//
// exchange 传空串表示走默认交换机，此时 routingKey 就是队列名（直投）。
func (a *AMQP) PublishDirectMessageWithContext(ctx context.Context, exchange, routingKey, body string) error {
	return a.publish(ctx, exchange, routingKey, body, nil)
}

// PublishDirectMessageJsonWithContext 把任意值序列化成 JSON 再发出去。
func (a *AMQP) PublishDirectMessageJsonWithContext(ctx context.Context, exchange, routingKey string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("mq: 序列化消息失败: %w", err)
	}
	return a.publish(ctx, exchange, routingKey, string(b), nil)
}

// PublishDirectMessagesWithContext 往同一个路由键上发一批消息，并等待全部确认。
//
// # 为什么需要它
//
// 循环调用单条发布会把一批 N 条消息变成 N 次「发一条、等一条确认」的往返。
// 确认是一次真实的 broker round-trip，批量提交动辄几十上百条，串起来就是
// 几十上百倍的延迟——而调用方通常正握着数据库连接等这批消息发完。
//
// 这里的做法是把 N 次发布一次性压给信道，再统一收 N 个确认：往返只剩一次的量级，
// 而「每条都被 broker 确认过」这个保证一点没少。
//
// # 部分失败的语义
//
// 中途失败时已经发出去的消息**不会被撤回**——AMQP 没有事务性批量（真有事务模式，
// 但它比逐条确认还慢，且这里不需要全有或全无）。调用方必须能接受「发了一部分」，
// 靠各自的补偿机制收口，而不是假设本方法是原子的。
func (a *AMQP) PublishDirectMessagesWithContext(ctx context.Context, exchange, routingKey string, bodies []string) error {
	if len(bodies) == 0 {
		return nil
	}
	ch, err := a.publishChannel()
	if err != nil {
		return err
	}

	headers := a.headersWithTraceID(ctx, nil)
	// 整批共用一个超时预算。批次越大越可能撞上它，这是有意的：
	// 一批消息发了半分钟还没确认完，调用方需要的是尽快拿到错误去补偿，
	// 而不是继续等下去。
	waitCtx, cancel := context.WithTimeout(ctx, a.opts.PublishTimeout)
	defer cancel()

	now := time.Now()
	confirms := make([]*amqp.DeferredConfirmation, 0, len(bodies))
	for _, body := range bodies {
		conf, err := ch.PublishWithDeferredConfirmWithContext(waitCtx, exchange, routingKey, false, false, amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Timestamp:    now,
			Headers:      headers,
			Body:         []byte(body),
		})
		if err != nil {
			a.dropPublishChannel(ch)
			return fmt.Errorf("mq: 批量发布到 %s/%s 失败（已发出 %d/%d 条）: %w",
				exchange, routingKey, len(confirms), len(bodies), err)
		}
		confirms = append(confirms, conf)
	}

	for i, conf := range confirms {
		acked, err := conf.WaitContext(waitCtx)
		if err != nil {
			return fmt.Errorf("mq: 等待 %s/%s 第 %d/%d 条的发布确认失败: %w",
				exchange, routingKey, i+1, len(bodies), err)
		}
		if !acked {
			return fmt.Errorf("mq: broker 拒绝了发往 %s/%s 的第 %d/%d 条消息",
				exchange, routingKey, i+1, len(bodies))
		}
	}
	return nil
}

func (a *AMQP) publish(ctx context.Context, exchange, routingKey, body string, headers amqp.Table) error {
	ch, err := a.publishChannel()
	if err != nil {
		return err
	}

	headers = a.headersWithTraceID(ctx, headers)

	// 确认等待要有上限：broker 半死不活（接受了连接但不回确认）时，
	// 没有上限就意味着调用方永远挂在这里，而调用方可能正握着一个数据库连接。
	waitCtx, cancel := context.WithTimeout(ctx, a.opts.PublishTimeout)
	defer cancel()

	conf, err := ch.PublishWithDeferredConfirmWithContext(waitCtx, exchange, routingKey, false, false, amqp.Publishing{
		ContentType: "application/json",
		// Persistent 让消息落盘。队列声明成 durable 却发非持久化消息，
		// 是这类系统最常见的一个假象：队列在，消息却随 broker 重启一起没了。
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
		Headers:      headers,
		Body:         []byte(body),
	})
	if err != nil {
		// 信道可能已经坏了，丢掉它，下次发布重建。
		a.dropPublishChannel(ch)
		return fmt.Errorf("mq: 发布到 %s/%s 失败: %w", exchange, routingKey, err)
	}

	acked, err := conf.WaitContext(waitCtx)
	if err != nil {
		return fmt.Errorf("mq: 等待 %s/%s 的发布确认失败: %w", exchange, routingKey, err)
	}
	if !acked {
		return fmt.Errorf("mq: broker 拒绝了发往 %s/%s 的消息", exchange, routingKey)
	}
	return nil
}

// headersWithTraceID 在必要时给消息头补上链路 id，返回要用的那份头。
//
// 空串不写：worker 里由定时任务发起的消息本来就没有上游链路，
// 补一个空值或编一个假 id，比让这个头缺席更糟——前者会让检索
// 捞出一堆互不相干的消息，后者会伪造出一条根本不存在的链路。
//
// 已有值不覆盖：转投（重试与死信）沿用的是原消息头里抄过来的那个 id，
// 而转投走的是客户端生命周期 ctx，本来就取不到 id。这条判断是防御性的，
// 它保证将来若有人给转投换了个带 ctx 的调用方，也不会把原始链路改写掉。
func (a *AMQP) headersWithTraceID(ctx context.Context, headers amqp.Table) amqp.Table {
	if _, exists := headers[HeaderTraceID]; exists {
		return headers
	}
	traceID := a.opts.TraceIDFromContext(ctx)
	if traceID == "" {
		return headers
	}
	if headers == nil {
		headers = amqp.Table{}
	}
	headers[HeaderTraceID] = traceID
	return headers
}

func (a *AMQP) dropPublishChannel(ch *amqp.Channel) {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	if a.pubCh == ch {
		a.pubCh = nil
	}
	_ = ch.Close()
}

// ---------------------------------------------------------------------------
// 订阅
// ---------------------------------------------------------------------------

// SubscribeSameQueueMultipleWithContext 为一个队列拉起消费者。
//
// 名字沿用团队既有服务的叫法。"Multiple" 指的是同一个队列上可以有多个并发消费者，
// 数量由队列配置里的 Consumers 决定。
//
// 本方法立刻返回，消费在后台进行，直到 Close 被调用。
// 队列必须已经在 InitiateSubscriber 里声明过——订阅一个没声明的队列是配置错误，
// 这里直接报错而不是默默建一个默认参数的队列：后者会让拼错的队列名
// 变成一个「消费者活着但永远收不到消息」的幽灵。
func (a *AMQP) SubscribeSameQueueMultipleWithContext(h Handler, queueName string) error {
	a.topoMu.RLock()
	q, ok := a.queues[queueName]
	a.topoMu.RUnlock()
	if !ok {
		return fmt.Errorf("mq: 队列 %s 未在配置中声明，无法订阅", queueName)
	}
	if h == nil {
		return fmt.Errorf("mq: 队列 %s 的处理器为空", queueName)
	}

	for i := 0; i < q.Consumers; i++ {
		a.wg.Add(1)
		go func(idx int) {
			defer a.wg.Done()
			a.consumeLoop(q, h, idx)
		}(i)
	}
	a.opts.Logger.Info("已订阅队列",
		zap.String("queue", q.Name),
		zap.Int("consumers", q.Consumers),
		zap.Int("prefetch", q.Prefetch),
		zap.Int("max_retries", q.MaxRetries),
		zap.Duration("retry_delay", q.RetryDelay))
	return nil
}

// consumeLoop 是一个消费者的完整生命周期：连上、消费到断开、退避、再来。
//
// 断线重连写在这里而不是交给调用方，是因为「消费者断了」没有任何业务上的处置方式——
// 唯一正确的反应就是重连。把它暴露出去只会让每个调用方各写一份一模一样的重连循环，
// 而其中总有一份会忘记退避，在 broker 重启期间把它打得起不来。
func (a *AMQP) consumeLoop(q Queue, h Handler, idx int) {
	for {
		if a.ctx.Err() != nil {
			return
		}

		ch, deliveries, err := a.openConsumer(q, idx)
		if err != nil {
			if a.ctx.Err() != nil {
				return
			}
			a.opts.Logger.Warn("订阅队列失败，稍后重试",
				zap.String("queue", q.Name), zap.Error(err))
			if !a.sleep(a.opts.ReconnectDelay) {
				return
			}
			continue
		}

		for d := range deliveries {
			a.handleDelivery(q, h, d)
		}
		_ = ch.Close()

		if a.ctx.Err() != nil {
			return
		}
		// deliveries 被关闭意味着信道或连接断了。退避之后重来。
		a.opts.Logger.Warn("消息队列连接中断，准备重连", zap.String("queue", q.Name))
		if !a.sleep(a.opts.ReconnectDelay) {
			return
		}
	}
}

func (a *AMQP) openConsumer(q Queue, idx int) (*amqp.Channel, <-chan amqp.Delivery, error) {
	ch, err := a.openChannel()
	if err != nil {
		return nil, nil, err
	}
	// 预取上限作用在单个消费者上（global=false）：这里要限的是
	// 「一个消费者手上同时压着几条」，不是「一条连接上一共压着几条」。
	if err := ch.Qos(q.Prefetch, 0, false); err != nil {
		_ = ch.Close()
		return nil, nil, fmt.Errorf("mq: 设置队列 %s 的预取失败: %w", q.Name, err)
	}
	tag := fmt.Sprintf("%s-%d", q.Name, idx)
	deliveries, err := ch.ConsumeWithContext(a.ctx, q.Name, tag, false, false, false, false, nil)
	if err != nil {
		_ = ch.Close()
		return nil, nil, fmt.Errorf("mq: 消费队列 %s 失败: %w", q.Name, err)
	}
	return ch, deliveries, nil
}

// handleDelivery 处理一条消息并决定它的去向。
//
// 三条出路，没有第四条：
//
//	成功           确认，消息消失
//	失败且还有次数  投进重试队列（延迟后自动回到主队列），确认原消息
//	失败且次数用尽  投进死信队列，确认原消息
//
// 注意**两条失败路径都确认了原消息**。用 Nack+requeue 会让消息立刻回到队首
// 再次被同一个消费者取到，形成一个毫秒级的死循环，能在几秒内打满 CPU 和日志。
// 只有在「连重投都失败了」的情况下才退回 Nack——那时消息已经无处可去，
// 让它留在队列里等下一次投递是唯一不丢消息的选择。
func (a *AMQP) handleDelivery(q Queue, h Handler, d amqp.Delivery) {
	// 把上一次失败留下的重跑范围交给处理器，它据此决定跳过哪些子处理器。
	ctx := WithRetryScopes(a.ctx, scopesOfHeader(d.Headers))

	// 在这里接回上游链路：此后处理器做的每一件事——领域服务、仓储、
	// 直到 GORM 打出来的那条 SQL——都落在发起方那次 HTTP 请求的同一个 trace_id 下。
	// 注入点必须在 ctx 交给处理器之前，且只此一处。
	traceID := traceIDOf(d.Headers)
	ctx = a.opts.WithTraceID(ctx, traceID)

	// 本包自己的日志走 a.opts.Logger（它不是从 ctx 里取的），
	// 所以链路字段得手动挂上去，否则同一条消息的处理器日志有 trace_id、
	// 而记录它重试和判死的日志没有——恰恰是排查时最需要的那两条。
	dlog := a.opts.Logger.With(zap.String("queue", q.Name))
	if traceID != "" {
		dlog = dlog.With(zap.String(LogFieldTraceID, traceID))
	}

	err := invoke(ctx, h, string(d.Body))
	if err == nil {
		if ackErr := d.Ack(false); ackErr != nil {
			dlog.Warn("确认消息失败", zap.Error(ackErr))
		}
		return
	}

	attempt := retryCountOf(d.Headers)
	scopes := scopesOfError(err)
	log := dlog.With(
		zap.Int("attempt", attempt),
		zap.Int("max_retries", q.MaxRetries),
		zap.Strings("retry_scope", scopes),
		zap.Error(err))

	if attempt >= q.MaxRetries {
		log.Error("消息重试次数已用尽，转入死信队列")
		a.route(d, q.DLQName(), attempt, err.Error(), scopes, dlog)
		return
	}

	log.Warn("消息处理失败，延迟重投")
	a.route(d, q.RetryName(), attempt+1, err.Error(), scopes, dlog)
}

// route 把一条消息转投到重试队列或死信队列，然后确认原消息。
//
// scopes 为空时会**清掉**消息头里的重跑范围而不是保留上一次的：
// 一个没有给出范围的失败意味着「整条都得重来」，留着旧范围会让重投
// 只跑上一轮那几个处理器，而真正该重跑的那个被永久跳过。
//
// 整份消息头被原样抄过来，链路 id 因此自动跟着走：一条失败三次最终躺进
// 死信队列的消息，头上仍然是最初那次 HTTP 请求的 trace_id。
// 这是刻意依赖的行为——别把下面的抄写改成只挑几个头复制。
//
// log 由调用方带上 queue 与链路字段传进来，省得在这里重新拼一遍。
func (a *AMQP) route(d amqp.Delivery, target string, attempt int, reason string, scopes []string, log *zap.Logger) {
	headers := amqp.Table{}
	for k, v := range d.Headers {
		headers[k] = v
	}
	headers[headerRetryCount] = int32(attempt)
	headers[headerDeadReason] = truncateReason(reason)

	if len(scopes) == 0 {
		delete(headers, headerRetryScope)
	} else if encoded, err := json.Marshal(scopes); err == nil {
		headers[headerRetryScope] = string(encoded)
	} else {
		// 编码不出来就退回「全部重跑」，绝不留一个半对的范围。
		delete(headers, headerRetryScope)
	}

	// 转投不受消费循环的生命周期约束，但也不能无限期阻塞，
	// publish 自身已经带了确认超时。
	if err := a.publish(a.ctx, "", target, string(d.Body), headers); err != nil {
		log.Error("转投消息失败，退回队列等待再次投递",
			zap.String("target", target), zap.Error(err))
		// 转投失败是唯一该 requeue 的场合：消息此刻无处可去，
		// 退回队列至少保证它不会凭空消失。
		if nackErr := d.Nack(false, true); nackErr != nil {
			log.Error("退回消息失败，该消息已丢失", zap.Error(nackErr))
		}
		return
	}
	if err := d.Ack(false); err != nil {
		// 转投已经成功，原消息没确认上会导致它被重投一次。
		// 处理器是幂等的，所以这是可接受的重复，但值得留一条日志。
		log.Warn("转投成功但确认原消息失败，可能产生一次重复投递", zap.Error(err))
	}
}

// invoke 调用处理器并把 panic 转成错误。
//
// 一个处理器 panic 不该带走整个消费循环：那会让这个队列上的**全部**消息
// 停止处理，一条坏消息的影响被放大成一条链路的全面停摆。
// 转成错误之后，它走的是和普通失败完全一样的重试与死信路径。
func invoke(ctx context.Context, h Handler, message string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("mq: 处理器 panic: %v", r)
		}
	}()
	return h(ctx, message)
}

// retryCountOf 从消息头里读出已重投次数。
//
// AMQP 的 Table 在网络上是弱类型的，同一个整数可能以 int32 / int64 / int
// 中的任意一种回来（取决于发送方的客户端实现），所以这里必须逐一认。
// 认不出来就当 0：把一条来源不明的消息当成「还没重试过」，最坏是多重试几次，
// 而当成「已经重试很多次」会让它被直接判死。
func retryCountOf(headers map[string]any) int {
	v, ok := headers[headerRetryCount]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int32:
		return int(n)
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// traceIDOf 从消息头里读出链路 id，没有则返回空串。
//
// 和 retryCountOf 一样得防着弱类型的 Table：amqp091 把 longstr 解成 string，
// 但换一个客户端发来的消息可能是 []byte。认不出来就当没有——
// 丢一条链路只是查起来麻烦，而把一段二进制垃圾当成 id 塞进日志，
// 会让检索结果里多出一条看着像真的、其实对不上任何请求的链路。
func traceIDOf(headers map[string]any) string {
	switch v := headers[HeaderTraceID].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

// maxReasonLen 是写进消息头的失败原因长度上限。
// 消息头会跟着消息在 broker 里存一份，塞一整个堆栈进去既占空间也没人会读。
const maxReasonLen = 512

func truncateReason(s string) string {
	if len(s) <= maxReasonLen {
		return s
	}
	cut := maxReasonLen
	// 按 UTF-8 起始字节回退，避免把一个汉字切成半个。
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}

// sleep 等待 d，期间若客户端被关闭则提前返回 false。
func (a *AMQP) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-a.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Close 停掉全部消费者并断开连接。
//
// 先取消 ctx 再关连接：关连接会让全部 deliveries 通道关闭，消费循环因此退出，
// 而 ctx 已经取消保证了它们不会转头又去重连。顺序反过来会让每个消费者
// 在停机瞬间各发起一次重连尝试。
func (a *AMQP) Close() error {
	a.cancel()

	a.connMu.Lock()
	conn := a.conn
	a.conn = nil
	a.pubCh = nil
	a.connMu.Unlock()

	var err error
	if conn != nil && !conn.IsClosed() {
		if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, amqp.ErrClosed) {
			err = closeErr
		}
	}
	a.wg.Wait()
	return err
}
