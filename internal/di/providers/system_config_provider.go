package providers

import (
	"context"
	"net/http"

	"github.com/google/wire"

	agent_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_services"
	agent_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	system_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/domain_services"
	system_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/repositories"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/llm"
)

// 配置中心的装配，以及它对外的那个探测端口。
//
// 覆盖方向是单向的：文件/环境变量给基础设施打底，数据库只覆盖运行期可调的部分。
// 反过来让数据库管连接串，一条坏记录就能把服务锁在自己的数据库外面。

// ProviderProbe 用真实的 LLM 客户端探测供应商连通性。
//
// 它实现 system_config 声明的窄端口，而不是把整个 Router 交过去：
// 探测只需要发一次最小请求，给它 Router 就等于给了它改路由表的能力。
//
// 每次探测都按待测配置临时建一个客户端，而不是复用已注册的路由：
// 管理员点「测试」的典型场景恰恰是刚改完地址或密钥还没生效，
// 复用旧客户端测的是旧配置，测了等于没测。
type ProviderProbe struct {
	httpClient *http.Client
}

var _ system_services.ProviderProbe = (*ProviderProbe)(nil)

func NewProviderProbe(client LLMHTTPClient) *ProviderProbe {
	return &ProviderProbe{httpClient: (*http.Client)(client)}
}

func (p *ProviderProbe) Probe(ctx context.Context, target system_services.ResolvedProvider, model string) error {
	client := NewLLMClient(target.Kind, target.Name, target.BaseURL, target.APIKey, p.httpClient)

	if model == "" {
		if len(target.Models) == 0 {
			return custom_errors.Invalid("供应商 %s 未配置任何模型，无法探测", target.Name)
		}
		model = target.Models[0]
	}

	// 一次最小补全：足以验证地址可达、密钥有效、模型名存在，
	// 又不会产生可观的费用（MaxTokens 压到个位数）。
	_, err := client.Complete(ctx, agent_vo.CompletionRequest{
		Model:     model,
		Messages:  []agent_vo.Message{{Role: agent_vo.RoleUser, Content: "ping"}},
		MaxTokens: 8,
	})
	if err != nil {
		return custom_errors.Unavailable("供应商 %s 探测失败", target.Name).Wrap(err)
	}
	return nil
}

// NewLLMClient 按厂商类型建客户端。
//
// openai_compat 一种实现覆盖 OpenAI / DeepSeek / 通义 / 智谱 / 硅基流动 / OpenRouter / Ollama：
// 它们的差异只在 baseURL 与密钥，协议是同一套。
func NewLLMClient(kind, name, baseURL, apiKey string, httpClient *http.Client) agent_services.LLMClient {
	switch kind {
	case "anthropic":
		return llm.NewAnthropic(apiKey, baseURL, httpClient)
	case "google":
		return llm.NewGoogle(apiKey, baseURL, httpClient)
	default:
		return llm.NewOpenAICompat(name, baseURL, apiKey, httpClient)
	}
}

var SystemConfigSet = wire.NewSet(
	NewProviderProbe,
	system_repo.NewLLMProviderRepository,
	system_repo.NewSystemSettingRepository,
	system_services.NewLLMProviderService,
	system_services.NewConfigService,
	system_services.NewProviderResolver,

	wire.Bind(new(system_services.ProviderProbe), new(*ProviderProbe)),
)
