package domain_event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/pkg/mq"
)

// MessagePublisher 是本包所需的窄能力，由消费方声明。
//
// 声明成接口而不是直接吃 *mq.AMQP：领域内核不该在编译期依赖某一个 broker 客户端。
// 这也让「发布了什么信封」这件事可以在没有 broker 的情况下被测试覆盖。
type MessagePublisher interface {
	PublishDirectMessageWithContext(ctx context.Context, exchange, routingKey, body string) error
}

// envelope 是领域事件在消息队列里的外壳。
//
// name / eventId / occurredAt 在信封上又出现了一份（payload 里本来就有）。
// 这份冗余是故意的：运维在 broker 的管理界面上翻一条积压消息时，
// 看到的应该是「什么事件、什么时候发生的」，而不是一坨需要先知道类型才能读懂的 JSON。
type envelope struct {
	Name       string          `json:"name"`
	EventID    string          `json:"eventId"`
	OccurredAt time.Time       `json:"occurredAt"`
	Payload    json.RawMessage `json:"payload"`
}

// subscriber 是一个已登记的处理器，连同它的名字。
//
// 名字用来做**失败隔离**：一条事件有多个处理器时，只有失败的那些会在重投时重跑。
// 它由 handlerName 从函数本身推导，调用点不必额外写一遍——一个需要手写的名字
// 迟早会出现两个处理器重名，而重名的后果是其中一个永远不会被重试。
type subscriber struct {
	name   string
	handle Handler
}

// registration 是一个事件名对应的类型与处理器们。
type registration struct {
	// eventType 是具体事件结构体的类型（已经剥掉指针）。
	// 收到消息时用它 new 一个实例再反序列化，处理器因此拿到的是强类型的事件，
	// 而不是一个需要自己 json.Unmarshal 的字符串。
	eventType   reflect.Type
	subscribers []subscriber
}

// handlerName 从处理器函数推导一个稳定的名字。
//
// 处理器都是方法值（`s.OnTaskCompleted`），运行时能拿到形如
// `pkg/path.(*NotificationSubscriber).OnTaskCompleted-fm` 的全名。
// 这里砍掉包路径与 Go 给方法值加的 `-fm` 后缀，留下
// `(*NotificationSubscriber).OnTaskCompleted`——足够短、足够可读、
// 且只在类型名或方法名真的改了时才会变。
//
// 名字变了会怎样：重投消息里的旧名字匹配不上任何处理器，此时**全部重跑**
// （见 Dispatch）。那是一次多余的重复执行，不是数据丢失——处理器本来就必须幂等。
// 这个降级方向是刻意选的：宁可重复，也不能因为一次改名让某个处理器被永久跳过。
func handlerName(h Handler) string {
	full := runtime.FuncForPC(reflect.ValueOf(h).Pointer()).Name()
	if idx := strings.LastIndex(full, "/"); idx >= 0 {
		full = full[idx+1:]
	}
	// 包名后面第一个点之后才是类型与方法，去掉包名让名字更短。
	if idx := strings.Index(full, "."); idx >= 0 {
		full = full[idx+1:]
	}
	return strings.TrimSuffix(full, "-fm")
}

