package repositories

import (
	"context"
	"errors"
	"strings"

	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// UserRepository persists the User aggregate. This is the only layer that knows GORM
// exists and the only layer allowed to open a transaction.
type UserRepository struct {
	db *gorm.DB
}

func NewUserRepository(db *gorm.DB) *UserRepository {
	return &UserRepository{db: db}
}

func (repo *UserRepository) Create(ctx context.Context, u *entities.User) error {
	dto := dtos.FromDomainUser(u)
	if err := repo.db.WithContext(ctx).Create(dto).Error; err != nil {
		return translate(err, "创建用户")
	}
	// Backfill the generated id; the caller signs a token with it right after.
	// Goes through the aggregate rather than poking the field directly: the registration
	// event raised back in Register() still carries UserID 0, and only the entity knows
	// which of its pending events need the id patched in once it exists.
	u.AssignPersistedID(dto.ID)
	return nil
}

func (repo *UserRepository) Update(ctx context.Context, u *entities.User) error {
	dto := dtos.FromDomainUser(u)
	res := repo.db.WithContext(ctx).Model(&dtos.UserDto{}).Where("id = ?", dto.ID).Updates(map[string]any{
		"email":            dto.Email,
		"password_hash":    dto.PasswordHash,
		"role":             dto.Role,
		"active":           dto.Active,
		"preferences":      dto.Preferences,
		"concurrent_limit": dto.ConcurrentLimit,
		"last_login_at":    dto.LastLoginAt,
		"updated_at":       dto.UpdatedAt,
	})
	if res.Error != nil {
		return translate(res.Error, "更新用户")
	}
	// RowsAffected == 0 is ambiguous: the row may be missing, or the values may simply be
	// unchanged (a repeat login within the same millisecond). Pay for the extra lookup
	// only on that branch so the normal path stays a single statement.
	if res.RowsAffected == 0 {
		var n int64
		if err := repo.db.WithContext(ctx).Model(&dtos.UserDto{}).Where("id = ?", dto.ID).Count(&n).Error; err != nil {
			return translate(err, "更新用户")
		}
		if n == 0 {
			return custom_errors.NotFound("用户不存在: %d", dto.ID)
		}
	}
	return nil
}

func (repo *UserRepository) Delete(ctx context.Context, id uint64) error {
	res := repo.db.WithContext(ctx).Delete(&dtos.UserDto{}, id)
	if res.Error != nil {
		return translate(res.Error, "删除用户")
	}
	if res.RowsAffected == 0 {
		return custom_errors.NotFound("用户不存在: %d", id)
	}
	return nil
}

func (repo *UserRepository) LoadByID(ctx context.Context, id uint64) (*entities.User, error) {
	var dto dtos.UserDto
	if err := repo.db.WithContext(ctx).First(&dto, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, custom_errors.NotFound("用户不存在: %d", id)
		}
		return nil, translate(err, "查询用户")
	}
	return dto.ToDomain(), nil
}

// LoadByUsername returns NotFound when absent; the login flow deliberately collapses
// that into the same message as a wrong password so the endpoint cannot be used to
// enumerate usernames.
func (repo *UserRepository) LoadByUsername(ctx context.Context, username value_objects.Username) (*entities.User, error) {
	var dto dtos.UserDto
	if err := repo.db.WithContext(ctx).Where("username = ?", username.String()).First(&dto).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, custom_errors.NotFound("用户不存在: %s", username.String())
		}
		return nil, translate(err, "查询用户")
	}
	return dto.ToDomain(), nil
}

func (repo *UserRepository) ExistsByUsername(ctx context.Context, username value_objects.Username) (bool, error) {
	var n int64
	if err := repo.db.WithContext(ctx).Model(&dtos.UserDto{}).
		Where("username = ?", username.String()).Count(&n).Error; err != nil {
		return false, translate(err, "检查用户名")
	}
	return n > 0, nil
}

func (repo *UserRepository) List(ctx context.Context, keyword string, page shared_vo.Page) ([]*entities.User, int64, error) {
	q := repo.db.WithContext(ctx).Model(&dtos.UserDto{})
	if kw := strings.TrimSpace(keyword); kw != "" {
		like := "%" + escapeLike(kw) + "%"
		q = q.Where("username LIKE ? ESCAPE '\\\\' OR email LIKE ? ESCAPE '\\\\'", like, like)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, translate(err, "统计用户")
	}
	if total == 0 {
		// An allocated empty slice, not nil: a nil slice marshals to JSON `null`,
		// and every other paginated repo here returns `[]`. Clients should not have
		// to handle two shapes for "no results".
		return []*entities.User{}, 0, nil
	}

	var rows []*dtos.UserDto
	if err := q.Order("id DESC").Offset(page.Offset()).Limit(page.Limit()).Find(&rows).Error; err != nil {
		return nil, 0, translate(err, "查询用户列表")
	}
	return dtos.ToDomainUsers(rows), total, nil
}

func (repo *UserRepository) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := repo.db.WithContext(ctx).Model(&dtos.UserDto{}).Count(&n).Error; err != nil {
		return 0, translate(err, "统计用户")
	}
	return n, nil
}

// Deactivate persists the deactivation, enforcing "the system keeps at least one active
// admin" as part of the same statement.
//
// The guard is a WHERE predicate rather than a separate count because a read-then-write
// pair is a TOCTOU hole: two concurrent requests deactivating the last two admins would
// both observe count == 2 and both proceed, leaving zero. Expressing it as a condition on
// the UPDATE makes the check and the act a single atomic operation, which is exactly the
// DB-level enforcement the invariant needs.
//
// `active = true` in the predicate additionally makes the call idempotent: a retry after a
// successful deactivation affects zero rows instead of rewriting updated_at.
func (repo *UserRepository) Deactivate(ctx context.Context, u *entities.User) error {
	err := repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		dto := dtos.FromDomainUser(u)

		q := tx.Model(&dtos.UserDto{}).Where("id = ? AND active = ?", dto.ID, true)
		if u.IsAdmin() {
			// MySQL forbids referencing the table being updated in a bare subquery,
			// hence the derived-table wrapper.
			q = q.Where(`(SELECT cnt FROM (SELECT COUNT(*) AS cnt FROM users WHERE role = ? AND active = ?) AS t) > 1`,
				value_objects.RoleAdmin.String(), true)
		}

		res := q.Updates(map[string]any{"active": false, "updated_at": dto.UpdatedAt})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected > 0 {
			return nil
		}

		// Zero rows is ambiguous; one read inside the same transaction tells us which
		// precondition failed so the caller gets an actionable message.
		var current dtos.UserDto
		if err := tx.First(&current, dto.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return custom_errors.NotFound("用户不存在: %d", dto.ID)
			}
			return err
		}
		if !current.Active {
			return custom_errors.Conflict("账号已处于停用状态")
		}
		return custom_errors.Conflict("系统必须保留至少一个启用状态的管理员")
	})
	if err != nil {
		var de *custom_errors.Error
		if errors.As(err, &de) {
			return de
		}
		return translate(err, "停用用户")
	}
	return nil
}
