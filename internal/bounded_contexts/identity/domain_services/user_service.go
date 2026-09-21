package domain_services

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

type UserService struct {
	userRepo    *repositories.UserRepository
	sessionRepo *repositories.SessionRepository
	hasher      entities.PasswordHasher
	publisher   domain_event.Publisher
}

func NewUserService(
	userRepo *repositories.UserRepository,
	sessionRepo *repositories.SessionRepository,
	hasher entities.PasswordHasher,
	publisher domain_event.Publisher,
) *UserService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	return &UserService{userRepo: userRepo, sessionRepo: sessionRepo, hasher: hasher, publisher: publisher}
}

type CreateUserCommand struct {
	Username string
	Email    string
	Password string
	Role     string
}

func (s *UserService) CreateUser(ctx context.Context, operator *Claims, cmd CreateUserCommand) (*entities.User, error) {
	if err := RequireAdmin(operator); err != nil {
		return nil, err
	}

	username, err := value_objects.NewUsername(cmd.Username)
	if err != nil {
		return nil, err
	}
	password, err := value_objects.NewPlainPassword(cmd.Password)
	if err != nil {
		return nil, err
	}
	role, err := value_objects.NewRole(cmd.Role)
	if err != nil {
		return nil, err
	}

	user, err := entities.Register(username, cmd.Email, password, role, s.hasher)
	if err != nil {
		return nil, err
	}
	// No "does this username exist?" pre-check: it would be a TOCTOU window (two
	// concurrent registrations both see "free" and both proceed) and a wasted round-trip.
	// The unique index on users.username is the actual guarantee; the repository maps the
	// duplicate-key error to AlreadyExists.
	if err := s.userRepo.Create(ctx, user); err != nil {
		if custom_errors.CodeOf(err) == custom_errors.CodeAlreadyExists {
			return nil, custom_errors.AlreadyExists("用户名已被占用: %s", username.String())
		}
		return nil, err
	}
	s.publish(ctx, user)
	return user, nil
}

func (s *UserService) GetByID(ctx context.Context, id uint64) (*entities.User, error) {
	return s.userRepo.LoadByID(ctx, id)
}

func (s *UserService) List(ctx context.Context, operator *Claims, keyword string, page shared_vo.Page) ([]*entities.User, int64, error) {
	if err := RequireAdmin(operator); err != nil {
		return nil, 0, err
	}
	return s.userRepo.List(ctx, keyword, page)
}

type UpdateProfileCommand struct {
	Email       string
	Preferences *value_objects.Preferences
}

func (s *UserService) UpdateProfile(ctx context.Context, userID uint64, cmd UpdateProfileCommand) (*entities.User, error) {
	user, err := s.userRepo.LoadByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	user.UpdateProfile(cmd.Email, cmd.Preferences)
	if err := s.userRepo.Update(ctx, user); err != nil {
		return nil, err
	}
	return user, nil
}

// ChangePassword revokes every session afterwards so tokens minted with the old
// credential stop working immediately.
func (s *UserService) ChangePassword(ctx context.Context, userID uint64, oldPlain, newPlain string) error {
	fresh, err := value_objects.NewPlainPassword(newPlain)
	if err != nil {
		return err
	}
	user, err := s.userRepo.LoadByID(ctx, userID)
	if err != nil {
		return err
	}
	// The old password is only compared, never re-validated against today's policy.
	if err := user.ChangePassword(value_objects.RawPassword(oldPlain), fresh, s.hasher); err != nil {
		return err
	}
	if err := s.userRepo.Update(ctx, user); err != nil {
		return err
	}
	if err := s.sessionRepo.RevokeAllOfUser(ctx, userID); err != nil {
		return err
	}
	s.publish(ctx, user)
	return nil
}

func (s *UserService) ResetPassword(ctx context.Context, operator *Claims, targetID uint64, newPlain string) error {
	if err := RequireAdmin(operator); err != nil {
		return err
	}
	fresh, err := value_objects.NewPlainPassword(newPlain)
	if err != nil {
		return err
	}
	user, err := s.userRepo.LoadByID(ctx, targetID)
	if err != nil {
		return err
	}
	if err := user.ResetPasswordByAdmin(fresh, s.hasher); err != nil {
		return err
	}
	if err := s.userRepo.Update(ctx, user); err != nil {
		return err
	}
	if err := s.sessionRepo.RevokeAllOfUser(ctx, targetID); err != nil {
		return err
	}
	s.publish(ctx, user)
	return nil
}

// Deactivate disables an account.
//
// "Keep at least one active admin" can only be judged atomically, so the repository
// does the update and the count in one transaction and this service reacts to the result.
func (s *UserService) Deactivate(ctx context.Context, operator *Claims, targetID uint64) error {
	if err := RequireAdmin(operator); err != nil {
		return err
	}
	if operator.UserID == targetID {
		return custom_errors.Forbidden("不能停用自己的账号")
	}

	user, err := s.userRepo.LoadByID(ctx, targetID)
	if err != nil {
		return err
	}
	if err := user.Deactivate(); err != nil {
		return err
	}

	// The last-admin invariant is enforced inside the repository's UPDATE predicate, so a
	// violation rolls back rather than committing and compensating. Nothing is persisted
	// unless the write was legal.
	if err := s.userRepo.Deactivate(ctx, user); err != nil {
		return err
	}

	if err := s.sessionRepo.RevokeAllOfUser(ctx, targetID); err != nil {
		return err
	}
	s.publish(ctx, user)
	return nil
}

// EnsureBootstrapAdmin seeds the first admin when the table is empty, called at startup.
func (s *UserService) EnsureBootstrapAdmin(ctx context.Context, username, password string) error {
	n, err := s.userRepo.Count(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	name, err := value_objects.NewUsername(username)
	if err != nil {
		return err
	}
	pwd, err := value_objects.NewPlainPassword(password)
	if err != nil {
		return err
	}
	admin, err := entities.Register(name, "", pwd, value_objects.RoleAdmin, s.hasher)
	if err != nil {
		return err
	}
	if err := s.userRepo.Create(ctx, admin); err != nil {
		return err
	}
	s.publish(ctx, admin)
	return nil
}

func (s *UserService) publish(ctx context.Context, u *entities.User) {
	if evts := u.GetAllPendingEvents(); len(evts) > 0 {
		_ = s.publisher.Publish(ctx, evts...)
	}
}

// RequireAdmin is an authorization check, not a business invariant, so it belongs to the
// service layer rather than the entity.
func RequireAdmin(c *Claims) error {
	if c == nil {
		return custom_errors.Unauthorized("未登录")
	}
	if !c.Role.IsAdmin() {
		return custom_errors.Forbidden("需要管理员权限")
	}
	return nil
}