// AmqpBus 把领域事件发到消息队列上，并在消费端还原成具体事件类型分发给处理器。
//
// ===========================================================================
// 为什么不再用进程内总线
// ===========================================================================
//
// 进程内分发有三个问题，它们都不是「以后再说」的问题：
//
//  1. 处理器的失败无处可去。同步分发时，一个失败的通知只能被记进日志然后丢掉——
//     业务操作已经落库了，回滚它是荒谬的，而重试又没有地方存放「待重试」这个状态。
//  2. 发布方与消费方被绑在同一个进程里。HTTP 副本发出的事件只能由 HTTP 副本自己
//     消费，于是一次登录事件的通知写入，占用的是用户请求的那条线程。
//  3. 进程重启即丢失。同步分发没有任何持久化，滚动发布期间发出的事件就是丢了。
//
// 走消息队列之后，这三件事分别由持久化队列、独立消费进程、有界重试 + 死信队列解决。
//
// # 代价：投递语义变成至少一次
//
// 处理器必须幂等。这不是新增的要求——进程内总线同样会因为业务侧的重试而重复投递，
// 只是此前没有任何机制逼着人正视它。现在重复是常态：重投、重连补投、
// 消费者被杀都会让同一个事件到达不止一次。
//
// # 代价：处理器之间不再共享事务，也不保证顺序
//
// 本来也没有。同步分发给人的「顺序有保证」是一种错觉——它只在单进程、
// 单发布点的前提下成立，而那两个前提从来没有被写下来过。
type AmqpBus struct {
	publisher  MessagePublisher
	exchange   string
	routingKey string
	log        *zap.Logger

	// mu 保护注册表。注册发生在启动期，分发发生在运行期，
	// 但「启动期一定早于运行期」是个假设而不是保证——加一把读写锁把它变成保证，
	// 代价是每条消息一次无争用的读锁。
	mu       sync.RWMutex
	registry map[string]*registration
}

var _ Publisher = (*AmqpBus)(nil)

func NewAmqpBus(publisher MessagePublisher, exchange, routingKey string, log *zap.Logger) *AmqpBus {
	if log == nil {
		log = zap.NewNop()
	}
	return &AmqpBus{
		publisher:  publisher,
		exchange:   exchange,
		routingKey: routingKey,
		log:        log,
		registry:   make(map[string]*registration),
	}
}

// RegisterSubscriber 登记一个处理器，并通过样例事件告诉总线该事件的具体类型。
//
// 签名与团队既有服务保持一致：`RegisterSubscriber(handler, &SomeEvent{})`。
// 传样例而不是传一个构造函数，是因为调用点因此只需要写出类型名——
// 而类型名恰好是读者最需要看到的东西。
//
// 同一个事件可以有多个处理器，按注册顺序依次执行。
func (b *AmqpBus) RegisterSubscriber(h Handler, prototype DomainEvent) {
	if h == nil || prototype == nil {
		return
	}
	t := reflect.TypeOf(prototype)
	if t.Kind() != reflect.Ptr || t.Elem().Kind() != reflect.Struct {
		// 样例必须是指向结构体的指针，否则无法在消费端 new 出实例。
		// 这是编码错误而不是运行期异常，在启动日志里喊出来即可。
		b.log.Error("领域事件样例必须是指向结构体的指针，本次注册被忽略",
			zap.String("type", t.String()))
		return
	}

	name := prototype.Name()
	sub := subscriber{name: handlerName(h), handle: h}

	b.mu.Lock()
	defer b.mu.Unlock()

	reg, ok := b.registry[name]
	if !ok {
		b.registry[name] = &registration{eventType: t.Elem(), subscribers: []subscriber{sub}}
		return
	}
	if reg.eventType != t.Elem() {
		// 两个不同的结构体自称同一个事件名。消费端只能挑一个来反序列化，
		// 另一个的处理器会拿到一个字段全是零值的事件——静默接受等于埋一颗雷。
		b.log.Error("同一事件名注册了不同的事件类型，本次注册被忽略",
			zap.String("event", name),
			zap.String("registered", reg.eventType.String()),
			zap.String("incoming", t.Elem().String()))
		return
	}
	for _, existing := range reg.subscribers {
		if existing.name == sub.name {
			// 同名处理器会让失败隔离失效：重投时两个都会被匹配上，
			// 于是成功的那个也跟着重跑，而这正是隔离要避免的。
			b.log.Error("同一事件注册了同名处理器，本次注册被忽略",
				zap.String("event", name), zap.String("handler", sub.name))
			return
		}
	}
	reg.subscribers = append(reg.subscribers, sub)
}

