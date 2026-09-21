package providers

import (
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"

	"github.com/wt5858/trading-agents-go/config"
	identity_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/domain_services"
	identity_entities "github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/entities"
	identity_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/repositories"
	"github.com/wt5858/trading-agents-go/pkg/hashx"
	"github.com/wt5858/trading-agents-go/pkg/jwtx"
)

// 本文件是身份上下文的装配清单。
//
// 配置里的散装字段（bcrypt 代价、JWT 密钥与有效期）在这里被收进各自的构造函数，
// 而不是让 Wire 去注入 int 和 time.Duration——那类基础类型在 Wire 眼里是全局唯一的，
// 两个上下文各需要一个 time.Duration 就会直接冲突。
// 凡是从配置里取值的地方，都由一个明确的 provider 函数负责。

func NewPasswordHasher(cfg *config.Config) *hashx.BcryptHasher {
	return hashx.NewBcryptHasher(cfg.Auth.BcryptCost)
}

func NewTokenIssuer(cfg *config.Config) *jwtx.Issuer {
	return jwtx.NewIssuer(
		cfg.Auth.JWTSecret,
		cfg.Auth.AccessTokenTTL,
		cfg.Auth.RefreshTokenTTL,
		cfg.App.Name,
	)
}

// NewSessionRepository 的有效期直接取自配置：会话存活时长与刷新令牌有效期
// 必须是同一个值，分开配会让「令牌还没过期但会话已经没了」这种状态成为可能。
func NewSessionRepository(rdb *redis.Client, cfg *config.Config) *identity_repo.SessionRepository {
	return identity_repo.NewSessionRepository(rdb, cfg.Auth.RefreshTokenTTL)
}

var IdentitySet = wire.NewSet(
	NewPasswordHasher,
	NewTokenIssuer,
	NewSessionRepository,
	identity_repo.NewUserRepository,
	identity_services.NewAuthService,
	identity_services.NewUserService,

	// 领域层只认自己声明的窄端口（谁来发令牌、谁来算哈希），不认 pkg 里的具体实现。
	// 这两条绑定就是端口与适配器之间的全部接缝。
	wire.Bind(new(identity_services.TokenIssuer), new(*jwtx.Issuer)),
	wire.Bind(new(identity_entities.PasswordHasher), new(*hashx.BcryptHasher)),
)
