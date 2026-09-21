package dtos

import (
	"encoding/json"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/value_objects"
)

// UserDto is the persistence shape of the users table.
// It is kept separate from the aggregate so the entity can be refactored freely
// while table changes stay bound to a migration.
type UserDto struct {
	ID              uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	Username        string     `gorm:"column:username;type:varchar(32);uniqueIndex:uk_users_username;not null"`
	Email           string     `gorm:"column:email;type:varchar(128);index:idx_users_email"`
	PasswordHash    string     `gorm:"column:password_hash;type:varchar(128);not null"`
	Role            string     `gorm:"column:role;type:varchar(16);not null;default:user"`
	Active          bool       `gorm:"column:active;not null;default:true"`
	Preferences     []byte     `gorm:"column:preferences;type:json"`
	ConcurrentLimit int        `gorm:"column:concurrent_limit;not null;default:0"`
	LastLoginAt     *time.Time `gorm:"column:last_login_at;type:datetime(3)"`
	CreatedAt       time.Time  `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime:false"`
	UpdatedAt       time.Time  `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime:false"`
}

func (UserDto) TableName() string { return "users" }

// ToDomain rebuilds the aggregate.
//
// No validation happens here: rows already stored are settled facts, and running them
// back through the validating value-object constructors would let a single legacy row
// break the entire user list endpoint. Validation belongs on the write path.
func (dto UserDto) ToDomain() *entities.User {
	prefs := value_objects.DefaultPreferences()
	if len(dto.Preferences) > 0 {
		_ = json.Unmarshal(dto.Preferences, &prefs)
	}
	return &entities.User{
		ID:              dto.ID,
		Username:        value_objects.RehydrateUsername(dto.Username),
		Email:           dto.Email,
		PasswordHash:    dto.PasswordHash,
		Role:            value_objects.Role(dto.Role),
		Active:          dto.Active,
		Preferences:     prefs,
		ConcurrentLimit: dto.ConcurrentLimit,
		LastLoginAt:     dto.LastLoginAt,
		CreatedAt:       dto.CreatedAt,
		UpdatedAt:       dto.UpdatedAt,
	}
}

func FromDomainUser(u *entities.User) *UserDto {
	prefs, err := json.Marshal(u.Preferences)
	if err != nil {
		// Preferences is plain data; a marshal failure can only be an unrecoverable
		// programming error. Degrading to a NULL column beats failing the whole write —
		// the read path falls back to defaults.
		prefs = nil
	}
	return &UserDto{
		ID:              u.ID,
		Username:        u.Username.String(),
		Email:           u.Email,
		PasswordHash:    u.PasswordHash,
		Role:            u.Role.String(),
		Active:          u.Active,
		Preferences:     prefs,
		ConcurrentLimit: u.ConcurrentLimit,
		LastLoginAt:     u.LastLoginAt,
		CreatedAt:       u.CreatedAt,
		UpdatedAt:       u.UpdatedAt,
	}
}

func ToDomainUsers(rows []*UserDto) []*entities.User {
	out := make([]*entities.User, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ToDomain())
	}
	return out
}
