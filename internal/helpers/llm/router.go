package llm

import (
	"strings"
	"sync"

	agentsvc "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_services"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Router 按模型名把请求分发到对应 Provider 客户端。
// 领域层只认模型字符串，具体走哪家、用什么密钥全在这里收敛。
type Router struct {
	mu           sync.RWMutex
	clients      map[string]agentsvc.LLMClient // provider -> 客户端
	models       map[string]string             // 模型名 -> provider
	defaultModel string
}

func NewRouter(defaultModel string) *Router {
	return &Router{
		clients:      map[string]agentsvc.LLMClient{},
		models:       map[string]string{},
		defaultModel: strings.TrimSpace(defaultModel),
	}
}

// Register 注册一个 Provider 客户端，并声明它负责的模型名。
// 同名 provider 重复注册会覆盖，方便运行期热替换配置。
func (r *Router) Register(c agentsvc.LLMClient, models ...string) {
	if c == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	provider := c.Provider()
	r.clients[provider] = c
	for _, m := range models {
		if m = normalizeModel(m); m != "" {
			r.models[m] = provider
		}
	}
}

// MapModel 单独补一条模型映射，用于注册之后再追加新模型。
func (r *Router) MapModel(model, provider string) {
	if model = normalizeModel(model); model == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models[model] = provider
}

func (r *Router) DefaultModel() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultModel
}

// Resolve 解析模型名。支持两种写法：
//   - "deepseek-chat"        —— 查映射表
//   - "deepseek/deepseek-chat" —— 显式指定 provider，优先级最高
//
// 解析不到时回落到默认模型；默认模型也解析不到才报 Invalid。
func (r *Router) Resolve(model string) (agentsvc.LLMClient, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	name := normalizeModel(model)
	if name == "" {
		name = r.defaultModel
		if name == "" {
			return nil, "", custom_errors.Invalid("未指定模型且未配置默认模型")
		}
	}

	if c, actual, ok := r.lookup(name); ok {
		return c, actual, nil
	}
	// 回落：配置里漏了某个模型时，宁可用默认模型跑通，也好过整条分析链路直接失败。
	if r.defaultModel != "" && r.defaultModel != name {
		if c, actual, ok := r.lookup(r.defaultModel); ok {
			return c, actual, nil
		}
	}
	return nil, "", custom_errors.Invalid("无法解析模型 %q，且默认模型不可用", name)
}

// lookup 必须在持有读锁时调用。
func (r *Router) lookup(name string) (agentsvc.LLMClient, string, bool) {
	// 显式前缀优先。但像 SiliconFlow 的 "deepseek-ai/DeepSeek-V3" 这种模型名本身
	// 就带斜杠，所以只有前缀确实是已注册 provider 时才当作路由指令。
	if i := strings.Index(name, "/"); i > 0 {
		if c, ok := r.clients[name[:i]]; ok {
			return c, name[i+1:], true
		}
	}
	if provider, ok := r.models[name]; ok {
		if c, ok := r.clients[provider]; ok {
			return c, name, true
		}
	}
	return nil, "", false
}

func normalizeModel(m string) string { return strings.TrimSpace(m) }
