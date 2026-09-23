package entities

import (
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// PasswordHasher is the hashing contract the User aggregate depends on.
// It lives here, not in the repository layer, because authentication is a business
// invariant of User; bcrypt itself is an infrastructure detail.
type PasswordHasher interface {
	Hash(plain string) (string, error)
	Verify(hashed, plain string) bool
}

// User is the aggregate root of the identity context.
//
// PasswordHash stays out of reach of casual mutation by convention rather than by
// unexporting it: every write path goes through ChangePassword / ResetPasswordByAdmin,
// which is where the strength policy and old-password check live.
type User struct {
	domain_event.EventRecorder

	ID              uint64
	Username        value_objects.Username
	Email           string
	PasswordHash    string
	Role            value_objects.Role
	Active          bool
	Preferences     value_objects.Preferences
	ConcurrentLimit int // 0 means "fall back to the system default"
	LastLoginAt     *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Register is the only way a user enters the system.
func Register(
	username value_objects.Username,
	email string,
	password value_objects.PlainPassword,
	role value_objects.Role,
	hasher PasswordHasher,
) (*User, error) {
	if username.IsZero() {
		return nil, custom_errors.Invalid("用户名不能为空")
	}
	if password.IsZero() {
		return nil, custom_errors.Invalid("密码不能为空")
	}
	if !role.Valid() {
		role = value_objects.RoleUser
	}
	hash, err := hasher.Hash(password.Expose())
	if err != nil {
		return nil, custom_errors.Internal("口令哈希失败").Wrap(err)
	}

	now := time.Now()
	u := &User{
		Username:     username,
		Email:        email,
		PasswordHash: hash,
		Role:         role,
		Active:       true,
		Preferences:  value_objects.DefaultPreferences(),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	// UserID 此刻只能填 0：主键是自增的，要等仓储插入之后才存在。
	// 它由 AssignPersistedID 在回填 ID 时一并补上——那是这条链路上第一个
	// 知道真实 ID 的地方。
	u.AddDomainEvent(domain_events.NewOnUserRegistered(0, username.String(), role.String()))
	return u, nil
}

// AssignPersistedID 记下数据库分配的主键，并补齐那些在建号时还拿不到 ID 的事件。
//
// # 为什么这件事不能留给调用方做
//
// 注册事件是在 Register 里记下的，而那时主键还不存在（自增列要插入之后才有值），
// 于是它带着 UserID: 0 出生。仓储插入后会回填 u.ID，但**事件不会自己跟着变**——
// 结果是每一个下游消费者都收到 userId=0，而这个错误完全静默：事件发出去了、
// 处理器也跑了，只是它永远找不到那个用户。
//
// 补齐的动作放在这里而不是在领域服务里，是因为「谁知道 ID 了」和「谁负责让聚合
// 自洽」应当是同一处。领域服务有两个建号入口（CreateUser 与 EnsureBootstrapAdmin），
// 让它们各自记得补一次，就是等着其中一个忘记。
func (u *User) AssignPersistedID(id uint64) {
	u.ID = id
	for _, e := range u.PendingEvents() {
		if reg, ok := e.(*domain_events.OnUserRegistered); ok {
			reg.UserID = id
		}
	}
}

func (u *User) IsAdmin() bool { return u.Role.IsAdmin() }

// Authenticate verifies the password. A deactivated account is refused even with the
// correct password — keeping that rule here means no caller can forget to check it.
func (u *User) Authenticate(password value_objects.PlainPassword, hasher PasswordHasher) error {
	if !u.Active {
		return custom_errors.Forbidden("账号已被停用")
	}
	if !hasher.Verify(u.PasswordHash, password.Expose()) {
		return custom_errors.Unauthorized("用户名或密码错误")
	}
	now := time.Now()
	u.LastLoginAt = &now
	u.AddDomainEvent(domain_events.NewOnUserLoggedIn(u.ID, u.Username.String()))
	return nil
}

// ChangePassword is the self-service path and requires the current password.
func (u *User) ChangePassword(old, fresh value_objects.PlainPassword, hasher PasswordHasher) error {
	if !hasher.Verify(u.PasswordHash, old.Expose()) {
		return custom_errors.Unauthorized("原密码不正确")
	}
	return u.resetTo(fresh, hasher, false)
}

// ResetPasswordByAdmin skips the old-password check; the caller is responsible for
// having established admin authority first.
func (u *User) ResetPasswordByAdmin(fresh value_objects.PlainPassword, hasher PasswordHasher) error {
	return u.resetTo(fresh, hasher, true)
}

func (u *User) resetTo(fresh value_objects.PlainPassword, hasher PasswordHasher, byAdmin bool) error {
	if fresh.IsZero() {
		return custom_errors.Invalid("新密码不能为空")
	}
	hash, err := hasher.Hash(fresh.Expose())
	if err != nil {
		return custom_errors.Internal("口令哈希失败").Wrap(err)
	}
	u.PasswordHash = hash
	u.UpdatedAt = time.Now()
	u.AddDomainEvent(domain_events.NewOnPasswordChanged(u.ID, byAdmin))
	return nil
}

// UpdateProfile applies a partial patch; preferences merge by the value object's own rule.
func (u *User) UpdateProfile(email string, prefs *value_objects.Preferences) {
	if email != "" {
		u.Email = email
	}
	if prefs != nil {
		u.Preferences = u.Preferences.MergedWith(*prefs)
	}
	u.UpdatedAt = time.Now()
}

// Deactivate raises an event the caller uses to revoke every live session.
func (u *User) Deactivate() error {
	if !u.Active {
		return custom_errors.Conflict("账号已处于停用状态")
	}
	u.Active = false
	u.UpdatedAt = time.Now()
	u.AddDomainEvent(domain_events.NewOnUserDeactivated(u.ID, u.Username.String()))
	return nil
}

func (u *User) Activate() error {
	if u.Active {
		return custom_errors.Conflict("账号已处于启用状态")
	}
	u.Active = true
	u.UpdatedAt = time.Now()
	return nil
}

func (u *User) SetConcurrentLimit(n int) error {
	if n < 0 {
		return custom_errors.Invalid("并发额度不能为负数")
	}
	u.ConcurrentLimit = n
	u.UpdatedAt = time.Now()
	return nil
}

// EffectiveConcurrentLimit resolves the per-user override against the system default.
func (u *User) EffectiveConcurrentLimit(systemDefault int) int {
	if u.ConcurrentLimit > 0 {
		return u.ConcurrentLimit
	}
	return systemDefault
}
