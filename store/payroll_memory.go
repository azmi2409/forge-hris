package store

// MemoryStore payroll implementation (component 5). All methods hold s.mu
// and deep-copy across the boundary, mirroring the Postgres adapter's
// semantics. System presets come from payroll.SeedPresets (never stored).

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/azmi2409/forge-hris/payroll"
)

// Compile-time contract check.
var _ PayrollStore = (*MemoryStore)(nil)

// systemPresetPrefix marks seed preset IDs in the merged catalog.
const systemPresetPrefix = "sys:"

// seedPresetByCode indexes the seed catalog by code.
func seedPresetByCode() map[string]payroll.Preset {
	out := make(map[string]payroll.Preset)
	for _, p := range payroll.SeedPresets() {
		out[p.Code] = p
	}
	return out
}

// resolveSystemPreset converts a "sys:"+code ID to the seed preset, or
// found=false.
func resolveSystemPreset(id string) (PayrollItemPreset, bool) {
	code, ok := strings.CutPrefix(id, systemPresetPrefix)
	if !ok || code == "" {
		return PayrollItemPreset{}, false
	}
	sp, ok := seedPresetByCode()[code]
	if !ok {
		return PayrollItemPreset{}, false
	}
	pr := PayrollToPreset(sp)
	pr.ID = systemPresetPrefix + sp.Code
	pr.IsSystem = true
	return pr, true
}

// paylineKey builds the composite map key for employee pay lines.
func paylineKey(orgID, runID, employeeID string) string {
	return orgID + "\x00" + runID + "\x00" + employeeID
}

func copyPayrollRun(r PayrollRun) PayrollRun { return r }

func copyEmployeePayLine(l EmployeePayLine) EmployeePayLine {
	l.Items = append([]payroll.PayLineItem(nil), l.Items...)
	return l
}

func copyPayrollItemPreset(p PayrollItemPreset) PayrollItemPreset { return p }

func copySyncItem(i SyncItem) SyncItem { return i }

func copyPayrollPreview(p PayrollPreview) PayrollPreview {
	p.Payload = copyBytes(p.Payload)
	return p
}

// ---------------------------------------------------------------------------
// Item presets
// ---------------------------------------------------------------------------

func (s *MemoryStore) ListPayrollPresets(ctx context.Context, orgID string, p Page) ([]PayrollItemPreset, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sys := make([]PayrollItemPreset, 0, len(payroll.SeedPresets()))
	for _, sp := range payroll.SeedPresets() {
		pr := PayrollToPreset(sp)
		pr.ID = systemPresetPrefix + sp.Code
		pr.IsSystem = true
		sys = append(sys, pr)
	}
	key := func(pr PayrollItemPreset) cursorKey { return cursorKey{pr.CreatedAt, pr.ID} }
	sortByCursor(sys, key)
	var org []PayrollItemPreset
	for _, pr := range s.payPresets {
		if pr.OrganizationID == orgID {
			org = append(org, copyPayrollItemPreset(pr))
		}
	}
	sortByCursor(org, key)
	return paginate(append(sys, org...), key, p)
}

