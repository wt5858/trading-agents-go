// Package providers 存放 Wire 的 provider 函数与 provider set。
//
// 分工与 injectors 包的关系：
//
//	providers/   「怎么造一个 X」—— 每个函数造一样东西，按限界上下文分文件
//	injectors/   「我要一个 Y」   —— 每个入口点列出它需要哪些 set，其余交给 Wire
//
// ===========================================================================
// 为什么换成 Wire
// ===========================================================================
//
// 手写的组装根有一个会随规模恶化的毛病：依赖顺序是**隐式**的。
// 「配置中心必须在 agent 之前装配，否则 LLM 路由读不到库里的供应商」这种约束，
// 在手写版本里只体现为 Build 函数里两行代码的先后，以及一句注释。
// 谁调整了顺序，编译照样通过，故障要等到运行时才出现。
//
// 换成 Wire 之后，这类约束由**函数签名**表达：NewLLMRouter 的参数里有
// *ProviderResolver，Wire 就必然先造它。顺序不再是一个需要人去维护的事实。
//
// 另外两个好处：入口点各取所需（HTTP 进程不会因为装配而连上它用不到的东西），
// 以及「这个服务到底由哪些东西组成」变成一份可以被工具检查的声明，而不是一段流程。
package providers

import (
	"context"
	"net/http"
	"sync"

	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/mongo"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/config"
	"github.com/wt5858/trading-agents-go/internal/db"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/constants"
	"github.com/wt5858/trading-agents-go/internal/helpers/logger"
	"github.com/wt5858/trading-agents-go/pkg/mq"
)

// ===========================================================================
// 单例
// ===========================================================================
//
// 基础设施句柄用 sync.Once 包成进程内单例，理由是一个进程可能调用多个注入器
// （worker 同时要 AmqpHandlers、DomainEventHandlers 和 WorkerRunners），
// 而它们都需要同一条数据库连接、同一个消息队列客户端。
// 没有 sync.Once 的话，每个注入器会各开一套连接池。
//
// 注意 sync.Once 只在**进程内**成立。凡是需要跨副本的保证（例如启动期迁移
// 不能被多个副本同时执行），都不能指望它——那种保证必须来自数据库或 broker。

var (
	connectionsOnce sync.Once
	connections     *db.Connections
	connectionsErr  error
)

// NewSingletonConnections 建立全部基础设施连接，并在此期间把表结构推到最新。
//
// ===========================================================================
// 迁移为什么放在这里
// ===========================================================================
//
// 因为这是「数据库刚连上、还没有任何人用它」的唯一时刻。放在更早不可能
// （还没有连接），放在更晚则意味着某些仓储已经拿到连接、可能已经在查一张
// 还没建好的表。
//
// 把它放在连接之后、业务装配之前，还让「代码和它需要的表结构必须同时到位」
// 这件事由进程自己负责，而不是依赖一个人记得在发布前跑一次迁移命令。
// 忘了跑、跑错顺序、或在滚动发布中途跑，都会让新代码撞上旧表结构。
//
// 多副本同时启动的并发由 MigrateOnBoot 里的数据库具名锁收敛，不是靠这里的
// sync.Once——它只保证一个进程跑一次，而三个副本就是三个进程。
func NewSingletonConnections(ctx context.Context, cfg *config.Config, log *zap.Logger) (*db.Connections, error) {
	connectionsOnce.Do(func() {
		conns, err := db.Open(ctx, cfg, log)
		if err != nil {
			connectionsErr = err
			return
		}
		if err := conns.MigrateOnBoot(ctx, log); err != nil {
			_ = conns.Close(ctx)
			connectionsErr = err
			return
		}
		if err := conns.EnsureMongoIndexes(ctx, log); err != nil {
			_ = conns.Close(ctx)
			connectionsErr = err
			return
		}
		connections = conns
	})
	return connections, connectionsErr
}

// 下面三个把 Connections 拆成各仓储真正需要的句柄。
//
// 仓储只收 *gorm.DB / *mongo.Database / *redis.Client，不收 *db.Connections：
// 一个只用 MySQL 的仓储不该在签名上宣称自己可能碰 Mongo 和 Redis。
// 参数表就是最诚实的依赖声明，放宽它等于放弃这份声明。

func NewGormDB(conns *db.Connections) *gorm.DB { return conns.MySQL }

func NewMongoDatabase(conns *db.Connections) *mongo.Database { return conns.Mongo }

func NewRedisClient(conns *db.Connections) *redis.Client { return conns.Redis }

var (
	brokerOnce sync.Once
	broker     *mq.AMQP
	brokerErr  error
)

// NewSingletonAmqp 建立消息队列客户端并声明拓扑。
//
// 拓扑在这里一次性声明——**包括只发消息不消费的进程**。
// 否则 HTTP 进程发出的第一条领域事件可能落到一个还不存在的交换机上，
// 而 AMQP 对此不报错，只是把消息丢掉。
func NewSingletonAmqp(cfg *config.Config, log *zap.Logger) (*mq.AMQP, error) {
	brokerOnce.Do(func() {
		opts := cfg.AMQP.Options(log)

		// 把 trace_id 的存取挂进去，让链路跨过消息队列这道边界。
		//
		// 为什么由这里来接，而不是 pkg/mq 自己 import logger：config 依赖
		// pkg/mq 拿 Options 与 ConnectionInfo，而 logger 依赖 config——
		// pkg/mq 直接 import logger 会形成 import 环，编译不过。
		// 于是 pkg/mq 只定义钩子的形状，由装配层（也就是这里）填实现。
		//
		// 不填也能跑：pkg/mq 的默认值是「取不到」和「原样返回」，
		// 代价只是 worker 的日志里没有 trace_id——而这正是不接线时该有的表现。
		opts.TraceIDFromContext = logger.TraceIDFromContext
		opts.WithTraceID = logger.WithTraceID

		client := mq.New(cfg.AMQP.ConnectionInfo(), opts)
		if err := client.InitiateSubscriber(cfg.AMQP.ExchangeList(), cfg.AMQP.QueueList()); err != nil {
			brokerErr = err
			return
		}
		broker = client
	})
	return broker, brokerErr
}

