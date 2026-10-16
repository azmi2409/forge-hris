// Package store implements forge-hris component 5: the payroll engine
// store layer (runs, pay lines, item presets, sync items, previews).
//
// The pure calculation side (statutory rule packs, gross->net math) lives
// in the payroll package and never touches storage; this file only
// references payroll.PresetKind, payroll.Preset, payroll.StatutorySplits
// and payroll.PayLineItem per the shared Worker A/B contract. All money is
// int64 minor units (sen/cents) everywhere.

package store

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/azmi2409/forge-hris/payroll"
)

// PayrollRunStatus is a payroll run's lifecycle state.
type PayrollRunStatus string

const (
	PayrollRunDraft     PayrollRunStatus = "draft"
	PayrollRunApproved  PayrollRunStatus = "approved"
	PayrollRunLocked    PayrollRunStatus = "locked"
	PayrollRunCancelled PayrollRunStatus = "cancelled"
)

// ValidPayrollRunStatus reports whether s is a known run status.
func ValidPayrollRunStatus(s PayrollRunStatus) bool {
	switch s {
	case PayrollRunDraft, PayrollRunApproved, PayrollRunLocked, PayrollRunCancelled:
		return true
	}
	return false
}

// ValidPresetKind reports whether k is a known item-preset kind.
func ValidPresetKind(k payroll.PresetKind) bool {
	switch k {
	case payroll.PresetKind("addition"), payroll.PresetKind("deduction"),
		payroll.PresetKind("employer_contribution"):
		return true
	}
	return false
}

var periodPattern = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)