// Publish 把事件逐条发到队列上。
//
// 逐条而不是打包成一条消息：打包会让其中一个事件的处理失败拖着其余事件一起重投，
// 而这些事件之间本来没有任何关系（一次执行可能同时产生「任务失败」和「任务被熔断」）。
//
// 中途失败会返回错误，但**已经发出去的不会撤回**——这正是想要的：
// 领域决策已经落库，能送出去几条是几条，撤回只会让更多消费者收不到消息。
// 调用方拿到错误后该做的是记日志告警，而不是回滚业务。
func (b *AmqpBus) Publish(ctx context.Context, events ...DomainEvent) error {
	var errs []error
	for _, e := range events {
		if e == nil {
			continue
		}
		if err := b.publishOne(ctx, e); err != nil {
			b.log.Error("领域事件发布失败",
				zap.String("event", e.Name()),
				zap.String("event_id", e.EventID()),
				zap.Error(err))
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (b *AmqpBus) publishOne(ctx context.Context, e DomainEvent) error {
	payload, err := e.ToJson()
	if err != nil {
		return fmt.Errorf("序列化领域事件 %s 失败: %w", e.Name(), err)
	}
	body, err := json.Marshal(envelope{
		Name:       e.Name(),
		EventID:    e.EventID(),
		OccurredAt: e.OccurredAt(),
		Payload:    json.RawMessage(payload),
	})
	if err != nil {
		return fmt.Errorf("封装领域事件 %s 失败: %w", e.Name(), err)
	}
	return b.publisher.PublishDirectMessageWithContext(ctx, b.exchange, b.routingKey, string(body))
}

// Dispatch 是挂到消息队列上的处理器：把一条消息还原成事件并交给处理器们。
//
// # 什么时候返回 nil（确认并丢弃），什么时候返回错误（触发重投）
//
// 判据只有一句：**重来一次有没有可能成功**。
//
//	报文解不开            重来一万次也是同样的结果  -> mq.ErrPoison（进死信）
//	本进程没注册这个事件   重来还是没注册            -> nil（确认丢弃）
//	处理器返回错误         下游可能只是临时不可用    -> 错误（重投）
//
// 前两类若返回普通错误，消息会在重试队列和主队列之间兜圈子直到进死信队列，
// 除了制造噪音没有任何作用——所以它们都不重投。但「不重投」有两种收场，
// 这两类分别落在不同的一种上：
//
//   - 解不开的报文进**死信队列**。它多半来自滚动发布期间新旧事件结构不兼容，
//     是最需要留下原件、事后能重放的情况；直接确认等于把它删了。
//   - 没订阅的事件**确认丢弃**。它不是异常而是常态：全部领域事件共用一个队列，
//     任何一个消费进程都只关心其中一部分，把别人的事件塞进我的死信队列纯属噪音。
func (b *AmqpBus) Dispatch(ctx context.Context, message string) error {
	var env envelope
	if err := json.Unmarshal([]byte(message), &env); err != nil {
		b.log.Error("领域事件报文无法解析，转入死信队列", zap.Error(err), zap.String("message", message))
		return fmt.Errorf("领域事件报文无法解析: %v: %w", err, mq.ErrPoison)
	}

	b.mu.RLock()
	reg, ok := b.registry[env.Name]
	var subs []subscriber
	var eventType reflect.Type
	if ok {
		subs = append(subs, reg.subscribers...)
		eventType = reg.eventType
	}
	b.mu.RUnlock()

	if !ok || len(subs) == 0 {
		b.log.Debug("本进程未订阅该领域事件，已确认并丢弃", zap.String("event", env.Name))
		return nil
	}

	event, err := decodeEvent(eventType, env.Payload)
	if err != nil {
		// 这里是订阅了、但 payload 对不上注册的类型——版本不兼容的典型表现，
		// 比解不开外层信封更值得留证据。
		b.log.Error("领域事件无法还原成具体类型，转入死信队列",
			zap.String("event", env.Name), zap.Error(err))
		return fmt.Errorf("领域事件 %s 无法还原: %v: %w", env.Name, err, mq.ErrPoison)
	}

	return b.notify(ctx, env, subs, event)
}

// notify 把事件交给处理器们，并把失败的那几个报给传输层去单独重投。
//
// ===========================================================================
// 失败隔离：一个处理器失败，不该拖着其他处理器重跑
// ===========================================================================
//
// 一条事件常有多个处理器（`task_completed` 既要发通知又要生成报告）。
// 朴素做法是「有任何一个失败就整条消息重投」，于是已经成功的那些被迫再来一遍。
// 靠幂等能兜住正确性，但幂等只是「不出错」，不是「没代价」——
// 重跑一次报告生成是一整轮数据库往返，而它本来就已经成功了。
//
// 这里的做法：**全部跑完，只把失败的那几个的名字报上去**。
// 传输层把名字写进消息头，重投回来时 RetryScopesFrom 给出这批名字，
// 本方法据此跳过已经成功的。
//
// # 与团队既有服务的差别，以及为什么不照抄
//
// 既有服务的做法是「遇到第一个失败就 return，并只记下那一个处理器的名字」。
// 这样做有一个洞：**排在失败者后面的处理器在这一轮根本没跑**，
// 而重投时它们又因为名字对不上被跳过——于是它们一次都不会执行，
// 而且没有任何错误信号。
//
// 所以这里不 return，而是跑完全部、收集全部失败者。代价是一轮里可能多几次
// 无谓的尝试（下游整体故障时几个处理器会一起失败），换来的是没有任何处理器
// 会被静默跳过。
func (b *AmqpBus) notify(ctx context.Context, env envelope, subs []subscriber, event DomainEvent) error {
	scopes := retryScopesOf(ctx)

	// 范围里一个都匹配不上，说明处理器改过名（或换了实现）。
	// 此时退回「全部执行」：宁可重复，也不能让某个处理器因为一次改名被永久跳过。
	if len(scopes) > 0 && !anyMatch(subs, scopes) {
		b.log.Warn("重投范围与已注册处理器对不上，本轮执行全部处理器",
			zap.String("event", env.Name), zap.Strings("scope", scopes))
		scopes = nil
	}

	var failed []string
	var errs []error
	for _, sub := range subs {
		if len(scopes) > 0 && !contains(scopes, sub.name) {
			// 上一轮已经成功，跳过。
			continue
		}
		if err := b.invoke(ctx, sub, event); err != nil {
			b.log.Warn("领域事件处理失败",
				zap.String("event", env.Name),
				zap.String("event_id", env.EventID),
				zap.String("handler", sub.name),
				zap.Error(err))
			failed = append(failed, sub.name)
			errs = append(errs, fmt.Errorf("%s: %w", sub.name, err))
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return &mq.PartialFailure{Scopes: failed, Err: errors.Join(errs...)}
}

// invoke 调用一个处理器并把 panic 转成错误。
//
// 一个处理器 panic 不该带走同一条事件的其余处理器：那会让一个与它们无关的
// 编码错误，变成「这条事件的所有副作用都没发生」。
func (b *AmqpBus) invoke(ctx context.Context, sub subscriber, event DomainEvent) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("处理器 %s panic: %v", sub.name, r)
		}
	}()
	return sub.handle(ctx, event)
}

// retryScopesOf 取出本次投递的重跑范围。
//
// 单独包一层是为了让 domain_kernel 对传输层的依赖只有这一处，
// 换掉消息中间件时只需要改这个函数。
func retryScopesOf(ctx context.Context) []string { return mq.RetryScopesFrom(ctx) }

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func anyMatch(subs []subscriber, scopes []string) bool {
	for _, sub := range subs {
		if contains(scopes, sub.name) {
			return true
		}
	}
	return false
}

// decodeEvent 按注册时登记的类型 new 一个实例并反序列化。
func decodeEvent(t reflect.Type, payload []byte) (DomainEvent, error) {
	instance := reflect.New(t).Interface()
	if err := json.Unmarshal(payload, instance); err != nil {
		return nil, err
	}
	event, ok := instance.(DomainEvent)
	if !ok {
		return nil, fmt.Errorf("%s 未实现 DomainEvent", t.String())
	}
	return event, nil
}

// SubscribedEvents 返回本进程订阅的全部事件名，供启动日志使用。
//
// 这条日志的价值在停机排查时才显现：「为什么报告没生成」的第一个问题永远是
// 「这个进程到底订了这个事件没有」，而它应该在启动日志里就能回答。
func (b *AmqpBus) SubscribedEvents() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.registry))
	for name := range b.registry {
		out = append(out, name)
	}
	return out
}
