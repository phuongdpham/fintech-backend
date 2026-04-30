package domain

import (
	"time"

	"github.com/google/uuid"
)

type AccountStatus string

const (
	AccountStatusActive AccountStatus = "ACTIVE"
	AccountStatusFrozen AccountStatus = "FROZEN"
	AccountStatusClosed AccountStatus = "CLOSED"
)

func (s AccountStatus) Valid() bool {
	switch s {
	case AccountStatusActive, AccountStatusFrozen, AccountStatusClosed:
		return true
	}
	return false
}

// Account is the ledger's account aggregate root.
//
// Currency is fixed at creation and immutable — a journal entry in a
// different currency is a domain error, enforced both here and by the
// composite FK in PostgreSQL. TenantID is similarly fixed; a journal
// entry whose tenant doesn't match the account's owning tenant fails
// at write time via the composite FK on (account_id, tenant_id).
type Account struct {
	ID        uuid.UUID
	TenantID  string
	Currency  Currency
	Status    AccountStatus
	CreatedAt time.Time
}

func NewAccount(id uuid.UUID, tenantID string, currency Currency, status AccountStatus, createdAt time.Time) (*Account, error) {
	if tenantID == "" {
		return nil, ErrTenantRequired
	}
	if !currency.Valid() {
		return nil, ErrInvalidCurrency
	}
	if !status.Valid() {
		return nil, ErrInvalidAccountStatus
	}
	return &Account{
		ID:        id,
		TenantID:  tenantID,
		Currency:  currency,
		Status:    status,
		CreatedAt: createdAt,
	}, nil
}

// CanPost reports whether the account is allowed to be a leg of a new
// journal entry. Closed accounts hard-fail; frozen accounts also hard-fail —
// reversal flows must reactivate or use a system account instead of
// silently bypassing the freeze.
func (a *Account) CanPost() error {
	switch a.Status {
	case AccountStatusClosed:
		return ErrAccountClosed
	case AccountStatusFrozen:
		return ErrAccountFrozen
	}
	return nil
}