// NewDomainEventBus 建领域事件总线。
//
// 返回具体类型而不是 domain_event.Publisher 接口：发布方需要接口
// （由 wire.Bind 绑定），而订阅方需要 RegisterSubscriber 与 Dispatch 这两个
// 接口上没有的方法。返回具体类型让两边都拿得到自己要的东西。
func NewDomainEventBus(broker *mq.AMQP, log *zap.Logger) *domain_event.AmqpBus {
	return domain_event.NewAmqpBus(broker, constants.ExchangeTradingAgents, constants.RoutingKeyDomainEvent, log)
}

// CloseInfra 关闭进程持有的全部基础设施连接。
//
// 先停消息队列再关数据库：反过来的话，正在处理消息的消费者会在数据库已经关掉的
// 情况下继续跑完手上那条消息，制造一批除了噪音以外毫无意义的错误日志。
//
// 它不是 provider，而是给 cmd 在退出路径上调用的。Wire 支持 cleanup 函数，
// 但那要求每个注入器各自持有一份清理逻辑；而这些连接是**进程级单例**，
// 由谁创建并不重要，关闭它们只该有一个入口。
func CloseInfra(ctx context.Context, log *zap.Logger) {
	if broker != nil {
		if err := broker.Close(); err != nil {
			log.Warn("关闭消息队列失败", zap.Error(err))
		}
	}
	if connections != nil {
		if err := connections.Close(ctx); err != nil {
			log.Warn("关闭数据库连接失败", zap.Error(err))
		}
	}
}

// ===========================================================================
// 带名字的 HTTP 客户端
// ===========================================================================
//
// 行情与大模型各需要一个 *http.Client，超时值差一个数量级（30s vs 180s）。
// Wire 按**类型**匹配依赖，两个 *http.Client 对它是同一样东西，会直接报冲突。
//
// 定义两个具名类型把它们区分开。这不是为了迁就 Wire——手写装配时同样存在
// 「哪个 client 传给了谁」的问题，只是那时靠变量名区分，而变量名编译器不检查。
// 具名类型把这件事交给了类型系统。
type (
	MarketHTTPClient *http.Client
	LLMHTTPClient    *http.Client
)

// 两个客户端都必须自带 Transport。
//
// Transport 留空会落到 http.DefaultTransport，而它的 MaxIdleConnsPerHost 是 **2**。
// 这个默认值是给「偶尔调一下别人接口」的程序准备的，不是给我们这种形态：
// 分析任务全局并发 20，每个任务内部分析师再扇出 3 路，几十个请求同时打向**同一个**
// 大模型域名，而连接池只肯留 2 条空闲连接——剩下的每次用完即关，于是几乎每一次
// 模型调用都要重新握一次 TCP + TLS。在跨境链路上这笔握手开销比请求本身还显眼，
// 而且它不表现为报错，只表现为「分析怎么越来越慢」。行情同步扇出 8 路，同理。
//
// 另外，DefaultTransport 是全进程共享的：两个客户端都用它，等于行情和大模型在抢
// 同一个池子，一边的突发会把另一边的空闲连接挤掉。各持一个副本就没有这回事。
//
// 刻意不设 ResponseHeaderTimeout：非流式的模型请求，部分网关要等整段生成完才吐
// 响应头，设了它等于给生成时间加了一个隐形上限，而超时的表现会是「长回答必失败」。
// 单次请求的总时长已经由 Client.Timeout 管着。
func newPooledTransport(maxPerHost int) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = maxPerHost
	t.MaxIdleConns = maxPerHost * 2
	return t
}

func NewMarketHTTPClient(cfg *config.Config) MarketHTTPClient {
	// 行情侧的并发上限是同步扇出（SyncFanOutLimit=8），留一倍余量。
	return &http.Client{Timeout: cfg.Market.Timeout, Transport: newPooledTransport(16)}
}

func NewLLMHTTPClient(cfg *config.Config) LLMHTTPClient {
	// 大模型侧按 全局并发 20 × 分析师扇出 3 估算，取 64 覆盖峰值。
	return &http.Client{Timeout: cfg.LLM.Timeout, Transport: newPooledTransport(64)}
}

// InfraSet 是每个入口点都需要的底座。
var InfraSet = wire.NewSet(
	NewSingletonConnections,
	NewGormDB,
	NewMongoDatabase,
	NewRedisClient,
	NewSingletonAmqp,
	NewDomainEventBus,
	NewMarketHTTPClient,
	NewLLMHTTPClient,

	// 全部 domain_services 都只认 Publisher 接口——它们不该知道事件是走消息队列
	// 还是走别的什么。这条绑定是接口与实现之间唯一的接缝，也是把它们换掉时
	// 唯一需要改的一行。
	wire.Bind(new(domain_event.Publisher), new(*domain_event.AmqpBus)),
)
