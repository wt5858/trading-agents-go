package providers

import (
	"context"
	"net/http"
	"time"

	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/config"
	agent_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_services"
	agent_entities "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	agent_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories"
	system_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/domain_services"
	"github.com/wt5858/trading-agents-go/internal/helpers/llm"
)

// NewLLMRouter 建立模型路由表。
//
// ===========================================================================
// 这个函数的参数表就是一条重要约束
// ===========================================================================
//
// 它收 *ProviderResolver，于是 Wire 必然先把配置中心装配好再造它。
// 这条「配置中心先于 agent」的顺序在手写组装根里只体现为两行代码的先后，
// 谁调换了顺序编译照样通过，故障要等到运行时才出现——表现为
// 「管理员在后台改了密钥但一直不生效」。
// 写进参数表之后，这个顺序由编译器保证。
//
// 覆盖顺序：先按文件配置注册作为兜底，再用库里的同名供应商盖掉。
// 库里有记录就以库为准，这样改密钥、换地址不需要改文件重启；
// 库为空（全新部署）时文件配置原样生效。
func NewLLMRouter(
	ctx context.Context,
	cfg *config.Config,
	client LLMHTTPClient,
	resolver *system_services.ProviderResolver,
	log *zap.Logger,
) *llm.Router {
	httpClient := (*http.Client)(client)
	router := llm.NewRouter(cfg.LLM.DefaultModel)

	for _, p := range cfg.LLM.EnabledProviders() {
		router.Register(NewLLMClient(p.Kind, p.Name, p.BaseURL, p.APIKey, httpClient), p.Models...)
	}

	resolved, err := resolver.Resolve(ctx)
	if err != nil {
		// 读不到配置中心不该让服务起不来：文件配置仍然可用。
		log.Warn("读取配置中心的 LLM 供应商失败，回退到文件配置", zap.Error(err))
		return router
	}
	for _, p := range resolved {
		router.Register(NewLLMClient(p.Kind, p.Name, p.BaseURL, p.APIKey, httpClient), p.Models...)
	}
	if len(resolved) > 0 {
		log.Info("已从配置中心加载 LLM 供应商", zap.Int("count", len(resolved)))
	}
	return router
}

// NewCompletionCache 建发言缓存。
//
// TTL 只是内存上限，不是「结论多久失效」——后者由缓存键本身回答：
// 键是提示词指纹，输入变了指纹就变了，根本命中不到旧结论。
// 详见 agent/repositories/completion_cache.go 顶部那段说明。
func NewCompletionCache(rdb *redis.Client) *agent_repo.CompletionCache {
	return agent_repo.NewCompletionCache(rdb, 24*time.Hour)
}

func NewRuntimeConfig() agent_services.RuntimeConfig {
	return agent_services.RuntimeConfig{
		Temperature:     0.3,
		MaxTokens:       4096,
		MaxToolRounds:   5,
		ToolFanOutLimit: 4,
	}
}

func NewEngineConfig() agent_services.EngineConfig {
	return agent_services.EngineConfig{
		KlineLookbackDays: 250,
		KlineLimit:        300,
		NewsLookbackDays:  30,
		NewsLimit:         50,
		SocialLimit:       50,
		FinancialLimit:    8,
		IndicatorTTL:      15 * time.Minute,
		DataFanOutLimit:   4,
	}
}

var AgentSet = wire.NewSet(
	NewLLMRouter,
	NewRuntimeConfig,
	NewEngineConfig,
	NewCompletionCache,
	agent_repo.NewIndicatorRepository,
	agent_repo.NewAnalysisRunRepository,
	agent_services.NewPromptService,
	agent_services.NewStockToolRegistry,
	agent_services.NewRuntimeService,
	agent_services.NewEngineService,
	agent_services.NewDecisionChainService,

	// agent 上下文只认自己声明的窄端口，不认 helpers 里的具体实现，
	// 也不认 stock 上下文的服务类型——跨上下文只通过接口往来。
	wire.Bind(new(agent_services.ModelRouter), new(*llm.Router)),
	wire.Bind(new(agent_services.ToolRegistry), new(*agent_services.StockToolRegistry)),
	wire.Bind(new(agent_entities.Runtime), new(*agent_services.RuntimeService)),
)

// 注意：agent 对 stock 的两条依赖（MarketReader / MarketBackfiller）不在这里，
// 而在 CoreSet——Wire 要求 wire.Bind 与具体类型的 provider 同处一个 set。
// 详见 core_provider.go。
