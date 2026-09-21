package value_objects

import (
	"strings"
	"unicode"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Role is the user's authorization role.
type Role string

const (
	RoleUser  Role = "user"
	RoleAdmin Role = "admin"
)

// NewRole rejects unknown roles instead of silently downgrading to user:
// a config typo that quietly strips admin rights is far harder to notice than a startup error.
func NewRole(s string) (Role, error) {
	switch Role(strings.ToLower(strings.TrimSpace(s))) {
	case RoleUser:
		return RoleUser, nil
	case RoleAdmin:
		return RoleAdmin, nil
	case "":
		return RoleUser, nil
	}
	return "", custom_errors.Invalid("非法的用户角色: %s", s)
}

func (r Role) Valid() bool    { return r == RoleUser || r == RoleAdmin }
func (r Role) IsAdmin() bool  { return r == RoleAdmin }
func (r Role) String() string { return string(r) }

// Username is a validated account name.
type Username struct{ v string }

func NewUsername(s string) (Username, error) {
	s = strings.TrimSpace(s)
	if len(s) < 3 || len(s) > 32 {
		return Username{}, custom_errors.Invalid("用户名长度须在 3-32 个字符之间")
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' {
			return Username{}, custom_errors.Invalid("用户名只能包含字母、数字、下划线、短横线和点")
		}
	}
	return Username{v: s}, nil
}

// RehydrateUsername skips validation: rows already in the database are settled facts,
// and re-validating them would let one legacy row break the whole user list endpoint.
func RehydrateUsername(s string) Username { return Username{v: s} }

func (u Username) String() string { return u.v }
func (u Username) IsZero() bool   { return u.v == "" }

// PlainPassword carries an unhashed password.
//
// It is a value object rather than a bare string for two concrete reasons: the strength
// policy gets a single home, and String() masks the value so a stray log line cannot leak it.
type PlainPassword struct{ v string }

func NewPlainPassword(s string) (PlainPassword, error) {
	if len(s) < 8 {
		return PlainPassword{}, custom_errors.Invalid("密码长度至少 8 位")
	}
	if len(s) > 128 {
		return PlainPassword{}, custom_errors.Invalid("密码长度不能超过 128 位")
	}
	var hasLetter, hasDigit bool
	for _, r := range s {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	if !hasLetter || !hasDigit {
		return PlainPassword{}, custom_errors.Invalid("密码须同时包含字母和数字")
	}
	return PlainPassword{v: s}, nil
}

// RawPassword builds a password without the strength check, for comparing against an
// existing hash. Validating the *old* password under today's rules would permanently
// lock out accounts created under a laxer policy.
func RawPassword(s string) PlainPassword { return PlainPassword{v: s} }

// Expose is deliberately named to stand out in review; it is only for hashing.
func (p PlainPassword) Expose() string { return p.v }

// String masks the secret so it never reaches a log.
func (p PlainPassword) String() string { return "****" }

func (p PlainPassword) IsZero() bool { return p.v == "" }

// Preferences holds a user's default analysis settings.
type Preferences struct {
	DefaultMarket   shared_vo.Market `json:"defaultMarket"`
	DefaultDepth    int              `json:"defaultDepth"`
	DefaultAnalysts []string         `json:"defaultAnalysts"`
	RiskPreference  string           `json:"riskPreference"`
}

func DefaultPreferences() Preferences {
	return Preferences{
		DefaultMarket:   shared_vo.MarketCN,
		DefaultDepth:    3,
		DefaultAnalysts: []string{"market", "fundamentals", "news", "sentiment"},
		RiskPreference:  "neutral",
	}
}

// MergedWith returns a new value with the non-zero fields of patch applied.
// Value objects are immutable, so this returns a copy rather than mutating in place.
func (p Preferences) MergedWith(patch Preferences) Preferences {
	out := p
	if patch.DefaultDepth >= 1 && patch.DefaultDepth <= 5 {
		out.DefaultDepth = patch.DefaultDepth
	}
	if patch.DefaultMarket.Valid() {
		out.DefaultMarket = patch.DefaultMarket
	}
	if len(patch.DefaultAnalysts) > 0 {
		out.DefaultAnalysts = append([]string(nil), patch.DefaultAnalysts...)
	}
	if patch.RiskPreference != "" {
		out.RiskPreference = patch.RiskPreference
	}
	return out
}
