package providers

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/wire"
	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/config"
	stock_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	stock_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories"
	"github.com/wt5858/trading-agents-go/internal/helpers/marketdata"
)

// NewMarketDataProvider 按配置声明的顺序组装数据源，Composite 逐个降级。
//
// 顺序来自 cfg.Market.Providers 而不是这里的 append 次序：哪个源该优先取决于部署
// 环境（有没有 Tushare 积分、在不在大陆网络内），写死在代码里就得改代码重新发版。
//
// 返回 *marketdata.Composite 这个具体类型而不是 DataProvider 接口，
// 是因为 Wire 按类型匹配：若返回接口，任何一个也返回该接口的 provider
// 都会与它冲突，而降级链的组装恰恰需要多个实现共存。
func NewMarketDataProvider(cfg *config.Config, client MarketHTTPClient, log *zap.Logger) *marketdata.Composite {
	httpClient := (*http.Client)(client)

	// 每个构造器要么返回数据源，要么返回一句「为什么没装上」。
	//
	// 返回原因而不是光返回 nil：配置里明明写了某个源、它却没出现在链上，是这块
	// 最容易卡住人的地方——降级链的错误信息只会列出「已尝试」的源，缺席的那个
	// 根本不会出现，于是现象是「我配了 X 但日志里连提都没提过 X」。
	build := map[string]func() (stock_services.DataProvider, string){
		"tushare": func() (stock_services.DataProvider, string) {
			if cfg.Market.TushareToken == "" {
				return nil, "未配置 TA_MARKET_TUSHARE_TOKEN"
			}
			return marketdata.NewTushareProvider(cfg.Market.TushareToken, httpClient), ""
		},
		"finnhub": func() (stock_services.DataProvider, string) {
			if cfg.Market.FinnhubToken == "" {
				return nil, "未配置 TA_MARKET_FINNHUB_TOKEN"
			}
			return marketdata.NewFinnhubProvider(cfg.Market.FinnhubToken, httpClient), ""
		},
		"eastmoney": func() (stock_services.DataProvider, string) {
			// 不需要密钥，但有速率约束：东财超限不返回错误码，直接掐连接。
			return marketdata.NewEastmoneyProvider(
				httpClient, cfg.Market.EastmoneyRPS, cfg.Market.EastmoneyBurst), ""
		},
		"mock": func() (stock_services.DataProvider, string) {
			// 两道开关是刻意的：显式列进 Providers 表达「我要用」，
			// EnableMock 则是生产环境防手滑的第二道锁——假数据流到用户面前
			// 这件事，值得多一把锁。代价是配了却没生效时必须说清楚，所以有下面这句。
			if !cfg.Market.EnableMock {
				return nil, "TA_MARKET_ENABLE_MOCK=false"
			}
			return marketdata.NewMockProvider(), ""
		},
	}

	var (
		providers []stock_services.DataProvider
		enabled   []string
		hasReal   bool
	)
	for _, name := range cfg.Market.Providers {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		ctor, known := build[name]
		if !known {
			// 一次配置笔误不该让整个服务起不来，但也绝不能静默——
			// 「配了 eastmony 结果一直在走 mock」是查不出来的那种问题。
			log.Warn("未知的行情数据源，已跳过",
				zap.String("provider", name),
				zap.Strings("可选值", []string{"tushare", "eastmoney", "finnhub", "mock"}))
			continue
		}
		p, skipReason := ctor()
		if p == nil {
			log.Warn("行情数据源已在配置中声明但未启用",
				zap.String("provider", name), zap.String("原因", skipReason))
			continue
		}
		providers = append(providers, p)
		enabled = append(enabled, name)
		if name != "mock" {
			hasReal = true
		}
	}

	// 一个真实源都没成的兜底。这条路径保证「什么都没配」时系统仍能端到端跑通，
	// 代价是数据全是编的，所以必须吼一声。
	if !hasReal {
		log.Warn("未启用任何真实行情数据源，产出的分析仅供联调，不具参考价值",
			zap.Strings("providers", cfg.Market.Providers))
		if len(providers) == 0 {
			providers = append(providers, marketdata.NewMockProvider())
			enabled = append(enabled, "mock")
		}
	}
	log.Info("行情数据源降级链已装配", zap.Strings("chain", enabled))
	return marketdata.NewComposite(providers...)
}

// NewSyncConfig 把散落在配置里的同步参数收成一个值对象。
//
// 并发上限由**数据源配额**决定而不是核数：Tushare 免费档约 500 次/分钟，
// Finnhub 免费档 60 次/分钟。真正的限流在数据源实现里，这里只保证不会有
// 几千个请求同时在飞。
func NewSyncConfig(cfg *config.Config) stock_services.SyncConfig {
	return stock_services.SyncConfig{
		FanOutLimit:       cfg.Market.SyncFanOutLimit,
		ChunkSize:         cfg.Market.SyncChunkSize,
		KlineLookbackDays: 365,
		NewsLookbackDays:  7,
		NewsLimit:         20,
		FinancialLimit:    8,
		StaleAfter:        2 * time.Hour,
	}
}

var StockSet = wire.NewSet(
	NewMarketDataProvider,
	NewSyncConfig,
	stock_repo.NewStockRepository,
	stock_repo.NewMarketDataRepository,
	stock_repo.NewSyncRunRepository,
	stock_services.NewStockService,
	stock_services.NewSyncService,

	wire.Bind(new(stock_services.DataProvider), new(*marketdata.Composite)),
)
