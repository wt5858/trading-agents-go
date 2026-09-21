// Package dtos 是配置中心的持久化对象与映射。
//
// 映射函数就写在 DTO 旁边，不单开 mapper 包：DTO 的字段和它的映射规则是同一件事，
// 拆开只会让加一个字段变成改两个文件、漏一个就静默丢数据。
//
// # 关于「哪些配置可以进数据库」
//
// 本包只定义两张表：llm_providers 与 system_settings。它们装的都是运行期可调参数。
// 数据库连接、Redis 地址、Mongo URI、HTTP 端口、JWT 密钥、bcrypt cost
// 这些基础设施参数永远留在 config/config.go 的文件与环境变量里，
// 绝不能在这里加列、也不能作为 system_settings 的一行存在。
// 原因很直接：这两张表本身就住在 MySQL 里，把 mysql.password 存进去意味着
// 一行写坏的配置会让服务连不上存着这行配置的库——一个自己锁死自己的死结，
// 唯一的解法是登到数据库上手改，而那时服务已经起不来了。
package dtos

import (
	"encoding/json"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/value_objects"
)

// LLMProviderDto 对应 llm_providers 表。
//
// # 关于 api_key 列
//
// 密钥目前是明文落库的。这在当前阶段是可接受的——库本身在内网、
// 有独立账号体系——但它不等于安全：任何一次 SELECT *、一次慢查询日志、
// 一次备份文件外泄，密钥就跟着出去了。
//
// 上生产前必须补上静态加密（KMS 信封加密：主密钥在 KMS，数据密钥加密后随行存储，
// 本列存密文而不是明文）。这件事明确不在本次改动的范围内，
// 在这里写清楚而不是默默留个 TODO，是因为「以为已经加密了」比「知道还没加密」危险得多。
type LLMProviderDto struct {
	ID uint64 `gorm:"column:id;primaryKey;autoIncrement"`
	// 唯一索引不只是查询优化：它是「同名供应商只能有一个」这条不变式的实际执行者。
	// 仓储不做「先查再插」，直接依赖它并把 1062 翻译成 AlreadyExists。
	Name    string `gorm:"column:name;type:varchar(32);uniqueIndex:uk_llm_providers_name;not null"`
	Kind    string `gorm:"column:kind;type:varchar(32);not null"`
	BaseURL string `gorm:"column:base_url;type:varchar(255);not null;default:''"`
	APIKey  string `gorm:"column:api_key;type:varchar(512);not null;default:''"`
	Models  []byte `gorm:"column:models;type:json"`
	// 启用态的读取是热路径（每次装配路由都要扫一遍），
	// (enabled, priority) 联合索引让它走索引而不是全表。
	// enabled 必须是第一列：只给 priority 建索引的话，过滤条件根本用不上它。
	Enabled   bool      `gorm:"column:enabled;not null;default:false;index:idx_llm_providers_enabled_priority,priority:1"`
	Priority  int       `gorm:"column:priority;not null;default:0;index:idx_llm_providers_enabled_priority,priority:2"`
	CreatedAt time.Time `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime:false"`
}

func (LLMProviderDto) TableName() string { return "llm_providers" }

// ToDomain 重建聚合。
//
// 全程走 Rehydrate 系列构造函数，不做校验：库里的行是既成事实。
// 拿写入期的规则去卡读路径，只会让一条历史脏数据把整个供应商列表接口打挂，
// 连「进管理台把它改对」这条自救路径都一起堵死。
func (dto LLMProviderDto) ToDomain() *entities.LLMProviderConfig {
	var models []string
	if len(dto.Models) > 0 {
		// 解析失败就当没有模型：这条记录会被 Usable() 判为不可用，
		// 不会混进路由表，比让整个列表查询失败合理。
		_ = json.Unmarshal(dto.Models, &models)
	}
	return &entities.LLMProviderConfig{
		ID:        dto.ID,
		Name:      value_objects.RehydrateProviderName(dto.Name),
		Kind:      value_objects.RehydrateProviderKind(dto.Kind),
		BaseURL:   value_objects.RehydrateEndpointURL(dto.BaseURL),
		APIKey:    value_objects.RehydrateSecret(dto.APIKey),
		Models:    models,
		Enabled:   dto.Enabled,
		Priority:  dto.Priority,
		CreatedAt: dto.CreatedAt,
		UpdatedAt: dto.UpdatedAt,
	}
}

// FromDomainLLMProvider 把聚合转成持久化对象。
//
// 这里调用了 SecretValue.Expose()。这是全仓库两处合法调用之一：
// 要把密钥存进数据库，就必须拿到明文，没有别的写法。
// 关键在于调用点只有这一个，且它的下一行就是写库——
// 明文不会流向任何其他地方，更不会被返回给上层。
func FromDomainLLMProvider(p *entities.LLMProviderConfig) *LLMProviderDto {
	models, err := json.Marshal(p.Models)
	if err != nil {
		// Models 是纯字符串切片，序列化失败只可能是不可恢复的程序错误。
		// 退化成 NULL 列好过让整次写入失败：读路径会把它当成「没有模型」。
		models = nil
	}
	return &LLMProviderDto{
		ID:        p.ID,
		Name:      p.Name.String(),
		Kind:      p.Kind.String(),
		BaseURL:   p.BaseURL.String(),
		APIKey:    p.APIKey.Expose(),
		Models:    models,
		Enabled:   p.Enabled,
		Priority:  p.Priority,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
}

func ToDomainLLMProviders(rows []*LLMProviderDto) []*entities.LLMProviderConfig {
	out := make([]*entities.LLMProviderConfig, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ToDomain())
	}
	return out
}
