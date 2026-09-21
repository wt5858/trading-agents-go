// Package hashx 提供口令哈希实现，满足 identity.PasswordHasher 端口。
package hashx

import "golang.org/x/crypto/bcrypt"

type BcryptHasher struct{ cost int }

// NewBcryptHasher cost 超出 bcrypt 允许区间时退回默认值。
func NewBcryptHasher(cost int) *BcryptHasher {
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		cost = bcrypt.DefaultCost
	}
	return &BcryptHasher{cost: cost}
}

func (h *BcryptHasher) Hash(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), h.cost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (h *BcryptHasher) Verify(hashed, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hashed), []byte(plain)) == nil
}
