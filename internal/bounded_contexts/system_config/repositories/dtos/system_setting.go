package dtos

import (
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/value_objects"
)

// SystemSettingDto 对应 system_settings 表。
//
// 列名用 setting_key 而不是 key：KEY 是 MySQL 的保留字，叫 key 的话
// 每一条手写 SQL 都得记得加反引号，迟早有人忘记。
//
// 主键是键本身，没有自增 ID：同一个配置键存成两行是不可能出现的状态，
// 那就让数据库来保证，而不是靠应用层小心。
type SystemSettingDto struct {
	Key   string `gorm:"column:setting_key;type:varchar(128);primaryKey"`
	Scope string `gorm:"column:scope;type:varchar(16);not null;index:idx_system_settings_scope"`
	// text 而不是 varchar：配置值里出现一段 JSON（比如某个数据源的字段映射）
	// 是完全可能的，被长度截断的配置比报错更难发现。
	Value       string    `gorm:"column:value;type:text"`
	Description string    `gorm:"column:description;type:varchar(255);not null;default:''"`
	UpdatedBy   uint64    `gorm:"column:updated_by;not null;default:0"`
	CreatedAt   time.Time `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime:false"`
	UpdatedAt   time.Time `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime:false"`
}

func (SystemSettingDto) TableName() string { return "system_settings" }

// ToDomain 重建聚合，同样不做校验，理由见 LLMProviderDto.ToDomain。
func (dto SystemSettingDto) ToDomain() *entities.SystemSetting {
	return &entities.SystemSetting{
		Key:         value_objects.RehydrateSettingKey(dto.Key),
		Scope:       value_objects.RehydrateSettingScope(dto.Scope),
		Value:       value_objects.RehydrateSettingValue(dto.Value),
		Description: dto.Description,
		UpdatedBy:   dto.UpdatedBy,
		CreatedAt:   dto.CreatedAt,
		UpdatedAt:   dto.UpdatedAt,
	}
}

func FromDomainSystemSetting(s *entities.SystemSetting) *SystemSettingDto {
	return &SystemSettingDto{
		Key:         s.Key.String(),
		Scope:       s.Scope.String(),
		Value:       s.Value.String(),
		Description: s.Description,
		UpdatedBy:   s.UpdatedBy,
		CreatedAt:   s.CreatedAt,
		UpdatedAt:   s.UpdatedAt,
	}
}

func ToDomainSystemSettings(rows []*SystemSettingDto) []*entities.SystemSetting {
	out := make([]*entities.SystemSetting, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ToDomain())
	}
	return out
}
