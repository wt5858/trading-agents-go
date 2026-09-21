package entities

import (
	"strings"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

const settingDescriptionMaxLen = 255

// SystemSetting 是一项运行期可调配置，配置中心的另一个聚合根。
//
// 没有自增 ID：Key 本身就是自然主键，再加一个代理键只会让「同一个键存了两行」
// 这种脏数据在数据库层面变得可能。
type SystemSetting struct {
	domain_event.EventRecorder

	Key         value_objects.SettingKey
	Scope       value_objects.SettingScope
	Value       value_objects.SettingValue
	Description string
	// UpdatedBy 是最后一次修改者的用户 ID。
	// 存 ID 而不是引用 identity 上下文的 User：跨上下文只传标识，不共享类型。
	UpdatedBy uint64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewSystemSetting 新建一项配置。
//
// Scope 从 Key 的第一段推导而不是让调用方另传：两个入参就有两处可能填错，
// 而「键叫 llm.timeout 却归在 market 域」这种错位在按域查询时才暴露，
// 那时已经有别的配置项依赖这个错误的归类了。
func NewSystemSetting(
	key value_objects.SettingKey,
	value value_objects.SettingValue,
	description string,
	updatedBy uint64,
) (*SystemSetting, error) {
	if key.IsZero() {
		return nil, custom_errors.Invalid("配置键不能为空")
	}
	scope := key.Scope()
	if !scope.Valid() {
		return nil, custom_errors.Invalid("配置键的域不合法: %s", key.String())
	}
	description = strings.TrimSpace(description)
	if len(description) > settingDescriptionMaxLen {
		return nil, custom_errors.Invalid("配置说明不能超过 %d 个字符", settingDescriptionMaxLen)
	}

	now := time.Now()
	return &SystemSetting{
		Key:         key,
		Scope:       scope,
		Value:       value,
		Description: description,
		UpdatedBy:   updatedBy,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// Change 改值。
//
// 值没变时是一次无副作用的空操作，不报错也不发事件：配置保存天然是幂等的
// （管理台一次保存会把整页配置全部提交回来），把「值和原来一样」当成冲突
// 会让正常的保存流程动不动就失败。这与 Enable/Disable 的取舍不同——
// 那里的重复调用意味着有人的状态变更被覆盖了，值得让调用方看见。
func (s *SystemSetting) Change(newValue value_objects.SettingValue, by uint64) error {
	if s.Value.Equal(newValue) {
		return nil
	}
	old := s.Value
	s.Value = newValue
	s.UpdatedBy = by
	s.UpdatedAt = time.Now()
	// 事件构造函数自行判断要不要脱敏，这里不做决定也不需要知道结论。
	s.AddDomainEvent(domain_events.NewOnSettingChanged(s.Key, s.Scope, old, newValue, by))
	return nil
}

// Describe 更新说明文字。说明只影响管理台展示，不产生领域事件。
func (s *SystemSetting) Describe(description string) error {
	description = strings.TrimSpace(description)
	if len(description) > settingDescriptionMaxLen {
		return custom_errors.Invalid("配置说明不能超过 %d 个字符", settingDescriptionMaxLen)
	}
	s.Description = description
	s.UpdatedAt = time.Now()
	return nil
}

// IsSecret 表示这一项的值不能原样展示给管理台。
//
// 只按键名判定，不像领域事件那样把整个 llm/market 域都算敏感：
// 两边的代价不对称。事件流向不受控（日志、MQ、DLQ、第三方消费方），
// 宁可多脱敏；管理台是有访问控制的运维界面，把 llm.default_model
// 也打成星号会让它彻底没法用。
func (s *SystemSetting) IsSecret() bool { return s.Key.LooksSecret() }

// MaskedValue 返回可安全展示的值：敏感项复用 SecretValue 的掩码规则，
// 保证管理台上「llm 的密钥」和「market 的 token」看起来是同一种东西。
func (s *SystemSetting) MaskedValue() string {
	if !s.IsSecret() {
		return s.Value.String()
	}
	return value_objects.RehydrateSecret(s.Value.String()).Masked()
}
