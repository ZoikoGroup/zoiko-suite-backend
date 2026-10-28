package domain

import "time"

// ErrAccountScopeBookMismatch is returned when the accounting book named for a scoped account version does not belong to the legal
// entity named with it.
const ErrAccountScopeBookMismatch = errorString("the accounting book does not belong to the legal entity")

// AccountScopeVersion is one version of an account for a legal entity, or for one accounting book of it: the facts that may differ
// from the tenant version. A field left nil inherits from the less specific version.
type AccountScopeVersion struct {
	ScopeVersionID     string    `json:"scope_version_id"`
	AccountID          string    `json:"account_id"`
	AccountCode        string    `json:"account_code"`
	LegalEntityID      string    `json:"legal_entity_id"`
	AccountingBookID   *string   `json:"accounting_book_id,omitempty"`
	Version            int       `json:"version"`
	AccountName        *string   `json:"account_name,omitempty"`
	ParentAccountID    *string   `json:"parent_account_id,omitempty"`
	Status             *string   `json:"status,omitempty"`
	EffectiveFrom      string    `json:"effective_from"`
	EffectiveTo        *string   `json:"effective_to,omitempty"`
	ChangeReason       string    `json:"change_reason"`
	EvidenceRefs       []string  `json:"evidence_refs,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	CreatedByPrincipal string    `json:"created_by_principal_id"`
}

// CreateAccountScopeVersionRequest records a scoped version. At least one of the name, the parent and the status is given.
type CreateAccountScopeVersionRequest struct {
	LegalEntityID     string   `json:"legal_entity_id"`
	AccountingBookID  string   `json:"accounting_book_id,omitempty"`
	AccountName       *string  `json:"account_name,omitempty"`
	ParentAccountCode *string  `json:"parent_account_code,omitempty"`
	Status            *string  `json:"status,omitempty"`
	EffectiveFrom     string   `json:"effective_from,omitempty"`
	Reason            string   `json:"reason"`
	EvidenceRefs      []string `json:"evidence_refs,omitempty"`
}

// ScopedAccount is an account as it stands for an entity or a book on a date: the tenant version with the most specific scoped version
// laid over it. The scope fields say where each answer comes from: BOOK, ENTITY or TENANT.
type ScopedAccount struct {
	AccountCode      string      `json:"account_code"`
	AccountName      string      `json:"account_name"`
	AccountType      AccountType `json:"account_type"`
	Status           string      `json:"status"`
	ParentAccountID  *string     `json:"parent_account_id,omitempty"`
	LegalEntityID    string      `json:"legal_entity_id"`
	AccountingBookID string      `json:"accounting_book_id,omitempty"`
	AsOf             string      `json:"as_of"`
	TenantVersion    int64       `json:"tenant_version"`
	NameScope        string      `json:"name_scope"`
	ParentScope      string      `json:"parent_scope"`
	StatusScope      string      `json:"status_scope"`
	ScopeVersions    []int       `json:"scope_versions,omitempty"`
}
