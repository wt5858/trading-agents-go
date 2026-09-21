package domain_services

import (
	"context"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// probeFanOutLimit 是批量探测的并发上限。
// 探测会真的打到各家厂商的接口上，它们都有限流；
// 这里宁可慢一点，也不要因为一次「测试全部」把某家的配额打满。
const probeFanOutLimit = 4

// SanitizedProvider 是供应商配置的对外读模型：没有密钥明文，只有掩码。
//
// 服务层返回它而不是返回聚合本身，是一个刻意的取舍：
// 这样「响应里不能出现明文密钥」就不再依赖 handler 作者记得调 Masked()，
// 而是结构上做不到——handler 手里根本没有 SecretValue。
// 代价是多了一层读模型，收益是这条安全要求不会因为将来新增一个接口而破功。
type SanitizedProvider struct {
	ID           uint64    `json:"id"`
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	BaseURL      string    `json:"baseUrl"`
	MaskedAPIKey string    `json:"apiKey"`
	HasAPIKey    bool      `json:"hasApiKey"`
	Models       []string  `json:"models"`
	Enabled      bool      `json:"enabled"`
	Usable       bool      `json:"usable"`
	Priority     int       `json:"priority"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// sanitize 把聚合降级成读模型。Usable 也一并算好：
// 让管理台自己拿 enabled/hasApiKey/models 去重新推导，就是把实体里的不变式
// 抄到了前端，两边迟早不一致。
func sanitize(p *entities.LLMProviderConfig) *SanitizedProvider {
	return &SanitizedProvider{
		ID:           p.ID,
		Name:         p.Name.String(),
		Kind:         p.Kind.String(),
		BaseURL:      p.BaseURL.String(),
		MaskedAPIKey: p.APIKey.Masked(),
		HasAPIKey:    !p.APIKey.IsZero(),
		Models:       append([]string(nil), p.Models...),
		Enabled:      p.Enabled,
		Usable:       p.Usable(),
		Priority:     p.Priority,
		CreatedAt:    p.CreatedAt,
		UpdatedAt:    p.UpdatedAt,
	}
}

func sanitizeAll(ps []*entities.LLMProviderConfig) []*SanitizedProvider {
	out := make([]*SanitizedProvider, 0, len(ps))
	for _, p := range ps {
		out = append(out, sanitize(p))
	}
	return out
}

// LLMProviderService 编排供应商配置的全部管理用例。
type LLMProviderService struct {
	repo      *repositories.LLMProviderRepository
	probe     ProviderProbe
	publisher domain_event.Publisher
}

func NewLLMProviderService(
	repo *repositories.LLMProviderRepository,
	probe ProviderProbe,
	publisher domain_event.Publisher,
) *LLMProviderService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	return &LLMProviderService{repo: repo, probe: probe, publisher: publisher}
}

type RegisterProviderCommand struct {
	Name     string
	Kind     string
	BaseURL  string
	APIKey   string
	Models   []string
	Priority int
	Enabled  bool
}

func (s *LLMProviderService) Register(ctx context.Context, op Operator, cmd RegisterProviderCommand) (*SanitizedProvider, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}

	name, err := value_objects.NewProviderName(cmd.Name)
	if err != nil {
		return nil, err
	}
	kind, err := value_objects.NewProviderKind(cmd.Kind)
	if err != nil {
		return nil, err
	}
	baseURL, err := value_objects.NewEndpointURL(cmd.BaseURL)
	if err != nil {
		return nil, err
	}
	apiKey, err := value_objects.NewSecret(cmd.APIKey)
	if err != nil {
		return nil, err
	}

	provider, err := entities.RegisterProvider(name, kind, baseURL, apiKey, cmd.Models, cmd.Priority, cmd.Enabled)
	if err != nil {
		return nil, err
	}
	// 不做「这个名字是否已存在」的前置查询，理由见仓储 Create 的注释。
	if err := s.repo.Create(ctx, provider); err != nil {
		if custom_errors.CodeOf(err) == custom_errors.CodeAlreadyExists {
			return nil, custom_errors.AlreadyExists("供应商已存在: %s", name.String())
		}
		return nil, err
	}
	s.publish(ctx, provider)
	return sanitize(provider), nil
}

// UpdateProviderCommand 里的指针字段区分「没传」和「传了空值」：
// 不用指针的话，管理台只想改优先级也会把 baseURL 清空。
type UpdateProviderCommand struct {
	BaseURL  *string
	Models   []string
	Priority *int
}

func (s *LLMProviderService) Update(ctx context.Context, op Operator, id uint64, cmd UpdateProviderCommand) (*SanitizedProvider, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}
	provider, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}

	if cmd.BaseURL != nil {
		baseURL, err := value_objects.NewEndpointURL(*cmd.BaseURL)
		if err != nil {
			return nil, err
		}
		if err := provider.UpdateEndpoint(baseURL); err != nil {
			return nil, err
		}
	}
	if cmd.Models != nil {
		if err := provider.SetModels(cmd.Models); err != nil {
			return nil, err
		}
	}
	if cmd.Priority != nil {
		if err := provider.SetPriority(*cmd.Priority); err != nil {
			return nil, err
		}
	}

	if err := s.repo.Update(ctx, provider); err != nil {
		return nil, err
	}
	s.publish(ctx, provider)
	return sanitize(provider), nil
}

// RotateKey 轮换密钥。
//
// 入参是裸 string 而不是 SecretValue：密钥从 HTTP 请求体进来时本来就是字符串，
// 让 handler 去构造值对象，就等于让 application 层持有一次密钥的明文副本。
// 在这里立刻转成 SecretValue，明文的生命周期就被压缩在这个函数的几行之内。
func (s *LLMProviderService) RotateKey(ctx context.Context, op Operator, id uint64, rawKey string) (*SanitizedProvider, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}
	key, err := value_objects.NewSecret(rawKey)
	if err != nil {
		return nil, err
	}
	provider, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := provider.RotateKey(key); err != nil {
		return nil, err
	}
	if err := s.repo.Update(ctx, provider); err != nil {
		return nil, err
	}
	s.publish(ctx, provider)
	return sanitize(provider), nil
}

func (s *LLMProviderService) Enable(ctx context.Context, op Operator, id uint64) (*SanitizedProvider, error) {
	return s.switchEnabled(ctx, op, id, true)
}

func (s *LLMProviderService) Disable(ctx context.Context, op Operator, id uint64) (*SanitizedProvider, error) {
	return s.switchEnabled(ctx, op, id, false)
}

// switchEnabled 让实体先判定状态迁移是否合法（并记下事件），
// 再由仓储用条件更新把这次迁移原子地落库。
// 实体的判定基于读到的那一瞬间，仓储的 WHERE 才是最终裁决——
// 两者都留着是有意的：前者给出准确的错误信息，后者保证不会写坏。
func (s *LLMProviderService) switchEnabled(ctx context.Context, op Operator, id uint64, enable bool) (*SanitizedProvider, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}
	provider, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}

	if enable {
		if err := provider.Enable(); err != nil {
			return nil, err
		}
		if err := s.repo.Enable(ctx, provider); err != nil {
			return nil, err
		}
	} else {
		if err := provider.Disable(); err != nil {
			return nil, err
		}
		if err := s.repo.Disable(ctx, provider); err != nil {
			return nil, err
		}
	}
	s.publish(ctx, provider)
	return sanitize(provider), nil
}

func (s *LLMProviderService) Delete(ctx context.Context, op Operator, id uint64) error {
	if err := requireAdmin(op); err != nil {
		return err
	}
	return s.repo.Delete(ctx, id)
}

func (s *LLMProviderService) Get(ctx context.Context, op Operator, id uint64) (*SanitizedProvider, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}
	provider, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return sanitize(provider), nil
}

func (s *LLMProviderService) List(ctx context.Context, op Operator, page shared_vo.Page) ([]*SanitizedProvider, int64, error) {
	if err := requireAdmin(op); err != nil {
		return nil, 0, err
	}
	providers, total, err := s.repo.List(ctx, page)
	if err != nil {
		return nil, 0, err
	}
	return sanitizeAll(providers), total, nil
}

// UsableProviders 返回当前真正能接活的供应商（已脱敏）。
//
// 过滤条件不是「enabled」而是实体的 Usable()：启用但没密钥、启用但没模型的记录
// 在这里就被挡掉，不会等到运行期才以一个 401 的形式暴露出来。
func (s *LLMProviderService) UsableProviders(ctx context.Context, op Operator) ([]*SanitizedProvider, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}
	providers, err := s.repo.ListEnabled(ctx)
	if err != nil {
		return nil, err
	}
	usable := make([]*entities.LLMProviderConfig, 0, len(providers))
	for _, p := range providers {
		if p.Usable() {
			usable = append(usable, p)
		}
	}
	return sanitizeAll(usable), nil
}

// TestConnection 对单家供应商发一次真实探测。
func (s *LLMProviderService) TestConnection(ctx context.Context, op Operator, id uint64) (*ProbeResult, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}
	if s.probe == nil {
		return nil, custom_errors.Unavailable("未配置连通性探测器")
	}
	provider, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	result := s.probeOne(ctx, provider)
	return &result, nil
}

// TestAllConnections 批量探测全部启用中的供应商。
//
// 用 concurrency.Settle 而不是 for 循环里串行发请求，也不是裸起 goroutine：
//   - 串行的话，五家供应商各超时 30 秒，管理员要等两分半；
//   - 裸 goroutine 没有并发上限，供应商多起来会同时打爆几家厂商的限流；
//   - Settle 而不是 Map，是因为一家不通不该让其余几家的结论丢失——
//     这个接口的全部价值就是那张「谁通谁不通」的表。
func (s *LLMProviderService) TestAllConnections(ctx context.Context, op Operator) ([]ProbeResult, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}
	if s.probe == nil {
		return nil, custom_errors.Unavailable("未配置连通性探测器")
	}
	providers, err := s.repo.ListEnabled(ctx)
	if err != nil {
		return nil, err
	}
	if len(providers) == 0 {
		return []ProbeResult{}, nil
	}

	outcomes, err := concurrency.Settle(ctx, providers, probeFanOutLimit,
		func(ctx context.Context, p *entities.LLMProviderConfig) (ProbeResult, error) {
			return s.probeOne(ctx, p), nil
		})
	if err != nil {
		// 只有父 context 被取消才会走到这里（探测本身的失败在 ProbeResult 里）。
		return nil, custom_errors.Unavailable("连通性探测被中断").Wrap(err)
	}

	results := make([]ProbeResult, 0, len(outcomes))
	for _, o := range outcomes {
		results = append(results, o.Value)
	}
	return results, nil
}

// probeOne 把一次探测的所有失败形态都收敛成 ProbeResult，不往外抛错误。
func (s *LLMProviderService) probeOne(ctx context.Context, p *entities.LLMProviderConfig) ProbeResult {
	result := ProbeResult{ProviderID: p.ID, ProviderName: p.Name.String()}

	if !p.Usable() {
		result.Message = "供应商当前不可用：需要同时满足已启用、已配置密钥（本地部署除外）、至少一个模型"
		return result
	}
	model, ok := p.PrimaryModel()
	if !ok {
		result.Message = "未配置任何模型，无法探测"
		return result
	}
	result.Model = model

	// 这是密钥第二个、也是最后一个合法的明文出口：
	// 探测要发一次真实请求，不给密钥就只能探到「地址是否可达」，没有意义。
	if err := s.probe.Probe(ctx, expose(p), model); err != nil {
		// 只回传领域消息，不回传原始错误：厂商返回的错误体里经常原样带着
		// 请求头，其中就有 Authorization。
		result.Message = custom_errors.MessageOf(err)
		return result
	}
	result.OK = true
	result.Message = "连通正常"
	return result
}

func (s *LLMProviderService) publish(ctx context.Context, p *entities.LLMProviderConfig) {
	if evts := p.GetAllPendingEvents(); len(evts) > 0 {
		_ = s.publisher.Publish(ctx, evts...)
	}
}

// ProviderResolver 是读侧：装配根用它构建 LLM 路由。
//
// 它被做成一个独立的类型、只有一个方法，而不是 LLMProviderService 上的一个
// 「带 includeSecret 参数的查询」。原因是这样一来「会交出密钥明文」就是
// 一个类型的全部职责，而不是某个通用查询 API 的一种可选行为——
// 前者只需要审计一个文件，后者需要审计每一个调用点。
//
// 它也没有 Operator 参数：它的调用方是装配根，不是某个登录用户。
// 加一个永远传管理员的参数，只会让「谁在调它」这件事变得模糊。
type ProviderResolver struct {
	repo *repositories.LLMProviderRepository
}

func NewProviderResolver(repo *repositories.LLMProviderRepository) *ProviderResolver {
	return &ProviderResolver{repo: repo}
}

// Resolve 返回全部可用供应商，含明文密钥，按优先级降序。
//
// 唯一的合法调用方是装配根（internal/di），用来构造各家的 HTTP 客户端。
// 任何把它的返回值转发给 handler、写进日志、或者塞进领域事件的用法都是错的。
func (r *ProviderResolver) Resolve(ctx context.Context) ([]ResolvedProvider, error) {
	providers, err := r.repo.ListEnabled(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ResolvedProvider, 0, len(providers))
	for _, p := range providers {
		if !p.Usable() {
			continue
		}
		out = append(out, expose(p))
	}
	return out, nil
}
