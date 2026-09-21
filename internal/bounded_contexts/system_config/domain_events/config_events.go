// Package domain_events 是配置中心对外广播的领域事件。
//
// 本包有一条不可协商的规则：事件里绝不出现任何密钥明文。
//
// 事件的去向天然是不受控的——进程内总线会打日志，将来换成 MQ 就会落盘、会被重投、
// 会进 DLQ，还可能被第三方消费。任何一条携带密钥的事件，等价于把密钥复制到了
// 所有这些地方，而这些地方全都没有配置中心的访问控制。
// 因此密钥轮换事件只说「哪一家的密钥换了」，不说换成了什么。
package domain_events

import (
	"encoding/json"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
)

const (
	OnProviderEnabledEventName     = "system_config.provider_enabled"
	OnProviderDisabledEventName    = "system_config.provider_disabled"
	OnProviderKeyRotatedEventName  = "system_config.provider_key_rotated"
	OnSettingChangedEventName      = "system_config.setting_changed"
	OnConfigReloadRequestedEvtName = "system_config.reload_requested"
)

// redactedPlaceholder 是事件里代替敏感值的占位符。
const redactedPlaceholder = "[REDACTED]"

// OnProviderEnabled 表示某家供应商被启用。
// 消费方（装配根）据此把客户端挂回路由表。
type OnProviderEnabled struct {
	domain_event.BaseDomainEvent
	ProviderID   uint64 `json:"providerId"`
	ProviderName string `json:"providerName"`
}

func NewOnProviderEnabled(id uint64, name string) *OnProviderEnabled {
	return &OnProviderEnabled{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		ProviderID:      id,
		ProviderName:    name,
	}
}

func (e *OnProviderEnabled) Name() string { return OnProviderEnabledEventName }

func (e *OnProviderEnabled) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnProviderDisabled 表示某家供应商被停用，消费方应把它从路由表摘掉。
type OnProviderDisabled struct {
	domain_event.BaseDomainEvent
	ProviderID   uint64 `json:"providerId"`
	ProviderName string `json:"providerName"`
}

func NewOnProviderDisabled(id uint64, name string) *OnProviderDisabled {
	return &OnProviderDisabled{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		ProviderID:      id,
		ProviderName:    name,
	}
}

func (e *OnProviderDisabled) Name() string { return OnProviderDisabledEventName }

func (e *OnProviderDisabled) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnProviderKeyRotated 表示某家供应商的密钥被轮换。
//
// 这个结构体里没有、将来也不允许加入任何密钥字段——无论是新密钥、旧密钥，
// 还是「只是掩码」。消费方需要新密钥时应当去仓储重新加载，
// 那条路径有访问控制，事件总线没有。
type OnProviderKeyRotated struct {
	domain_event.BaseDomainEvent
	ProviderID   uint64 `json:"providerId"`
	ProviderName string `json:"providerName"`
}

func NewOnProviderKeyRotated(id uint64, name string) *OnProviderKeyRotated {
	return &OnProviderKeyRotated{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		ProviderID:      id,
		ProviderName:    name,
	}
}

func (e *OnProviderKeyRotated) Name() string { return OnProviderKeyRotatedEventName }

func (e *OnProviderKeyRotated) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnSettingChanged 表示一项运行期配置被修改。
//
// 新旧值只在「确定不敏感」时才携带：配置项的值有审计价值（谁在什么时候把并发调到了 50），
// 但 market.tushare_token 这类项的值就是密钥本身。Redacted 为真时，
// 两个值字段都是占位符，消费方据此知道「变了但不告诉你变成什么」。
type OnSettingChanged struct {
	domain_event.BaseDomainEvent
	Key       string `json:"key"`
	Scope     string `json:"scope"`
	OldValue  string `json:"oldValue"`
	NewValue  string `json:"newValue"`
	Redacted  bool   `json:"redacted"`
	ChangedBy uint64 `json:"changedBy"`
}

// NewOnSettingChanged 自行判定是否需要脱敏，不把这个决定交给调用方：
// 交出去就意味着每一个新增的调用点都要重新判断一次，早晚漏掉一处。
func NewOnSettingChanged(
	key value_objects.SettingKey,
	scope value_objects.SettingScope,
	oldValue, newValue value_objects.SettingValue,
	changedBy uint64,
) *OnSettingChanged {
	e := &OnSettingChanged{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		Key:             key.String(),
		Scope:           scope.String(),
		OldValue:        oldValue.String(),
		NewValue:        newValue.String(),
		ChangedBy:       changedBy,
	}
	// 键名像密钥，或者整个域都可能装密钥，就一律脱敏。
	// 两个条件取并集是刻意的：域级判断挡住命名不规范的新配置项，
	// 键级判断挡住 sync/feature 域里偶然出现的敏感项。
	if key.LooksSecret() || scope.CarriesSecrets() {
		e.OldValue = redactedPlaceholder
		e.NewValue = redactedPlaceholder
		e.Redacted = true
	}
	return e
}

func (e *OnSettingChanged) Name() string { return OnSettingChangedEventName }

func (e *OnSettingChanged) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnConfigReloadRequested 通知各消费方重新加载某个域的配置。
//
// 事件本身不带配置内容，只带「哪个域该重读了」：让消费方回仓储取，
// 既避免在事件里搬运密钥，也避免几个消费方拿到不同版本的配置快照。
type OnConfigReloadRequested struct {
	domain_event.BaseDomainEvent
	Scope       string `json:"scope"`
	Reason      string `json:"reason"`
	RequestedBy uint64 `json:"requestedBy"`
}

func NewOnConfigReloadRequested(scope value_objects.SettingScope, reason string, requestedBy uint64) *OnConfigReloadRequested {
	return &OnConfigReloadRequested{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		Scope:           scope.String(),
		Reason:          reason,
		RequestedBy:     requestedBy,
	}
}

func (e *OnConfigReloadRequested) Name() string { return OnConfigReloadRequestedEvtName }

func (e *OnConfigReloadRequested) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}
