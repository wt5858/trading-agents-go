package domain_events

import (
	"encoding/json"

	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
)

const (
	OnUserRegisteredEventName  = "identity.user_registered"
	OnUserLoggedInEventName    = "identity.user_logged_in"
	OnPasswordChangedEventName = "identity.password_changed"
	OnUserDeactivatedEventName = "identity.user_deactivated"
)

type OnUserRegistered struct {
	domain_event.BaseDomainEvent
	UserID   uint64 `json:"userId"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

func NewOnUserRegistered(userID uint64, username, role string) *OnUserRegistered {
	return &OnUserRegistered{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		UserID:          userID,
		Username:        username,
		Role:            role,
	}
}

func (e *OnUserRegistered) Name() string { return OnUserRegisteredEventName }

func (e *OnUserRegistered) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

type OnUserLoggedIn struct {
	domain_event.BaseDomainEvent
	UserID   uint64 `json:"userId"`
	Username string `json:"username"`
}

func NewOnUserLoggedIn(userID uint64, username string) *OnUserLoggedIn {
	return &OnUserLoggedIn{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		UserID:          userID,
		Username:        username,
	}
}

func (e *OnUserLoggedIn) Name() string { return OnUserLoggedInEventName }

func (e *OnUserLoggedIn) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnPasswordChanged carries ByAdmin so downstream consumers can tell a self-service
// change from an admin reset — the two warrant different notifications.
type OnPasswordChanged struct {
	domain_event.BaseDomainEvent
	UserID  uint64 `json:"userId"`
	ByAdmin bool   `json:"byAdmin"`
}

func NewOnPasswordChanged(userID uint64, byAdmin bool) *OnPasswordChanged {
	return &OnPasswordChanged{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		UserID:          userID,
		ByAdmin:         byAdmin,
	}
}

func (e *OnPasswordChanged) Name() string { return OnPasswordChangedEventName }

func (e *OnPasswordChanged) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

type OnUserDeactivated struct {
	domain_event.BaseDomainEvent
	UserID   uint64 `json:"userId"`
	Username string `json:"username"`
}

func NewOnUserDeactivated(userID uint64, username string) *OnUserDeactivated {
	return &OnUserDeactivated{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		UserID:          userID,
		Username:        username,
	}
}

func (e *OnUserDeactivated) Name() string { return OnUserDeactivatedEventName }

func (e *OnUserDeactivated) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}
