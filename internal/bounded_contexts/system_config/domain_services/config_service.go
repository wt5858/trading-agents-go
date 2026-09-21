package domain_services

import (
	"context"
	"sort"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// SanitizedSetting 是配置项的对外读模型。
// Value 对密钥类配置项已经打码，和 SanitizedProvider 一样，
// 让「不泄露」成为结构上的保证而不是 handler 的自觉。
type SanitizedSetting struct {
	Key         string    `json:"key"`
	Scope       string    `json:"scope"`
	Value       string    `json:"value"`
	Secret      bool      `json:"secret"`
	Description string    `json:"description"`
	UpdatedBy   uint64    `json:"updatedBy"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func sanitizeSetting(s *entities.SystemSetting) *SanitizedSetting {
	return &SanitizedSetting{
		Key:         s.Key.String(),
		Scope:       s.Scope.String(),
		Value:       s.MaskedValue(),
		Secret:      s.IsSecret(),
		Description: s.Description,
		UpdatedBy:   s.UpdatedBy,
		UpdatedAt:   s.UpdatedAt,
	}
}

func sanitizeSettings(rows []*entities.SystemSetting) []*SanitizedSetting {
	out := make([]*SanitizedSetting, 0, len(rows))
	for _, r := range rows {
		out = append(out, sanitizeSetting(r))
	}
	return out
}

// ConfigSnapshot 是给管理台看的全局配置快照，全程脱敏。
//
// 它只包含【数据库管的那一半】配置。基础设施参数（数据库连接、端口、JWT 密钥）
// 不在这里，也永远不该加进来：管理台没有展示 JWT 密钥的正当需求，
// 而多一个展示点就多一个泄露面。理由详见本包的包注释。
type ConfigSnapshot struct {
	Providers   []*SanitizedProvider `json:"providers"`
	Settings    []*SanitizedSetting  `json:"settings"`
	GeneratedAt time.Time            `json:"generatedAt"`
}

// ConfigService 编排运行期配置项的读写。
type ConfigService struct {
	settingRepo  *repositories.SystemSettingRepository
	providerRepo *repositories.LLMProviderRepository
	publisher    domain_event.Publisher
}

func NewConfigService(
	settingRepo *repositories.SystemSettingRepository,
	providerRepo *repositories.LLMProviderRepository,
	publisher domain_event.Publisher,
) *ConfigService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	return &ConfigService{settingRepo: settingRepo, providerRepo: providerRepo, publisher: publisher}
}

func (s *ConfigService) Get(ctx context.Context, op Operator, rawKey string) (*SanitizedSetting, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}
	key, err := value_objects.NewSettingKey(rawKey)
	if err != nil {
		return nil, err
	}
	setting, err := s.settingRepo.FindByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return sanitizeSetting(setting), nil
}

func (s *ConfigService) ListByScope(ctx context.Context, op Operator, rawScope string) ([]*SanitizedSetting, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}
	scope, err := value_objects.NewSettingScope(rawScope)
	if err != nil {
		return nil, err
	}
	settings, err := s.settingRepo.ListByScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	return sanitizeSettings(settings), nil
}

type SetSettingCommand struct {
	Key         string
	Value       string
	Description string
}

// Set 写入一项配置。
//
// 这里先查后写，但它不是 check-then-act：写入本身是一条 upsert 语句，
// 存在与否都能正确落库，正确性完全不依赖前面那次查询。
// 查询只有一个用途——拿到旧值，好让审计事件说得出「从什么改成了什么」。
// 极端并发下事件里的旧值可能是几毫秒前的版本，但库里的值始终是对的；
// 为了这点审计精度给配置表上乐观锁并不划算。
func (s *ConfigService) Set(ctx context.Context, op Operator, cmd SetSettingCommand) (*SanitizedSetting, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}
	key, err := value_objects.NewSettingKey(cmd.Key)
	if err != nil {
		return nil, err
	}
	value := value_objects.NewSettingValue(cmd.Value)

	setting, err := s.settingRepo.FindByKey(ctx, key)
	switch {
	case err == nil:
		// 已存在：走实体的状态迁移，由它决定要不要记事件。
		if err := setting.Change(value, op.UserID); err != nil {
			return nil, err
		}
		if cmd.Description != "" {
			if err := setting.Describe(cmd.Description); err != nil {
				return nil, err
			}
		}
	case custom_errors.CodeOf(err) == custom_errors.CodeNotFound:
		// 不存在：新建。首次写入没有「旧值」，因此不发变更事件——
		// 一条 oldValue 为空的变更记录只会让审计流水更难读。
		setting, err = entities.NewSystemSetting(key, value, cmd.Description, op.UserID)
		if err != nil {
			return nil, err
		}
	default:
		return nil, err
	}

	if err := s.settingRepo.Upsert(ctx, setting); err != nil {
		return nil, err
	}
	s.publishSetting(ctx, setting)
	return sanitizeSetting(setting), nil
}

func (s *ConfigService) Delete(ctx context.Context, op Operator, rawKey string) error {
	if err := requireAdmin(op); err != nil {
		return err
	}
	key, err := value_objects.NewSettingKey(rawKey)
	if err != nil {
		return err
	}
	return s.settingRepo.Delete(ctx, key)
}

// LoadAll 供装配根在启动时做覆盖（overlay）用。
//
// 返回的是 key -> SettingValue 的扁平映射，而不是聚合：调用方要做的事
// 就是「库里有这一项就用库里的值覆盖文件默认值」，给它聚合只是徒增噪音。
//
// 再强调一次覆盖方向：文件/环境变量打底，数据库覆盖运行期可调的那部分。
// 库里没有的键一律沿用文件值，所以配置表是空的（全新部署）时系统照常启动。
func (s *ConfigService) LoadAll(ctx context.Context) (map[string]value_objects.SettingValue, error) {
	settings, err := s.settingRepo.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]value_objects.SettingValue, len(settings))
	for k, v := range settings {
		out[k] = v.Value
	}
	return out, nil
}

// Snapshot 返回管理台用的全局配置视图，供应商密钥与密钥类配置项均已打码。
func (s *ConfigService) Snapshot(ctx context.Context, op Operator) (*ConfigSnapshot, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}
	settings, err := s.settingRepo.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	// 这里刻意不并发取两份数据：两次查询打的是同一个库、同一个连接池，
	// 并发化省不下多少时间，却多出一段需要维护的并发代码。
	providers, err := s.providerRepo.ListEnabled(ctx)
	if err != nil {
		return nil, err
	}

	rows := make([]*entities.SystemSetting, 0, len(settings))
	for _, v := range settings {
		rows = append(rows, v)
	}
	sortSettingsByKey(rows)

	return &ConfigSnapshot{
		Providers:   sanitizeAll(providers),
		Settings:    sanitizeSettings(rows),
		GeneratedAt: time.Now(),
	}, nil
}

// Reload 广播「某个域的配置该重读了」。
//
// 它不自己去改任何消费方的状态：配置中心不知道谁缓存了什么，
// 发个事件让各消费方自己回仓储重取，才不会出现「配置中心以为改好了、
// 某个组件其实还拿着旧值」这种最难查的状态。
func (s *ConfigService) Reload(ctx context.Context, op Operator, rawScope, reason string) error {
	if err := requireAdmin(op); err != nil {
		return err
	}
	scope, err := value_objects.NewSettingScope(rawScope)
	if err != nil {
		return err
	}
	evt := domain_events.NewOnConfigReloadRequested(scope, reason, op.UserID)
	return s.publisher.Publish(ctx, evt)
}

func (s *ConfigService) publishSetting(ctx context.Context, setting *entities.SystemSetting) {
	if evts := setting.GetAllPendingEvents(); len(evts) > 0 {
		_ = s.publisher.Publish(ctx, evts...)
	}
}

// sortSettingsByKey 保证快照里配置项的顺序稳定。
// map 遍历顺序随机，不排序的话管理台每次刷新顺序都在跳，
// 也没法对两次快照做有意义的 diff。
func sortSettingsByKey(rows []*entities.SystemSetting) {
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Key.String() < rows[j].Key.String()
	})
}