// PayrollRun is one payroll computation for an org and a YYYY-MM period.
// Status flows draft -> approved -> locked (cancelled is a terminal side
// state from draft or approved). Only draft runs accept pay-line edits.
type PayrollRun struct {
	ID              string           `json:"id"`
	OrganizationID  string           `json:"organization_id"`
	Period          string           `json:"period"` // YYYY-MM
	CountryCode     string           `json:"country_code"`
	Status          PayrollRunStatus `json:"status"`
	RulePackVersion string           `json:"rule_pack_version,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	Version         int64            `json:"version"` // optimistic concurrency token
}

// Validate checks the run's shape.
func (r PayrollRun) Validate() error {
	if strings.TrimSpace(r.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if !periodPattern.MatchString(r.Period) {
		return &ValidationError{Field: "period", Message: "must be YYYY-MM"}
	}
	if r.CountryCode != "MY" && r.CountryCode != "ID" {
		return &ValidationError{Field: "country_code", Message: "must be MY or ID"}
	}
	if r.Status != "" && !ValidPayrollRunStatus(r.Status) {
		return &ValidationError{Field: "status", Message: "unknown payroll run status"}
	}
	return nil
}

// EmployeePayLine is one employee's computed pay for a run: itemized
// additions/deductions plus the statutory split the rule pack produced.
type EmployeePayLine struct {
	ID              string                  `json:"id"`
	OrganizationID  string                  `json:"organization_id"`
	RunID           string                  `json:"run_id"`
	EmployeeID      string                  `json:"employee_id"`
	GrossMinor      int64                   `json:"gross_minor"`
	AdditionsMinor  int64                   `json:"additions_minor"`
	DeductionsMinor int64                   `json:"deductions_minor"`
	NetMinor        int64                   `json:"net_minor"`
	Items           []payroll.PayLineItem   `json:"items,omitempty"`
	Statutory       payroll.StatutorySplits `json:"statutory"`
	BankAccount     string                  `json:"bank_account,omitempty"`
	BankCode        string                  `json:"bank_code,omitempty"`
	CreatedAt       time.Time               `json:"created_at"`
	UpdatedAt       time.Time               `json:"updated_at"`
	Version         int64                   `json:"version"` // optimistic concurrency token
}

// Validate checks the pay line's shape.
func (l EmployeePayLine) Validate() error {
	if strings.TrimSpace(l.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(l.RunID) == "" {
		return &ValidationError{Field: "run_id", Message: "required"}
	}
	if strings.TrimSpace(l.EmployeeID) == "" {
		return &ValidationError{Field: "employee_id", Message: "required"}
	}
	return nil
}

// PayrollItemPreset is one payroll item preset. System presets come from
// payroll.SeedPresets (IsSystem=true, ID "sys:"+code) and are immutable;
// org presets are per-organization overrides/custom rows whose codes must
// not collide with seed codes.
type PayrollItemPreset struct {
	ID                       string             `json:"id"`
	OrganizationID           string             `json:"organization_id"`
	Code                     string             `json:"code"`
	Name                     string             `json:"name"`
	Kind                     payroll.PresetKind `json:"kind"`
	SubjectToEPF             bool               `json:"subject_to_epf"`
	SubjectToSOCSO           bool               `json:"subject_to_socso"`
	SubjectToEIS             bool               `json:"subject_to_eis"`
	SubjectToPCB             bool               `json:"subject_to_pcb"`
	SubjectToBPJSKes         bool               `json:"subject_to_bpjs_kes"`
	SubjectToJHT             bool               `json:"subject_to_jht"`
	SubjectToJKK             bool               `json:"subject_to_jkk"`
	SubjectToJKM             bool               `json:"subject_to_jkm"`
	SubjectToJP              bool               `json:"subject_to_jp"`
	SubjectToPPh21           bool               `json:"subject_to_pph21"`
	ReducesContributionWages bool               `json:"reduces_contribution_wages"`
	IsSystem                 bool               `json:"is_system"` // seed preset, never stored
	CreatedAt                time.Time          `json:"created_at"`
	UpdatedAt                time.Time          `json:"updated_at"`
	Version                  int64              `json:"version"` // optimistic concurrency token
}

// Validate checks the preset's shape.
func (p PayrollItemPreset) Validate() error {
	if strings.TrimSpace(p.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(p.Code) == "" {
		return &ValidationError{Field: "code", Message: "required"}
	}
	if strings.TrimSpace(p.Name) == "" {
		return &ValidationError{Field: "name", Message: "required"}
	}
	if !ValidPresetKind(p.Kind) {
		return &ValidationError{Field: "kind", Message: "unknown preset kind"}
	}
	return nil
}

// PayrollRunFilter selects payroll runs for listing. Zero values mean "any".
type PayrollRunFilter struct {
	Status PayrollRunStatus
	Period string
}

// SyncItem is one externally-ingested payroll item staged on a draft run
// (e.g. from an attendance or claims system) before it is folded into a
// pay line.
type SyncItem struct {
	ID             string             `json:"id"`
	OrganizationID string             `json:"organization_id"`
	RunID          string             `json:"run_id"`
	EmployeeID     string             `json:"employee_id"`
	Source         string             `json:"source,omitempty"`
	ExternalID     string             `json:"external_id,omitempty"`
	Label          string             `json:"label,omitempty"`
	Kind           payroll.PresetKind `json:"kind"` // addition | deduction
	PresetCode     string             `json:"preset_code,omitempty"`
	AmountMinor    int64              `json:"amount_minor"`
	CreatedAt      time.Time          `json:"created_at"`
}

// Validate checks the sync item's shape.
func (s SyncItem) Validate() error {
	if strings.TrimSpace(s.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(s.RunID) == "" {
		return &ValidationError{Field: "run_id", Message: "required"}
	}
	if strings.TrimSpace(s.EmployeeID) == "" {
		return &ValidationError{Field: "employee_id", Message: "required"}
	}
	switch s.Kind {
	case payroll.PresetKind("addition"), payroll.PresetKind("deduction"):
	default:
		return &ValidationError{Field: "kind", Message: "must be addition or deduction"}
	}
	return nil
}

// PayrollPreview is a stored preview payload for a run (the preview ->
// submit two-phase gate). Expired previews read as ErrNotFound.
type PayrollPreview struct {
	ID             string          `json:"id"`
	OrganizationID string          `json:"organization_id"`
	RunID          string          `json:"run_id"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	ExpiresAt      time.Time       `json:"expires_at,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// Validate checks the preview's shape.
func (p PayrollPreview) Validate() error {
	if strings.TrimSpace(p.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(p.RunID) == "" {
		return &ValidationError{Field: "run_id", Message: "required"}
	}
	return nil
}

// PresetToPayroll converts a store preset to the shared payroll.Preset
// contract (drops store-only fields like ID, IsSystem, Version).
func PresetToPayroll(p PayrollItemPreset) payroll.Preset {
	return payroll.Preset{
		Code:                     p.Code,
		Name:                     p.Name,
		Kind:                     p.Kind,
		SubjectToEPF:             p.SubjectToEPF,
		SubjectToSOCSO:           p.SubjectToSOCSO,
		SubjectToEIS:             p.SubjectToEIS,
		SubjectToPCB:             p.SubjectToPCB,
		SubjectToBPJSKes:         p.SubjectToBPJSKes,
		SubjectToJHT:             p.SubjectToJHT,
		SubjectToJKK:             p.SubjectToJKK,
		SubjectToJKM:             p.SubjectToJKM,
		SubjectToJP:              p.SubjectToJP,
		SubjectToPPh21:           p.SubjectToPPh21,
		ReducesContributionWages: p.ReducesContributionWages,
	}
}

// PayrollToPreset converts a shared payroll.Preset to a store preset.
// Store-only fields (ID, OrganizationID, IsSystem, Version, timestamps) are
// left zero for the caller to fill.
func PayrollToPreset(p payroll.Preset) PayrollItemPreset {
	return PayrollItemPreset{
		Code:                     p.Code,
		Name:                     p.Name,
		Kind:                     p.Kind,
		SubjectToEPF:             p.SubjectToEPF,
		SubjectToSOCSO:           p.SubjectToSOCSO,
		SubjectToEIS:             p.SubjectToEIS,
		SubjectToPCB:             p.SubjectToPCB,
		SubjectToBPJSKes:         p.SubjectToBPJSKes,
		SubjectToJHT:             p.SubjectToJHT,
		SubjectToJKK:             p.SubjectToJKK,
		SubjectToJKM:             p.SubjectToJKM,
		SubjectToJP:              p.SubjectToJP,
		SubjectToPPh21:           p.SubjectToPPh21,
		ReducesContributionWages: p.ReducesContributionWages,
	}
}

// allowedRunTransition reports whether from -> to is a legal payroll run
// status transition.
func allowedRunTransition(from, to PayrollRunStatus) bool {
	switch from {
	case PayrollRunDraft:
		return to == PayrollRunApproved || to == PayrollRunCancelled
	case PayrollRunApproved:
		return to == PayrollRunLocked || to == PayrollRunCancelled
	default:
		return false
	}
}

// PayrollStore is the component 5 contract: payroll runs and their
// lifecycle, per-employee pay lines, item presets (system seeds + org
// rows), sync items, and preview payloads.
//
// Implementations must be safe for concurrent use and must deep-copy values
// across the boundary, so a caller can never mutate stored state through a
// previously returned value.
type PayrollStore interface {
	// Item presets (system seeds from payroll.SeedPresets + org rows).
	// ListPayrollPresets returns the merged catalog: seed presets
	// (IsSystem=true, ID "sys:"+code) first, then the organization's own
	// presets, ordered by (created_at, id) within each half, paged.
	ListPayrollPresets(ctx context.Context, orgID string, p Page) (presets []PayrollItemPreset, next string, err error)
	// GetPayrollPreset returns a preset by ID, resolving "sys:"+code to
	// the seed preset, or ErrNotFound.
	GetPayrollPreset(ctx context.Context, orgID, id string) (PayrollItemPreset, error)
	// CreatePayrollPreset stores an org preset. The code must not collide
	// with a seed code or another org preset code (ErrAlreadyExists).
	// Version starts at 1.
	CreatePayrollPreset(ctx context.Context, orgID string, pr PayrollItemPreset) (PayrollItemPreset, error)
	// UpdatePayrollPreset applies pr as the new state iff pr.Version
	// matches. System presets are immutable (ErrSystemPresetImmutable).
	// Returns ErrVersionConflict on mismatch, ErrNotFound when unknown.
	UpdatePayrollPreset(ctx context.Context, orgID string, pr PayrollItemPreset) (PayrollItemPreset, error)

	// Payroll runs.
	// CreatePayrollRun stores a new run; status is forced to draft.
	// (organization_id, period) must be unique (ErrAlreadyExists on clash).
	CreatePayrollRun(ctx context.Context, r PayrollRun) (PayrollRun, error)
	// GetPayrollRun returns the run or ErrNotFound.
	GetPayrollRun(ctx context.Context, orgID, id string) (PayrollRun, error)
	// ListPayrollRuns returns matching runs ordered by (created_at, id),
	// paged.
	ListPayrollRuns(ctx context.Context, orgID string, f PayrollRunFilter, p Page) (runs []PayrollRun, next string, err error)
	// TransitionPayrollRun moves a run to a new status iff version
	// matches. Allowed: draft->approved, draft->cancelled,
	// approved->locked, approved->cancelled; anything else is
	// ErrIllegalRunTransition. actor identifies who requested the
	// transition (callers record it in the audit log).
	TransitionPayrollRun(ctx context.Context, orgID, id string, version int64, to PayrollRunStatus, actor string) (PayrollRun, error)

	// Pay lines.
	// UpsertPayLine inserts the line, or updates it iff the stored
	// version matches (ErrVersionConflict). The run must be in draft
	// (ErrRunNotDraft otherwise).
	UpsertPayLine(ctx context.Context, l EmployeePayLine) (EmployeePayLine, error)
	// ListPayLines returns the run's pay lines ordered by (created_at, id),
	// paged.
	ListPayLines(ctx context.Context, orgID, runID string, p Page) (lines []EmployeePayLine, next string, err error)
	// GetPayLine returns the run's pay line for the employee, or
	// ErrNotFound.
	GetPayLine(ctx context.Context, orgID, runID, employeeID string) (EmployeePayLine, error)

	// Sync items (staged external items on a draft run).
	// AddSyncItems appends items atomically; empty is a no-op. The run
	// must be in draft (ErrRunNotDraft otherwise).
	AddSyncItems(ctx context.Context, orgID, runID string, items []SyncItem) error
	// ListSyncItems returns the run's sync items ordered by
	// (created_at, id), paged.
	ListSyncItems(ctx context.Context, orgID, runID string, p Page) (items []SyncItem, next string, err error)

	// Previews (preview -> submit two-phase gate).
	// SavePayrollPreview stores a preview payload with an expiry.
	SavePayrollPreview(ctx context.Context, p PayrollPreview) (PayrollPreview, error)
	// GetPayrollPreview returns the preview, or ErrNotFound when unknown
	// or expired (ExpiresAt in the past).
	GetPayrollPreview(ctx context.Context, orgID, id string) (PayrollPreview, error)
}
