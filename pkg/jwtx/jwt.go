// Package jwtx implements the identity context's TokenIssuer using JWT.
package jwtx

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// tokenKind separates access from refresh tokens. They share a signing key, so this
// claim is what stops a refresh token from being replayed as an access token.
type tokenKind string

const (
	kindAccess  tokenKind = "access"
	kindRefresh tokenKind = "refresh"
)

type claims struct {
	jwt.RegisteredClaims
	UserID    uint64    `json:"uid"`
	Username  string    `json:"usr"`
	Role      string    `json:"rol"`
	SessionID string    `json:"sid"`
	Kind      tokenKind `json:"knd"`
}

type Issuer struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	issuer     string
}

var _ domain_services.TokenIssuer = (*Issuer)(nil)

func NewIssuer(secret string, accessTTL, refreshTTL time.Duration, issuerName string) *Issuer {
	if accessTTL <= 0 {
		accessTTL = 2 * time.Hour
	}
	if refreshTTL <= 0 {
		refreshTTL = 30 * 24 * time.Hour
	}
	return &Issuer{secret: []byte(secret), accessTTL: accessTTL, refreshTTL: refreshTTL, issuer: issuerName}
}

func (i *Issuer) Issue(u *entities.User, sessionID string) (domain_services.TokenPair, error) {
	access, err := i.sign(u, sessionID, kindAccess, i.accessTTL)
	if err != nil {
		return domain_services.TokenPair{}, err
	}
	refresh, err := i.sign(u, sessionID, kindRefresh, i.refreshTTL)
	if err != nil {
		return domain_services.TokenPair{}, err
	}
	return domain_services.TokenPair{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		ExpiresIn:    int64(i.accessTTL.Seconds()),
	}, nil
}

func (i *Issuer) sign(u *entities.User, sessionID string, kind tokenKind, ttl time.Duration) (string, error) {
	now := time.Now()
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.issuer,
			Subject:   u.Username.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		UserID:    u.ID,
		Username:  u.Username.String(),
		Role:      u.Role.String(),
		SessionID: sessionID,
		Kind:      kind,
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(i.secret)
	if err != nil {
		return "", custom_errors.Internal("签发令牌失败").Wrap(err)
	}
	return token, nil
}

func (i *Issuer) ParseAccess(token string) (*domain_services.Claims, error) {
	return i.parse(token, kindAccess)
}

func (i *Issuer) ParseRefresh(token string) (*domain_services.Claims, error) {
	return i.parse(token, kindRefresh)
}

func (i *Issuer) parse(token string, want tokenKind) (*domain_services.Claims, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		// Pin the algorithm explicitly; accepting whatever the header claims is how
		// alg=none downgrade attacks get in.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("非预期的签名算法")
		}
		return i.secret, nil
	}, jwt.WithIssuer(i.issuer))

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, custom_errors.Unauthorized("令牌已过期")
		}
		return nil, custom_errors.Unauthorized("令牌无效")
	}
	if c.Kind != want {
		return nil, custom_errors.Unauthorized("令牌类型不匹配")
	}
	return &domain_services.Claims{
		UserID:    c.UserID,
		Username:  c.Username,
		Role:      value_objects.Role(c.Role),
		SessionID: c.SessionID,
	}, nil
}
