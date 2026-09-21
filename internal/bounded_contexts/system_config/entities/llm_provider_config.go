// Package entities 是配置中心的聚合根，所有业务不变式都住在这里。
//
// 本上下文只有两个聚合根：LLMProviderConfig 与 SystemSetting。
// 它们各自独立——一个供应商被停用不该要求某个配置项跟着改，反之亦然，
// 所以它们是两个聚合而不是一个「配置」大聚合。
package entities

import (
	"sort"
	"strings"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// LocalProviderName 是本地部署的模型服务名。
//
// Ollama 跑在自己的机器（或同一个内网）上，它根本没有账号体系，也就没有密钥可配。
// 这不是「配置上的特例」，而是一条业务规则：判断一家供应商能不能用，
// 对本地部署和对云厂商本来就是两套标准。规则写在实体里，
// 路由装配、连通性探测、管理台三条读路径拿到的判定才会一致；
// 写在任何一个调用方那里，另外两个迟早会漏掉它。
const LocalProviderName = "ollama"

// LLMProviderConfig 是一家大模型供应商的接入配置，配置中心的聚合根之一。
//
// 字段全部导出：这是本仓库聚合根的一贯写法，靠「写路径只走方法」的约定而不是
// 靠不可见性来保护不变式。唯一的例外是 APIKey——它的保护来自类型本身
// （SecretValue 默认形态就是掩码），而不是来自字段可见性。
type LLMProviderConfig struct {
	domain_event.EventRecorder

	ID      uint64
	Name    value_objects.ProviderName
	Kind    value_objects.ProviderKind
	BaseURL value_objects.EndpointURL
	APIKey  value_objects.SecretValue
	Models  []string
	Enabled bool
	// Priority 数值越大越优先。同一个模型名被多家供应商声明时（比如自建网关和官方
	// 各配了一份 deepseek-chat），装配根按它决定谁后注册、谁覆盖谁。
	Priority  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RegisterProvider 是供应商进入系统的唯一入口。
//
// 这里不拦「启用了但不可用」的组合：可用性由 Usable() 统一判定，
// 读路径本来就会过滤。若在此处再拦一道，就会出现「先建好禁用的、
// 补齐密钥再启用」和「一次建好」两条语义不同的路径，
// 而 Usable() 的存在意义正是让这件事只有一处判定。
func RegisterProvider(
	name value_objects.ProviderName,
	kind value_objects.ProviderKind,
	baseURL value_objects.EndpointURL,
	apiKey value_objects.SecretValue,
	models []string,
	priority int,
	enabled bool,
) (*LLMProviderConfig, error) {
	if name.IsZero() {
		return nil, custom_errors.Invalid("供应商名称不能为空")
	}
	if !kind.Valid() {
		return nil, custom_errors.Invalid("供应商协议类型不合法: %s", kind.String())
	}
	if priority < 0 {
		return nil, custom_errors.Invalid("优先级不能为负数")
	}
	normalized, err := normalizeModels(models)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	return &LLMProviderConfig{
		ID:        0,
		Name:      name,
		Kind:      kind,
		BaseURL:   baseURL,
		APIKey:    apiKey,
		Models:    normalized,
		Enabled:   enabled,
		Priority:  priority,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// IsLocal 判断这是不是本地部署的模型服务。见 LocalProviderName 的注释。
func (p *LLMProviderConfig) IsLocal() bool { return p.Name.String() == LocalProviderName }

// Usable 是本聚合真正的不变式：这家供应商现在能不能接活。
//
// 三个条件缺一不可——启用、有密钥、至少有一个模型。少了模型的供应商注册进路由表
// 等于一个永远匹配不上的空条目；少了密钥的供应商会在第一次调用时返回 401，
// 而那时调用已经消耗了一次分析任务的重试额度。
// 与其让不可用的配置混进路由表再在运行期炸，不如在装配时就筛掉。
func (p *LLMProviderConfig) Usable() bool {
	if !p.Enabled {
		return false
	}
	if len(p.Models) == 0 {
		return false
	}
	// 本地部署没有密钥这一说，这条例外是业务规则，不是配置兼容性补丁。
	if p.APIKey.IsZero() && !p.IsLocal() {
		return false
	}
	return true
}

// UpdateEndpoint 改接入地址，用于切自建网关或切代理。
func (p *LLMProviderConfig) UpdateEndpoint(baseURL value_objects.EndpointURL) error {
	if p.BaseURL.String() == baseURL.String() {
		return nil
	}
	p.BaseURL = baseURL
	p.UpdatedAt = time.Now()
	return nil
}

// RotateKey 轮换密钥。
//
// 拒绝把密钥轮换成空值：想让一家供应商停止服务应当用 Disable()，
// 那条路径会发事件、会被审计。用「把密钥清空」来达到同样效果的话，
// 配置看起来仍然是启用的，故障表现却是所有调用 401——
// 这是运维最难排查的一类状态。
func (p *LLMProviderConfig) RotateKey(key value_objects.SecretValue) error {
	if key.IsZero() && !p.IsLocal() {
		return custom_errors.Invalid("密钥不能为空；如需停止使用该供应商请停用它")
	}
	if p.APIKey.Equal(key) {
		// 值没变就不记事件：一次「没改动的保存」不该在审计流水里留下一次轮换记录。
		return nil
	}
	p.APIKey = key
	p.UpdatedAt = time.Now()
	// 事件只带名字，不带密钥。理由见 domain_events 的包注释。
	p.AddDomainEvent(domain_events.NewOnProviderKeyRotated(p.ID, p.Name.String()))
	return nil
}

// SetModels 重设该供应商声明支持的模型列表。
//
// 不接受空列表：清空模型会让这家供应商在路由表里变成一个匹配不到任何请求的
// 僵尸条目，看起来启用着却永远不工作。要让它下线就 Disable()。
func (p *LLMProviderConfig) SetModels(models []string) error {
	normalized, err := normalizeModels(models)
	if err != nil {
		return err
	}
	if len(normalized) == 0 {
		return custom_errors.Invalid("至少需要配置一个模型；如需停用该供应商请停用它")
	}
	p.Models = normalized
	p.UpdatedAt = time.Now()
	return nil
}

// SetPriority 调整同名模型的覆盖顺序。
func (p *LLMProviderConfig) SetPriority(priority int) error {
	if priority < 0 {
		return custom_errors.Invalid("优先级不能为负数")
	}
	p.Priority = priority
	p.UpdatedAt = time.Now()
	return nil
}

// Enable 启用供应商。
//
// 重复启用报 Conflict 而不是静默成功：管理台点两次「启用」和
// 两个管理员同时点「启用」，前者无害、后者说明有人的操作被覆盖了。
// 报冲突让第二次操作看得见，调用方可以据此提示「状态已变更，请刷新」。
func (p *LLMProviderConfig) Enable() error {
	if p.Enabled {
		return custom_errors.Conflict("供应商已处于启用状态: %s", p.Name.String())
	}
	p.Enabled = true
	p.UpdatedAt = time.Now()
	p.AddDomainEvent(domain_events.NewOnProviderEnabled(p.ID, p.Name.String()))
	return nil
}

func (p *LLMProviderConfig) Disable() error {
	if !p.Enabled {
		return custom_errors.Conflict("供应商已处于停用状态: %s", p.Name.String())
	}
	p.Enabled = false
	p.UpdatedAt = time.Now()
	p.AddDomainEvent(domain_events.NewOnProviderDisabled(p.ID, p.Name.String()))
	return nil
}

// PrimaryModel 取一个用于连通性探测的模型名。
// 探测要真发一次请求才有意义，而发请求必须指定模型。
func (p *LLMProviderConfig) PrimaryModel() (string, bool) {
	if len(p.Models) == 0 {
		return "", false
	}
	return p.Models[0], true
}

// normalizeModels 去空白、去空项、去重并排序。
//
// 排序是为了让「同一组模型」在库里有唯一的表示形态：否则两次保存只是调换了顺序，
// 就会产生一条看起来有差异、实际没有差异的变更记录。
func normalizeModels(models []string) ([]string, error) {
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if strings.ContainsAny(m, " \t\n") {
			return nil, custom_errors.Invalid("模型名不能包含空白字符: %q", m)
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	sort.Strings(out)
	return out, nil
}
