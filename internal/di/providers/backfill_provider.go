package providers

import (
	"github.com/google/wire"

	agent_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_services"
	agent_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories"
	stock_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories"
	"github.com/wt5858/trading-agents-go/internal/db"
)

// BackfillDeps 是 backfill 子命令要用到的几样东西。
//
// 打成一个结构体的理由与 WorkerRunners 相同：Wire 只造被需要的东西，
// 而这几样彼此之间没有依赖关系，必须由一个结构把它们同时「需要」出来。
//
// 为什么不在 cmd 里手工 new 这几个仓储（backtest 子命令就是那么做的）：
// 因为回填要用的是 EngineService，而它背后挂着 LLM 路由，
// 路由必须先从数据库读出供应商配置才能解析模型名。那条依赖顺序一旦手写，
// 就会和 serve 那边分叉——而分叉的表现是「管理员改了密钥但回填一直用旧的」。
// 既然引擎必须走 Wire，顺手让 Wire 把仓储也给出来，比一半注入一半手工干净。
type BackfillDeps struct {
	// Engine 直接驱动一次完整分析，不经过任务队列与 Redis 并发名额。
	Engine *agent_services.EngineService
	// Market 用来列出区间内**真实存在的交易日**。
	//
	// 不按日历推算：节假日、临时休市、以及这只票自己的停牌都会让日历日
	// 对不上实际有数据的交易日，而那些格子跑出来的必然是缺数据的分析——
	// 既花了钱，又给样本集掺进一批注定被跳过的噪声。
	Market *stock_repo.MarketDataRepository
	// Runs 用来判断某一格是否已经跑过，支撑断点续跑。
	Runs *agent_repo.AnalysisRunRepository
	// Conns 保留连接句柄，命令退出时要关。
	Conns *db.Connections
}

func NewBackfillDeps(
	engine *agent_services.EngineService,
	market *stock_repo.MarketDataRepository,
	runs *agent_repo.AnalysisRunRepository,
	conns *db.Connections,
) *BackfillDeps {
	return &BackfillDeps{Engine: engine, Market: market, Runs: runs, Conns: conns}
}

var BackfillSet = wire.NewSet(NewBackfillDeps)