func (s *MemoryStore) GetPayrollPreset(ctx context.Context, orgID, id string) (PayrollItemPreset, error) {
	if err := ctx.Err(); err != nil {
		return PayrollItemPreset{}, err
	}
	if pr, ok := resolveSystemPreset(id); ok {
		return pr, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	pr, ok := s.payPresets[gkey(orgID, id)]
	if !ok {
		return PayrollItemPreset{}, fmt.Errorf("%w: payroll item preset %s", ErrNotFound, id)
	}
	return copyPayrollItemPreset(pr), nil
}

func (s *MemoryStore) CreatePayrollPreset(ctx context.Context, orgID string, pr PayrollItemPreset) (PayrollItemPreset, error) {
	if err := ctx.Err(); err != nil {
		return PayrollItemPreset{}, err
	}
	pr.OrganizationID = orgID
	if err := pr.Validate(); err != nil {
		return PayrollItemPreset{}, err
	}
	if pr.ID == "" {
		pr.ID = newID()
	}
	now := time.Now().UTC()
	pr.CreatedAt = now
	pr.UpdatedAt = now
	pr.Version = 1
	pr.IsSystem = false

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[orgID]; !ok {
		return PayrollItemPreset{}, fmt.Errorf("%w: organization %s", ErrNotFound, orgID)
	}
	if _, clash := seedPresetByCode()[pr.Code]; clash {
		return PayrollItemPreset{}, fmt.Errorf("%w: preset code %q collides with a system preset", ErrAlreadyExists, pr.Code)
	}
	if _, taken := s.payPresetCodes[gkey(orgID, pr.Code)]; taken {
		return PayrollItemPreset{}, fmt.Errorf("%w: preset code %q", ErrAlreadyExists, pr.Code)
	}
	if _, ok := s.payPresets[gkey(orgID, pr.ID)]; ok {
		return PayrollItemPreset{}, fmt.Errorf("%w: payroll item preset %s", ErrAlreadyExists, pr.ID)
	}
	s.payPresets[gkey(orgID, pr.ID)] = pr
	s.payPresetCodes[gkey(orgID, pr.Code)] = pr.ID
	return copyPayrollItemPreset(pr), nil
}

func (s *MemoryStore) UpdatePayrollPreset(ctx context.Context, orgID string, pr PayrollItemPreset) (PayrollItemPreset, error) {
	if err := ctx.Err(); err != nil {
		return PayrollItemPreset{}, err
	}
	if _, ok := resolveSystemPreset(pr.ID); ok {
		return PayrollItemPreset{}, fmt.Errorf("%w: preset %s", ErrSystemPresetImmutable, pr.ID)
	}
	pr.OrganizationID = orgID
	if err := pr.Validate(); err != nil {
		return PayrollItemPreset{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := gkey(orgID, pr.ID)
	cur, ok := s.payPresets[k]
	if !ok {
		return PayrollItemPreset{}, fmt.Errorf("%w: payroll item preset %s", ErrNotFound, pr.ID)
	}
	if pr.Version != cur.Version {
		return PayrollItemPreset{}, fmt.Errorf("%w: payroll item preset %s", ErrVersionConflict, pr.ID)
	}
	if pr.Code != cur.Code {
		if _, clash := seedPresetByCode()[pr.Code]; clash {
			return PayrollItemPreset{}, fmt.Errorf("%w: preset code %q collides with a system preset", ErrAlreadyExists, pr.Code)
		}
		if _, taken := s.payPresetCodes[gkey(orgID, pr.Code)]; taken {
			return PayrollItemPreset{}, fmt.Errorf("%w: preset code %q", ErrAlreadyExists, pr.Code)
		}
		delete(s.payPresetCodes, gkey(orgID, cur.Code))
		s.payPresetCodes[gkey(orgID, pr.Code)] = pr.ID
	}
	cur.Code = pr.Code
	cur.Name = pr.Name
	cur.Kind = pr.Kind
	cur.SubjectToEPF = pr.SubjectToEPF
	cur.SubjectToSOCSO = pr.SubjectToSOCSO
	cur.SubjectToEIS = pr.SubjectToEIS
	cur.SubjectToPCB = pr.SubjectToPCB
	cur.SubjectToBPJSKes = pr.SubjectToBPJSKes
	cur.SubjectToJHT = pr.SubjectToJHT
	cur.SubjectToJKK = pr.SubjectToJKK
	cur.SubjectToJKM = pr.SubjectToJKM
	cur.SubjectToJP = pr.SubjectToJP
	cur.SubjectToPPh21 = pr.SubjectToPPh21
	cur.ReducesContributionWages = pr.ReducesContributionWages
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	s.payPresets[k] = cur
	return copyPayrollItemPreset(cur), nil
}

// ---------------------------------------------------------------------------
// Payroll runs
// ---------------------------------------------------------------------------

func (s *MemoryStore) CreatePayrollRun(ctx context.Context, r PayrollRun) (PayrollRun, error) {
	if err := ctx.Err(); err != nil {
		return PayrollRun{}, err
	}
	r.Status = PayrollRunDraft // creation always starts in draft
	if err := r.Validate(); err != nil {
		return PayrollRun{}, err
	}
	if r.ID == "" {
		r.ID = newID()
	}
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	r.Version = 1

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[r.OrganizationID]; !ok {
		return PayrollRun{}, fmt.Errorf("%w: organization %s", ErrNotFound, r.OrganizationID)
	}
	if _, taken := s.payrollRunPeriod[gkey(r.OrganizationID, r.Period)]; taken {
		return PayrollRun{}, fmt.Errorf("%w: payroll run for period %s", ErrAlreadyExists, r.Period)
	}
	if _, ok := s.payrollRuns[gkey(r.OrganizationID, r.ID)]; ok {
		return PayrollRun{}, fmt.Errorf("%w: payroll run %s", ErrAlreadyExists, r.ID)
	}
	s.payrollRuns[gkey(r.OrganizationID, r.ID)] = r
	s.payrollRunPeriod[gkey(r.OrganizationID, r.Period)] = r.ID
	return copyPayrollRun(r), nil
}

func (s *MemoryStore) GetPayrollRun(ctx context.Context, orgID, id string) (PayrollRun, error) {
	if err := ctx.Err(); err != nil {
		return PayrollRun{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.payrollRuns[gkey(orgID, id)]
	if !ok {
		return PayrollRun{}, fmt.Errorf("%w: payroll run %s", ErrNotFound, id)
	}
	return copyPayrollRun(r), nil
}

func (s *MemoryStore) ListPayrollRuns(ctx context.Context, orgID string, f PayrollRunFilter, p Page) ([]PayrollRun, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []PayrollRun
	for _, r := range s.payrollRuns {
		if r.OrganizationID != orgID {
			continue
		}
		if f.Status != "" && r.Status != f.Status {
			continue
		}
		if f.Period != "" && r.Period != f.Period {
			continue
		}
		out = append(out, copyPayrollRun(r))
	}
	sortByCursor(out, func(r PayrollRun) cursorKey { return cursorKey{r.CreatedAt, r.ID} })
	return paginate(out, func(r PayrollRun) cursorKey { return cursorKey{r.CreatedAt, r.ID} }, p)
}

func (s *MemoryStore) TransitionPayrollRun(ctx context.Context, orgID, id string, version int64, to PayrollRunStatus, actor string) (PayrollRun, error) {
	if err := ctx.Err(); err != nil {
		return PayrollRun{}, err
	}
	if !ValidPayrollRunStatus(to) {
		return PayrollRun{}, &ValidationError{Field: "status", Message: "unknown payroll run status"}
	}
	_ = actor // informational; callers record it in the audit log
	s.mu.Lock()
	defer s.mu.Unlock()
	k := gkey(orgID, id)
	r, ok := s.payrollRuns[k]
	if !ok {
		return PayrollRun{}, fmt.Errorf("%w: payroll run %s", ErrNotFound, id)
	}
	if version != r.Version {
		return PayrollRun{}, fmt.Errorf("%w: payroll run %s", ErrVersionConflict, id)
	}
	if !allowedRunTransition(r.Status, to) {
		return PayrollRun{}, fmt.Errorf("%w: %s -> %s", ErrIllegalRunTransition, r.Status, to)
	}
	r.Status = to
	r.UpdatedAt = time.Now().UTC()
	r.Version++
	s.payrollRuns[k] = r
	return copyPayrollRun(r), nil
}

// ---------------------------------------------------------------------------
// Pay lines
// ---------------------------------------------------------------------------

// draftRunLocked returns the run iff it exists and is in draft.
func (s *MemoryStore) draftRunLocked(orgID, runID string) (PayrollRun, error) {
	r, ok := s.payrollRuns[gkey(orgID, runID)]
	if !ok {
		return PayrollRun{}, fmt.Errorf("%w: payroll run %s", ErrNotFound, runID)
	}
	if r.Status != PayrollRunDraft {
		return PayrollRun{}, fmt.Errorf("%w: payroll run %s is %s", ErrRunNotDraft, runID, r.Status)
	}
	return r, nil
}

func (s *MemoryStore) UpsertPayLine(ctx context.Context, l EmployeePayLine) (EmployeePayLine, error) {
	if err := ctx.Err(); err != nil {
		return EmployeePayLine{}, err
	}
	if err := l.Validate(); err != nil {
		return EmployeePayLine{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.draftRunLocked(l.OrganizationID, l.RunID); err != nil {
		return EmployeePayLine{}, err
	}
	if _, ok := s.employees[gkey(l.OrganizationID, l.EmployeeID)]; !ok {
		return EmployeePayLine{}, fmt.Errorf("%w: employee %s", ErrNotFound, l.EmployeeID)
	}
	k := paylineKey(l.OrganizationID, l.RunID, l.EmployeeID)
	if cur, ok := s.payLines[k]; ok {
		if l.Version != cur.Version {
			return EmployeePayLine{}, fmt.Errorf("%w: pay line for employee %s", ErrVersionConflict, l.EmployeeID)
		}
		cur.GrossMinor = l.GrossMinor
		cur.AdditionsMinor = l.AdditionsMinor
		cur.DeductionsMinor = l.DeductionsMinor
		cur.NetMinor = l.NetMinor
		cur.Items = append([]payroll.PayLineItem(nil), l.Items...)
		cur.Statutory = l.Statutory
		cur.BankAccount = l.BankAccount
		cur.BankCode = l.BankCode
		cur.UpdatedAt = time.Now().UTC()
		cur.Version++
		s.payLines[k] = cur
		return copyEmployeePayLine(cur), nil
	}
	if l.ID == "" {
		l.ID = newID()
	}
	now := time.Now().UTC()
	l.CreatedAt = now
	l.UpdatedAt = now
	l.Version = 1
	l.Items = append([]payroll.PayLineItem(nil), l.Items...)
	s.payLines[k] = l
	return copyEmployeePayLine(l), nil
}

func (s *MemoryStore) ListPayLines(ctx context.Context, orgID, runID string, p Page) ([]EmployeePayLine, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []EmployeePayLine
	for _, l := range s.payLines {
		if l.OrganizationID == orgID && l.RunID == runID {
			out = append(out, copyEmployeePayLine(l))
		}
	}
	sortByCursor(out, func(l EmployeePayLine) cursorKey { return cursorKey{l.CreatedAt, l.ID} })
	return paginate(out, func(l EmployeePayLine) cursorKey { return cursorKey{l.CreatedAt, l.ID} }, p)
}

func (s *MemoryStore) GetPayLine(ctx context.Context, orgID, runID, employeeID string) (EmployeePayLine, error) {
	if err := ctx.Err(); err != nil {
		return EmployeePayLine{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	l, ok := s.payLines[paylineKey(orgID, runID, employeeID)]
	if !ok {
		return EmployeePayLine{}, fmt.Errorf("%w: pay line for employee %s", ErrNotFound, employeeID)
	}
	return copyEmployeePayLine(l), nil
}

// ---------------------------------------------------------------------------
// Sync items
// ---------------------------------------------------------------------------

func (s *MemoryStore) AddSyncItems(ctx context.Context, orgID, runID string, items []SyncItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	// Validate everything before inserting anything: the add is atomic.
	for i := range items {
		items[i].OrganizationID = orgID
		items[i].RunID = runID
		if err := items[i].Validate(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.draftRunLocked(orgID, runID); err != nil {
		return err
	}
	for i := range items {
		if _, ok := s.employees[gkey(orgID, items[i].EmployeeID)]; !ok {
			return fmt.Errorf("%w: employee %s", ErrNotFound, items[i].EmployeeID)
		}
	}
	for i := range items {
		it := items[i]
		if it.ID == "" {
			it.ID = newID()
		}
		it.CreatedAt = time.Now().UTC()
		s.paySyncItems[gkey(orgID, it.ID)] = it
	}
	return nil
}

func (s *MemoryStore) ListSyncItems(ctx context.Context, orgID, runID string, p Page) ([]SyncItem, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []SyncItem
	for _, it := range s.paySyncItems {
		if it.OrganizationID == orgID && it.RunID == runID {
			out = append(out, copySyncItem(it))
		}
	}
	sortByCursor(out, func(i SyncItem) cursorKey { return cursorKey{i.CreatedAt, i.ID} })
	return paginate(out, func(i SyncItem) cursorKey { return cursorKey{i.CreatedAt, i.ID} }, p)
}

// ---------------------------------------------------------------------------
// Previews
// ---------------------------------------------------------------------------

func (s *MemoryStore) SavePayrollPreview(ctx context.Context, p PayrollPreview) (PayrollPreview, error) {
	if err := ctx.Err(); err != nil {
		return PayrollPreview{}, err
	}
	if err := p.Validate(); err != nil {
		return PayrollPreview{}, err
	}
	if p.ID == "" {
		p.ID = newID()
	}
	p.CreatedAt = time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.payrollRuns[gkey(p.OrganizationID, p.RunID)]; !ok {
		return PayrollPreview{}, fmt.Errorf("%w: payroll run %s", ErrNotFound, p.RunID)
	}
	s.payPreviews[gkey(p.OrganizationID, p.ID)] = p
	return copyPayrollPreview(p), nil
}

func (s *MemoryStore) GetPayrollPreview(ctx context.Context, orgID, id string) (PayrollPreview, error) {
	if err := ctx.Err(); err != nil {
		return PayrollPreview{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.payPreviews[gkey(orgID, id)]
	if !ok {
		return PayrollPreview{}, fmt.Errorf("%w: payroll preview %s", ErrNotFound, id)
	}
	if !p.ExpiresAt.IsZero() && time.Now().After(p.ExpiresAt) {
		return PayrollPreview{}, fmt.Errorf("%w: payroll preview %s expired", ErrNotFound, id)
	}
	return copyPayrollPreview(p), nil
}
