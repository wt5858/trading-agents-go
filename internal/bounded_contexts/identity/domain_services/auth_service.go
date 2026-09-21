package domain_services

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/pkg/idx"
)

// TokenPair is the credential pair handed to a client.
type TokenPair struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	TokenType    string `json:"tokenType"`
	ExpiresIn    int64  `json:"expiresIn"`
}

// Claims is the identity decoded from a token.
type Claims struct {
	UserID    uint64
	Username  string
	Role      value_objects.Role
	SessionID string
}

// TokenIssuer signs and verifies tokens. Declared here, on the consumer side, so the
// service stays unaware of JWT; the implementation lives in pkg/jwtx.
type TokenIssuer interface {
	Issue(u *entities.User, sessionID string) (TokenPair, error)
	ParseAccess(token string) (*Claims, error)
	ParseRefresh(token string) (*Claims, error)
}

type AuthService struct {
	userRepo    *repositories.UserRepository
	sessionRepo *repositories.SessionRepository
	tokens      TokenIssuer
	hasher      entities.PasswordHasher
	publisher   domain_event.Publisher
}

func NewAuthService(
	userRepo *repositories.UserRepository,
	sessionRepo *repositories.SessionRepository,
	tokens TokenIssuer,
	hasher entities.PasswordHasher,
	publisher domain_event.Publisher,
) *AuthService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	return &AuthService{
		userRepo:    userRepo,
		sessionRepo: sessionRepo,
		tokens:      tokens,
		hasher:      hasher,
		publisher:   publisher,
	}
}

type LoginResult struct {
	Tokens TokenPair
	User   *entities.User
}

// Login authenticates a user and opens a session.
//
// "No such user" and "wrong password" collapse into one message on purpose: telling them
// apart turns the endpoint into a username oracle.
func (s *AuthService) Login(ctx context.Context, rawUsername, rawPassword string) (*LoginResult, error) {
	username, err := value_objects.NewUsername(rawUsername)
	if err != nil {
		return nil, custom_errors.Unauthorized("用户名或密码错误")
	}

	user, err := s.userRepo.LoadByUsername(ctx, username)
	if err != nil {
		if custom_errors.CodeOf(err) == custom_errors.CodeNotFound {
			return nil, custom_errors.Unauthorized("用户名或密码错误")
		}
		return nil, err
	}

	// The stored password predates today's strength policy, so compare it raw.
	if err := user.Authenticate(value_objects.RawPassword(rawPassword), s.hasher); err != nil {
		return nil, err
	}

	sessionID := idx.SessionID()
	tokens, err := s.tokens.Issue(user, sessionID)
	if err != nil {
		return nil, err
	}
	if err := s.sessionRepo.Save(ctx, sessionID, user.ID); err != nil {
		return nil, err
	}
	if err := s.userRepo.Update(ctx, user); err != nil {
		return nil, err
	}

	// Events go out only after the decision is durable.
	s.publish(ctx, user)
	return &LoginResult{Tokens: tokens, User: user}, nil
}

// Refresh exchanges a refresh token for a fresh pair. The session must still exist,
// which is what makes logout and admin deactivation able to actually revoke a token.
func (s *AuthService) Refresh(ctx context.Context, refreshToken string) (*LoginResult, error) {
	claims, err := s.tokens.ParseRefresh(refreshToken)
	if err != nil {
		return nil, err
	}
	ok, err := s.sessionRepo.Exists(ctx, claims.SessionID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, custom_errors.Unauthorized("会话已失效，请重新登录")
	}

	user, err := s.userRepo.LoadByID(ctx, claims.UserID)
	if err != nil {
		return nil, err
	}
	if !user.Active {
		// Clear the stragglers so the refresh token stops working immediately
		// rather than at the end of its TTL.
		_ = s.sessionRepo.RevokeAllOfUser(ctx, user.ID)
		return nil, custom_errors.Forbidden("账号已被停用")
	}

	tokens, err := s.tokens.Issue(user, claims.SessionID)
	if err != nil {
		return nil, err
	}
	return &LoginResult{Tokens: tokens, User: user}, nil
}

func (s *AuthService) Logout(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	return s.sessionRepo.Revoke(ctx, sessionID)
}

// Authenticate validates an access token; used by the HTTP auth middleware.
func (s *AuthService) Authenticate(ctx context.Context, accessToken string) (*Claims, error) {
	claims, err := s.tokens.ParseAccess(accessToken)
	if err != nil {
		return nil, err
	}
	ok, err := s.sessionRepo.Exists(ctx, claims.SessionID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, custom_errors.Unauthorized("会话已失效，请重新登录")
	}
	return claims, nil
}

func (s *AuthService) publish(ctx context.Context, u *entities.User) {
	if evts := u.GetAllPendingEvents(); len(evts) > 0 {
		_ = s.publisher.Publish(ctx, evts...)
	}
}
