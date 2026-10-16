package store

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// timeMemoryTables is the component 2 sidecar for MemoryStore. Component 1
// owns the MemoryStore struct itself (store/memory.go, not touched here), so
// the time-tracking tables live in this lazily-initialized sidecar keyed by
// the store pointer. It carries its own RWMutex and mirrors the Postgres
// adapter's semantics: UUID identities, optimistic concurrency, soft deletes,
// correction chains, the per-employee clock state machine, and pay-period
// freezing.
type timeMemoryTables struct {
	mu       sync.RWMutex
	projects map[string]Project             // gkey(orgID, id)
	entries  map[string]TimeEntry           // gkey(orgID, id)
	hours    map[string]HourEntry           // gkey(orgID, id)
	defs     map[string]PayPeriodDefinition // gkey(orgID, id)
	periods  map[string]PayPeriod           // gkey(orgID, id)
	scheds   map[string]Schedule            // gkey(orgID, id)
	shifts   map[string]Shift               // gkey(orgID, id)
}

func newTimeMemoryTables() *timeMemoryTables {
	return &timeMemoryTables{
		projects: make(map[string]Project),
		entries:  make(map[string]TimeEntry),
		hours:    make(map[string]HourEntry),
		defs:     make(map[string]PayPeriodDefinition),
		periods:  make(map[string]PayPeriod),
		scheds:   make(map[string]Schedule),
		shifts:   make(map[string]Shift),
	}
}

var (
	timeMemMu sync.Mutex
	timeMem   = map[*MemoryStore]*timeMemoryTables{}
)

// timeTables returns the component 2 sidecar for s, creating it on first
// use. The sidecar outlives individual calls exactly like MemoryStore's own
// maps.
func timeTables(s *MemoryStore) *timeMemoryTables {
	timeMemMu.Lock()
	defer timeMemMu.Unlock()
	t, ok := timeMem[s]
	if !ok {
		t = newTimeMemoryTables()
		timeMem[s] = t
	}
	return t
}

// --- deep copies ---

func copyTimeEntry(e TimeEntry) TimeEntry {
	e.ProjectID = copyStrPtr(e.ProjectID)
	e.ActivityID = copyStrPtr(e.ActivityID)
	e.LocationID = copyStrPtr(e.LocationID)
	e.Latitude = copyFloatPtr(e.Latitude)
	e.Longitude = copyFloatPtr(e.Longitude)
	e.Address = copyStrPtr(e.Address)
	e.PrevID = copyStrPtr(e.PrevID)
	e.NextID = copyStrPtr(e.NextID)
	e.DeletedAt = copyTimePtr(e.DeletedAt)
	return e
}

func copyFloatPtr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyHourEntry(e HourEntry) HourEntry {
	e.ProjectID = copyStrPtr(e.ProjectID)
	e.DeletedAt = copyTimePtr(e.DeletedAt)
	return e
}

func copyProject(p Project) Project { return p }

func copyPayPeriodDefinition(d PayPeriodDefinition) PayPeriodDefinition { return d }

func copyPayPeriod(p PayPeriod) PayPeriod { return p }

func copySchedule(s Schedule) Schedule { return s }

func copyShift(s Shift) Shift { return s }

// --- shared helpers ---

// latestEntryLocked returns the employee's latest head entry (not superseded,
// not deleted) ordered by (occurred_at, created_at, id). The caller must
// hold tt.mu.
func latestEntryLocked(tt *timeMemoryTables, orgID, employeeID string) (TimeEntry, bool) {
	var best TimeEntry
	found := false
	for _, e := range tt.entries {
		if e.OrganizationID != orgID || e.EmployeeID != employeeID {
			continue
		}
		if e.DeletedAt != nil || e.NextID != nil {
			continue
		}
		if !found || afterTimeEntry(e, best) {
			best, found = e, true
		}
	}
	return best, found
}

func afterTimeEntry(a, b TimeEntry) bool {
	if !a.OccurredAt.Equal(b.OccurredAt) {
		return a.OccurredAt.After(b.OccurredAt)
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID > b.ID
}

// periodFrozenAtLocked reports whether any pay period covering t is in a
// frozen status (Submitted/Approved). The caller must hold tt.mu (read is
// fine).
func periodFrozenAtLocked(tt *timeMemoryTables, orgID string, t time.Time) bool {
	for _, p := range tt.periods {
		if p.OrganizationID != orgID {
			continue
		}
		if !periodFrozen(p.Status) {
			continue
		}
		if !t.Before(p.PeriodStart) && t.Before(p.PeriodEnd) {
			return true
		}
	}
	return false
}

// newTimeEntry assigns identity/timestamps/defaults before insert.
func newTimeEntry(e TimeEntry) TimeEntry {
	if e.ID == "" {
		e.ID = newID()
	}
	if e.ClientType == "" {
		e.ClientType = TimeEntryClientWeb
	}
	now := time.Now().UTC()
	e.OccurredAt = e.OccurredAt.UTC()
	e.CreatedAt = now
	e.UpdatedAt = now
	e.Version = 1
	return e
}

// ---------------------------------------------------------------------------
// Projects
// ---------------------------------------------------------------------------

func (s *MemoryStore) CreateProject(ctx context.Context, p Project) (Project, error) {
	if err := ctx.Err(); err != nil {
		return Project{}, err
	}
	if err := p.Validate(); err != nil {
		return Project{}, err
	}
	if p.ID == "" {
		p.ID = newID()
	}
	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now
	p.Version = 1

	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if _, ok := s.orgs[p.OrganizationID]; !ok {
		return Project{}, fmt.Errorf("%w: organization %s", ErrNotFound, p.OrganizationID)
	}
	if _, ok := tt.projects[gkey(p.OrganizationID, p.ID)]; ok {
		return Project{}, fmt.Errorf("%w: project %s", ErrAlreadyExists, p.ID)
	}
	tt.projects[gkey(p.OrganizationID, p.ID)] = p
	return copyProject(p), nil
}

func (s *MemoryStore) GetProject(ctx context.Context, orgID, id string) (Project, error) {
	if err := ctx.Err(); err != nil {
		return Project{}, err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	p, ok := tt.projects[gkey(orgID, id)]
	if !ok {
		return Project{}, fmt.Errorf("%w: project %s", ErrNotFound, id)
	}
	return copyProject(p), nil
}

func (s *MemoryStore) UpdateProject(ctx context.Context, p Project) (Project, error) {
	if err := ctx.Err(); err != nil {
		return Project{}, err
	}
	if err := p.Validate(); err != nil {
		return Project{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	k := gkey(p.OrganizationID, p.ID)
	cur, ok := tt.projects[k]
	if !ok {
		return Project{}, fmt.Errorf("%w: project %s", ErrNotFound, p.ID)
	}
	if p.Version != cur.Version {
		return Project{}, fmt.Errorf("%w: project %s", ErrVersionConflict, p.ID)
	}
	cur.Name = p.Name
	cur.ClientName = p.ClientName
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	tt.projects[k] = cur
	return copyProject(cur), nil
}

func (s *MemoryStore) DeleteProject(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	k := gkey(orgID, id)
	if _, ok := tt.projects[k]; !ok {
		return fmt.Errorf("%w: project %s", ErrNotFound, id)
	}
	delete(tt.projects, k)
	// Referencing entries keep their rows; the FK is SET NULL in Postgres,
	// mirrored here.
	for ek, e := range tt.entries {
		if e.OrganizationID == orgID && e.ProjectID != nil && *e.ProjectID == id {
			e.ProjectID = nil
			e.UpdatedAt = time.Now().UTC()
			tt.entries[ek] = e
		}
	}
	for hk, h := range tt.hours {
		if h.OrganizationID == orgID && h.ProjectID != nil && *h.ProjectID == id {
			h.ProjectID = nil
			h.UpdatedAt = time.Now().UTC()
			tt.hours[hk] = h
		}
	}
	return nil
}

func (s *MemoryStore) ListProjects(ctx context.Context, orgID string, pg Page) ([]Project, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	var out []Project
	for _, p := range tt.projects {
		if p.OrganizationID == orgID {
			out = append(out, copyProject(p))
		}
	}
	sortByCursor(out, func(p Project) cursorKey { return cursorKey{p.CreatedAt, p.ID} })
	return paginate(out, func(p Project) cursorKey { return cursorKey{p.CreatedAt, p.ID} }, pg)
}

// ---------------------------------------------------------------------------
// Clock state machine + time entries
// ---------------------------------------------------------------------------

// insertEntryLocked validates, period-gate-checks, stamps, and stores e. The
// caller must hold tt.mu (write).
func insertEntryLocked(tt *timeMemoryTables, orgID string, e TimeEntry) (TimeEntry, error) {
	if err := e.Validate(); err != nil {
		return TimeEntry{}, err
	}
	if periodFrozenAtLocked(tt, orgID, e.OccurredAt) {
		return TimeEntry{}, fmt.Errorf("%w: entry falls in a frozen pay period", ErrPeriodLocked)
	}
	if e.ProjectID != nil {
		if _, ok := tt.projects[gkey(orgID, *e.ProjectID)]; !ok {
			return TimeEntry{}, fmt.Errorf("%w: project %s", ErrNotFound, *e.ProjectID)
		}
	}
	e = newTimeEntry(e)
	if _, ok := tt.entries[gkey(orgID, e.ID)]; ok {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s", ErrAlreadyExists, e.ID)
	}
	tt.entries[gkey(orgID, e.ID)] = e
	return copyTimeEntry(e), nil
}

func (s *MemoryStore) ClockIn(ctx context.Context, orgID, employeeID string, e TimeEntry) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if _, ok := s.employees[gkey(orgID, employeeID)]; !ok {
		return TimeEntry{}, fmt.Errorf("%w: employee %s", ErrNotFound, employeeID)
	}
	if open, found := latestEntryLocked(tt, orgID, employeeID); found && openKind(open.Kind) {
		return TimeEntry{}, fmt.Errorf("%w: employee %s already clocked in", ErrAlreadyClockedIn, employeeID)
	}
	e.OrganizationID = orgID
	e.EmployeeID = employeeID
	e.Kind = TimeEntryIn
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	return insertEntryLocked(tt, orgID, e)
}

func (s *MemoryStore) ClockOut(ctx context.Context, orgID, employeeID string, e TimeEntry) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if _, ok := s.employees[gkey(orgID, employeeID)]; !ok {
		return TimeEntry{}, fmt.Errorf("%w: employee %s", ErrNotFound, employeeID)
	}
	open, found := latestEntryLocked(tt, orgID, employeeID)
	if !found || !openKind(open.Kind) {
		return TimeEntry{}, fmt.Errorf("%w: employee %s has no open entry", ErrNotFound, employeeID)
	}
	e.OrganizationID = orgID
	e.EmployeeID = employeeID
	e.Kind = TimeEntryOut
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	if !e.OccurredAt.After(open.OccurredAt) && !e.OccurredAt.Equal(open.OccurredAt) {
		return TimeEntry{}, &ValidationError{Field: "occurred_at", Message: "must not be before the open entry"}
	}
	return insertEntryLocked(tt, orgID, e)
}

func (s *MemoryStore) StartBreak(ctx context.Context, orgID, employeeID string, e TimeEntry) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if _, ok := s.employees[gkey(orgID, employeeID)]; !ok {
		return TimeEntry{}, fmt.Errorf("%w: employee %s", ErrNotFound, employeeID)
	}
	open, found := latestEntryLocked(tt, orgID, employeeID)
	if !found || !openKind(open.Kind) {
		return TimeEntry{}, fmt.Errorf("%w: employee %s has no open entry", ErrNotFound, employeeID)
	}
	if open.Kind != TimeEntryIn && open.Kind != TimeEntryEndBreak {
		return TimeEntry{}, fmt.Errorf("%w: cannot start a break while %s", ErrInvalidTransition, open.Kind)
	}
	e.OrganizationID = orgID
	e.EmployeeID = employeeID
	e.Kind = TimeEntryStartBreak
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	return insertEntryLocked(tt, orgID, e)
}

func (s *MemoryStore) EndBreak(ctx context.Context, orgID, employeeID string, e TimeEntry) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if _, ok := s.employees[gkey(orgID, employeeID)]; !ok {
		return TimeEntry{}, fmt.Errorf("%w: employee %s", ErrNotFound, employeeID)
	}
	open, found := latestEntryLocked(tt, orgID, employeeID)
	if !found || open.Kind != TimeEntryStartBreak {
		return TimeEntry{}, fmt.Errorf("%w: employee %s is not on a break", ErrInvalidTransition, employeeID)
	}
	e.OrganizationID = orgID
	e.EmployeeID = employeeID
	e.Kind = TimeEntryEndBreak
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	return insertEntryLocked(tt, orgID, e)
}

func (s *MemoryStore) OpenTimeEntry(ctx context.Context, orgID, employeeID string) (TimeEntry, bool, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, false, err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	open, found := latestEntryLocked(tt, orgID, employeeID)
	if !found || !openKind(open.Kind) {
		return TimeEntry{}, false, nil
	}
	return copyTimeEntry(open), true, nil
}

// CreateTimeEntry stores a backfill/manual punch without enforcing the
// clock state machine (the Clock* methods do that).
func (s *MemoryStore) CreateTimeEntry(ctx context.Context, e TimeEntry) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if _, ok := s.employees[gkey(e.OrganizationID, e.EmployeeID)]; !ok {
		return TimeEntry{}, fmt.Errorf("%w: employee %s", ErrNotFound, e.EmployeeID)
	}
	return insertEntryLocked(tt, e.OrganizationID, e)
}

func (s *MemoryStore) GetTimeEntry(ctx context.Context, orgID, id string) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	e, ok := tt.entries[gkey(orgID, id)]
	if !ok {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s", ErrNotFound, id)
	}
	return copyTimeEntry(e), nil
}

// mutableEntryLocked returns the entry if it may be changed: not soft
// deleted, not locked, and not inside a frozen pay period.
func mutableEntryLocked(tt *timeMemoryTables, orgID, id string) (TimeEntry, error) {
	e, ok := tt.entries[gkey(orgID, id)]
	if !ok || e.DeletedAt != nil {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s", ErrNotFound, id)
	}
	if e.IsLocked {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s is locked", ErrPeriodLocked, id)
	}
	if periodFrozenAtLocked(tt, orgID, e.OccurredAt) {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s falls in a frozen pay period", ErrPeriodLocked, id)
	}
	return e, nil
}

func (s *MemoryStore) UpdateTimeEntry(ctx context.Context, e TimeEntry) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	if err := e.Validate(); err != nil {
		return TimeEntry{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	cur, err := mutableEntryLocked(tt, e.OrganizationID, e.ID)
	if err != nil {
		return TimeEntry{}, err
	}
	if e.Version != cur.Version {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s", ErrVersionConflict, e.ID)
	}
	if e.Kind != cur.Kind {
		return TimeEntry{}, &ValidationError{Field: "kind", Message: "cannot change kind; use CorrectTimeEntry"}
	}
	if e.EmployeeID != cur.EmployeeID {
		return TimeEntry{}, &ValidationError{Field: "employee_id", Message: "cannot reassign entries between employees"}
	}
	if e.ProjectID != nil {
		if _, ok := tt.projects[gkey(e.OrganizationID, *e.ProjectID)]; !ok {
			return TimeEntry{}, fmt.Errorf("%w: project %s", ErrNotFound, *e.ProjectID)
		}
	}
	// The new occurred_at must also clear the period gate (a move across
	// periods is a different timesheet).
	if periodFrozenAtLocked(tt, e.OrganizationID, e.OccurredAt) {
		return TimeEntry{}, fmt.Errorf("%w: new occurred_at falls in a frozen pay period", ErrPeriodLocked)
	}
	cur.OccurredAt = e.OccurredAt.UTC()
	cur.ProjectID = copyStrPtr(e.ProjectID)
	cur.ActivityID = copyStrPtr(e.ActivityID)
	cur.LocationID = copyStrPtr(e.LocationID)
	cur.Latitude = copyFloatPtr(e.Latitude)
	cur.Longitude = copyFloatPtr(e.Longitude)
	cur.Address = copyStrPtr(e.Address)
	cur.ClientType = e.ClientType
	if cur.ClientType == "" {
		cur.ClientType = TimeEntryClientWeb
	}
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	tt.entries[gkey(e.OrganizationID, e.ID)] = cur
	return copyTimeEntry(cur), nil
}

func (s *MemoryStore) DeleteTimeEntry(ctx context.Context, orgID, id string, version int64) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	cur, err := mutableEntryLocked(tt, orgID, id)
	if err != nil {
		return TimeEntry{}, err
	}
	if version != cur.Version {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s", ErrVersionConflict, id)
	}
	now := time.Now().UTC()
	cur.DeletedAt = &now
	cur.UpdatedAt = now
	cur.Version++
	tt.entries[gkey(orgID, id)] = cur
	return copyTimeEntry(cur), nil
}

func (s *MemoryStore) CorrectTimeEntry(ctx context.Context, orgID, id string, correction TimeEntry) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	// Precedence: "already corrected" is the more specific error for a
	// frozen-via-correction original, so check the chain before the lock.
	raw, ok := tt.entries[gkey(orgID, id)]
	if !ok || raw.DeletedAt != nil {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s", ErrNotFound, id)
	}
	if raw.NextID != nil {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s already corrected", ErrInvalidTransition, id)
	}
	orig, err := mutableEntryLocked(tt, orgID, id)
	if err != nil {
		return TimeEntry{}, err
	}
	correction.OrganizationID = orgID
	correction.EmployeeID = orig.EmployeeID
	if correction.OccurredAt.IsZero() {
		correction.OccurredAt = orig.OccurredAt
	}
	if err := correction.Validate(); err != nil {
		return TimeEntry{}, err
	}
	if periodFrozenAtLocked(tt, orgID, correction.OccurredAt) {
		return TimeEntry{}, fmt.Errorf("%w: correction falls in a frozen pay period", ErrPeriodLocked)
	}
	if correction.ProjectID != nil {
		if _, ok := tt.projects[gkey(orgID, *correction.ProjectID)]; !ok {
			return TimeEntry{}, fmt.Errorf("%w: project %s", ErrNotFound, *correction.ProjectID)
		}
	}
	created := newTimeEntry(correction)
	created.PrevID = &orig.ID
	tt.entries[gkey(orgID, created.ID)] = created
	// Freeze the original and link it forward.
	orig.NextID = &created.ID
	orig.IsLocked = true
	orig.UpdatedAt = time.Now().UTC()
	orig.Version++
	tt.entries[gkey(orgID, id)] = orig
	return copyTimeEntry(created), nil
}

func (s *MemoryStore) ListTimeEntries(ctx context.Context, orgID string, f TimeEntryFilter, pg Page) ([]TimeEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	var out []TimeEntry
	for _, e := range tt.entries {
		if e.OrganizationID != orgID {
			continue
		}
		if f.EmployeeID != "" && e.EmployeeID != f.EmployeeID {
			continue
		}
		if f.Kind != "" && e.Kind != f.Kind {
			continue
		}
		if f.From != nil && e.OccurredAt.Before(*f.From) {
			continue
		}
		if f.To != nil && !e.OccurredAt.Before(*f.To) {
			continue
		}
		if e.DeletedAt != nil && !f.IncludeDeleted {
			continue
		}
		if e.NextID != nil && !f.IncludeHistory {
			continue
		}
		out = append(out, copyTimeEntry(e))
	}
	sortByCursor(out, func(e TimeEntry) cursorKey { return cursorKey{e.CreatedAt, e.ID} })
	return paginate(out, func(e TimeEntry) cursorKey { return cursorKey{e.CreatedAt, e.ID} }, pg)
}

// ---------------------------------------------------------------------------
// Hour entries
// ---------------------------------------------------------------------------

func (s *MemoryStore) CreateHourEntry(ctx context.Context, e HourEntry) (HourEntry, error) {
	if err := ctx.Err(); err != nil {
		return HourEntry{}, err
	}
	if err := e.Validate(); err != nil {
		return HourEntry{}, err
	}
	if e.ID == "" {
		e.ID = newID()
	}
	now := time.Now().UTC()
	e.WorkDate = e.WorkDate.UTC()
	e.CreatedAt = now
	e.UpdatedAt = now
	e.Version = 1

	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if _, ok := s.employees[gkey(e.OrganizationID, e.EmployeeID)]; !ok {
		return HourEntry{}, fmt.Errorf("%w: employee %s", ErrNotFound, e.EmployeeID)
	}
	if e.ProjectID != nil {
		if _, ok := tt.projects[gkey(e.OrganizationID, *e.ProjectID)]; !ok {
			return HourEntry{}, fmt.Errorf("%w: project %s", ErrNotFound, *e.ProjectID)
		}
	}
	if periodFrozenAtLocked(tt, e.OrganizationID, e.WorkDate) {
		return HourEntry{}, fmt.Errorf("%w: work date falls in a frozen pay period", ErrPeriodLocked)
	}
	if _, ok := tt.hours[gkey(e.OrganizationID, e.ID)]; ok {
		return HourEntry{}, fmt.Errorf("%w: hour entry %s", ErrAlreadyExists, e.ID)
	}
	tt.hours[gkey(e.OrganizationID, e.ID)] = e
	return copyHourEntry(e), nil
}

func (s *MemoryStore) GetHourEntry(ctx context.Context, orgID, id string) (HourEntry, error) {
	if err := ctx.Err(); err != nil {
		return HourEntry{}, err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	e, ok := tt.hours[gkey(orgID, id)]
	if !ok {
		return HourEntry{}, fmt.Errorf("%w: hour entry %s", ErrNotFound, id)
	}
	return copyHourEntry(e), nil
}

func (s *MemoryStore) UpdateHourEntry(ctx context.Context, e HourEntry) (HourEntry, error) {
	if err := ctx.Err(); err != nil {
		return HourEntry{}, err
	}
	if err := e.Validate(); err != nil {
		return HourEntry{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	k := gkey(e.OrganizationID, e.ID)
	cur, ok := tt.hours[k]
	if !ok || cur.DeletedAt != nil {
		return HourEntry{}, fmt.Errorf("%w: hour entry %s", ErrNotFound, e.ID)
	}
	if e.Version != cur.Version {
		return HourEntry{}, fmt.Errorf("%w: hour entry %s", ErrVersionConflict, e.ID)
	}
	if e.EmployeeID != cur.EmployeeID {
		return HourEntry{}, &ValidationError{Field: "employee_id", Message: "cannot reassign entries between employees"}
	}
	if periodFrozenAtLocked(tt, e.OrganizationID, cur.WorkDate) ||
		periodFrozenAtLocked(tt, e.OrganizationID, e.WorkDate) {
		return HourEntry{}, fmt.Errorf("%w: hour entry falls in a frozen pay period", ErrPeriodLocked)
	}
	if e.ProjectID != nil {
		if _, ok := tt.projects[gkey(e.OrganizationID, *e.ProjectID)]; !ok {
			return HourEntry{}, fmt.Errorf("%w: project %s", ErrNotFound, *e.ProjectID)
		}
	}
	cur.WorkDate = e.WorkDate.UTC()
	cur.Hours = e.Hours
	cur.ProjectID = copyStrPtr(e.ProjectID)
	cur.Note = e.Note
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	tt.hours[k] = cur
	return copyHourEntry(cur), nil
}

func (s *MemoryStore) DeleteHourEntry(ctx context.Context, orgID, id string, version int64) (HourEntry, error) {
	if err := ctx.Err(); err != nil {
		return HourEntry{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	k := gkey(orgID, id)
	cur, ok := tt.hours[k]
	if !ok || cur.DeletedAt != nil {
		return HourEntry{}, fmt.Errorf("%w: hour entry %s", ErrNotFound, id)
	}
	if version != cur.Version {
		return HourEntry{}, fmt.Errorf("%w: hour entry %s", ErrVersionConflict, id)
	}
	if periodFrozenAtLocked(tt, orgID, cur.WorkDate) {
		return HourEntry{}, fmt.Errorf("%w: hour entry %s falls in a frozen pay period", ErrPeriodLocked, id)
	}
	now := time.Now().UTC()
	cur.DeletedAt = &now
	cur.UpdatedAt = now
	cur.Version++
	tt.hours[k] = cur
	return copyHourEntry(cur), nil
}

func (s *MemoryStore) ListHourEntries(ctx context.Context, orgID string, f HourEntryFilter, pg Page) ([]HourEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	var out []HourEntry
	for _, e := range tt.hours {
		if e.OrganizationID != orgID {
			continue
		}
		if f.EmployeeID != "" && e.EmployeeID != f.EmployeeID {
			continue
		}
		if f.From != nil && e.WorkDate.Before(*f.From) {
			continue
		}
		if f.To != nil && !e.WorkDate.Before(*f.To) {
			continue
		}
		if e.DeletedAt != nil && !f.IncludeDeleted {
			continue
		}
		out = append(out, copyHourEntry(e))
	}
	sortByCursor(out, func(e HourEntry) cursorKey { return cursorKey{e.CreatedAt, e.ID} })
	return paginate(out, func(e HourEntry) cursorKey { return cursorKey{e.CreatedAt, e.ID} }, pg)
}

// ---------------------------------------------------------------------------
// Pay-period definitions
// ---------------------------------------------------------------------------

func (s *MemoryStore) CreatePayPeriodDefinition(ctx context.Context, d PayPeriodDefinition) (PayPeriodDefinition, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriodDefinition{}, err
	}
	if err := d.Validate(); err != nil {
		return PayPeriodDefinition{}, err
	}
	if d.ID == "" {
		d.ID = newID()
	}
	now := time.Now().UTC()
	d.CreatedAt = now
	d.UpdatedAt = now
	d.Version = 1

	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if _, ok := s.orgs[d.OrganizationID]; !ok {
		return PayPeriodDefinition{}, fmt.Errorf("%w: organization %s", ErrNotFound, d.OrganizationID)
	}
	if _, ok := tt.defs[gkey(d.OrganizationID, d.ID)]; ok {
		return PayPeriodDefinition{}, fmt.Errorf("%w: pay period definition %s", ErrAlreadyExists, d.ID)
	}
	tt.defs[gkey(d.OrganizationID, d.ID)] = d
	return copyPayPeriodDefinition(d), nil
}

func (s *MemoryStore) GetPayPeriodDefinition(ctx context.Context, orgID, id string) (PayPeriodDefinition, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriodDefinition{}, err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	d, ok := tt.defs[gkey(orgID, id)]
	if !ok {
		return PayPeriodDefinition{}, fmt.Errorf("%w: pay period definition %s", ErrNotFound, id)
	}
	return copyPayPeriodDefinition(d), nil
}

func (s *MemoryStore) UpdatePayPeriodDefinition(ctx context.Context, d PayPeriodDefinition) (PayPeriodDefinition, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriodDefinition{}, err
	}
	if err := d.Validate(); err != nil {
		return PayPeriodDefinition{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	k := gkey(d.OrganizationID, d.ID)
	cur, ok := tt.defs[k]
	if !ok {
		return PayPeriodDefinition{}, fmt.Errorf("%w: pay period definition %s", ErrNotFound, d.ID)
	}
	if d.Version != cur.Version {
		return PayPeriodDefinition{}, fmt.Errorf("%w: pay period definition %s", ErrVersionConflict, d.ID)
	}
	cur.Name = d.Name
	cur.Cadence = d.Cadence
	cur.AnchorDay = d.AnchorDay
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	tt.defs[k] = cur
	return copyPayPeriodDefinition(cur), nil
}

func (s *MemoryStore) DeletePayPeriodDefinition(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	k := gkey(orgID, id)
	if _, ok := tt.defs[k]; !ok {
		return fmt.Errorf("%w: pay period definition %s", ErrNotFound, id)
	}
	for _, p := range tt.periods {
		if p.OrganizationID == orgID && p.DefinitionID == id {
			return fmt.Errorf("%w: definition %s still has pay periods", ErrNotEmpty, id)
		}
	}
	delete(tt.defs, k)
	return nil
}

func (s *MemoryStore) ListPayPeriodDefinitions(ctx context.Context, orgID string, pg Page) ([]PayPeriodDefinition, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	var out []PayPeriodDefinition
	for _, d := range tt.defs {
		if d.OrganizationID == orgID {
			out = append(out, copyPayPeriodDefinition(d))
		}
	}
	sortByCursor(out, func(d PayPeriodDefinition) cursorKey { return cursorKey{d.CreatedAt, d.ID} })
	return paginate(out, func(d PayPeriodDefinition) cursorKey { return cursorKey{d.CreatedAt, d.ID} }, pg)
}

// ---------------------------------------------------------------------------
// Pay periods
// ---------------------------------------------------------------------------

func (s *MemoryStore) CreatePayPeriod(ctx context.Context, orgID, definitionID string, periodStart time.Time) (PayPeriod, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriod{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	def, ok := tt.defs[gkey(orgID, definitionID)]
	if !ok {
		return PayPeriod{}, fmt.Errorf("%w: pay period definition %s", ErrNotFound, definitionID)
	}
	start, end, err := ComputePayPeriodRange(def, periodStart)
	if err != nil {
		return PayPeriod{}, err
	}
	for _, p := range tt.periods {
		if p.OrganizationID == orgID && p.DefinitionID == definitionID && p.PeriodStart.Equal(start) {
			return PayPeriod{}, fmt.Errorf("%w: pay period already exists for %s", ErrAlreadyExists, start.Format("2006-01-02"))
		}
	}
	now := time.Now().UTC()
	p := PayPeriod{
		ID:             newID(),
		OrganizationID: orgID,
		DefinitionID:   definitionID,
		PeriodStart:    start,
		PeriodEnd:      end,
		Status:         PayPeriodOpen,
		Version:        1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	tt.periods[gkey(orgID, p.ID)] = p
	return copyPayPeriod(p), nil
}

func (s *MemoryStore) GetPayPeriod(ctx context.Context, orgID, id string) (PayPeriod, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriod{}, err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	p, ok := tt.periods[gkey(orgID, id)]
	if !ok {
		return PayPeriod{}, fmt.Errorf("%w: pay period %s", ErrNotFound, id)
	}
	return copyPayPeriod(p), nil
}

func (s *MemoryStore) ListPayPeriods(ctx context.Context, orgID string, f PayPeriodFilter, pg Page) ([]PayPeriod, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	var out []PayPeriod
	for _, p := range tt.periods {
		if p.OrganizationID != orgID {
			continue
		}
		if f.DefinitionID != "" && p.DefinitionID != f.DefinitionID {
			continue
		}
		if f.Status != "" && p.Status != f.Status {
			continue
		}
		out = append(out, copyPayPeriod(p))
	}
	sortByCursor(out, func(p PayPeriod) cursorKey { return cursorKey{p.CreatedAt, p.ID} })
	return paginate(out, func(p PayPeriod) cursorKey { return cursorKey{p.CreatedAt, p.ID} }, pg)
}

// transitionPayPeriodLocked applies a version-checked status transition. On
// approve it locks the covered entries; on reopen it unlocks them. The
// caller must hold tt.mu (write).
func transitionPayPeriodLocked(tt *timeMemoryTables, orgID, id string, version int64, to PayPeriodStatus) (PayPeriod, error) {
	k := gkey(orgID, id)
	cur, ok := tt.periods[k]
	if !ok {
		return PayPeriod{}, fmt.Errorf("%w: pay period %s", ErrNotFound, id)
	}
	if version != cur.Version {
		return PayPeriod{}, fmt.Errorf("%w: pay period %s", ErrVersionConflict, id)
	}
	if !validPayPeriodTransition(cur.Status, to) {
		return PayPeriod{}, fmt.Errorf("%w: cannot move pay period from %s to %s", ErrInvalidTransition, cur.Status, to)
	}
	cur.Status = to
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	tt.periods[k] = cur
	// Freeze / unfreeze the covered entries.
	locked := to == PayPeriodApproved
	if to == PayPeriodApproved || to == PayPeriodReopened {
		for ek, e := range tt.entries {
			if e.OrganizationID != orgID || e.DeletedAt != nil {
				continue
			}
			if e.OccurredAt.Before(cur.PeriodStart) || !e.OccurredAt.Before(cur.PeriodEnd) {
				continue
			}
			e.IsLocked = locked
			e.UpdatedAt = time.Now().UTC()
			e.Version++
			tt.entries[ek] = e
		}
	}
	return copyPayPeriod(cur), nil
}

func (s *MemoryStore) SubmitPayPeriod(ctx context.Context, orgID, id string, version int64) (PayPeriod, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriod{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return transitionPayPeriodLocked(tt, orgID, id, version, PayPeriodSubmitted)
}

func (s *MemoryStore) ApprovePayPeriod(ctx context.Context, orgID, id string, version int64) (PayPeriod, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriod{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return transitionPayPeriodLocked(tt, orgID, id, version, PayPeriodApproved)
}

func (s *MemoryStore) RejectPayPeriod(ctx context.Context, orgID, id string, version int64) (PayPeriod, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriod{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return transitionPayPeriodLocked(tt, orgID, id, version, PayPeriodRejected)
}

func (s *MemoryStore) ReopenPayPeriod(ctx context.Context, orgID, id string, version int64) (PayPeriod, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriod{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return transitionPayPeriodLocked(tt, orgID, id, version, PayPeriodReopened)
}

// ---------------------------------------------------------------------------
// Schedules
// ---------------------------------------------------------------------------

func (s *MemoryStore) CreateSchedule(ctx context.Context, sc Schedule) (Schedule, error) {
	if err := ctx.Err(); err != nil {
		return Schedule{}, err
	}
	if err := sc.Validate(); err != nil {
		return Schedule{}, err
	}
	if sc.ID == "" {
		sc.ID = newID()
	}
	now := time.Now().UTC()
	sc.CreatedAt = now
	sc.UpdatedAt = now
	sc.Version = 1

	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if _, ok := s.employees[gkey(sc.OrganizationID, sc.EmployeeID)]; !ok {
		return Schedule{}, fmt.Errorf("%w: employee %s", ErrNotFound, sc.EmployeeID)
	}
	if _, ok := tt.scheds[gkey(sc.OrganizationID, sc.ID)]; ok {
		return Schedule{}, fmt.Errorf("%w: schedule %s", ErrAlreadyExists, sc.ID)
	}
	tt.scheds[gkey(sc.OrganizationID, sc.ID)] = sc
	return copySchedule(sc), nil
}

func (s *MemoryStore) GetSchedule(ctx context.Context, orgID, id string) (Schedule, error) {
	if err := ctx.Err(); err != nil {
		return Schedule{}, err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	sc, ok := tt.scheds[gkey(orgID, id)]
	if !ok {
		return Schedule{}, fmt.Errorf("%w: schedule %s", ErrNotFound, id)
	}
	return copySchedule(sc), nil
}

func (s *MemoryStore) UpdateSchedule(ctx context.Context, sc Schedule) (Schedule, error) {
	if err := ctx.Err(); err != nil {
		return Schedule{}, err
	}
	if err := sc.Validate(); err != nil {
		return Schedule{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	k := gkey(sc.OrganizationID, sc.ID)
	cur, ok := tt.scheds[k]
	if !ok {
		return Schedule{}, fmt.Errorf("%w: schedule %s", ErrNotFound, sc.ID)
	}
	if sc.Version != cur.Version {
		return Schedule{}, fmt.Errorf("%w: schedule %s", ErrVersionConflict, sc.ID)
	}
	if sc.EmployeeID != cur.EmployeeID {
		if _, ok := s.employees[gkey(sc.OrganizationID, sc.EmployeeID)]; !ok {
			return Schedule{}, fmt.Errorf("%w: employee %s", ErrNotFound, sc.EmployeeID)
		}
	}
	cur.EmployeeID = sc.EmployeeID
	cur.Kind = sc.Kind
	cur.ExpectedDailyHours = sc.ExpectedDailyHours
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	tt.scheds[k] = cur
	return copySchedule(cur), nil
}

func (s *MemoryStore) DeleteSchedule(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	k := gkey(orgID, id)
	if _, ok := tt.scheds[k]; !ok {
		return fmt.Errorf("%w: schedule %s", ErrNotFound, id)
	}
	delete(tt.scheds, k)
	return nil
}

func (s *MemoryStore) ListSchedules(ctx context.Context, orgID, employeeID string, pg Page) ([]Schedule, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	var out []Schedule
	for _, sc := range tt.scheds {
		if sc.OrganizationID != orgID {
			continue
		}
		if employeeID != "" && sc.EmployeeID != employeeID {
			continue
		}
		out = append(out, copySchedule(sc))
	}
	sortByCursor(out, func(sc Schedule) cursorKey { return cursorKey{sc.CreatedAt, sc.ID} })
	return paginate(out, func(sc Schedule) cursorKey { return cursorKey{sc.CreatedAt, sc.ID} }, pg)
}

// ---------------------------------------------------------------------------
// Shifts
// ---------------------------------------------------------------------------

func (s *MemoryStore) CreateShift(ctx context.Context, sh Shift) (Shift, error) {
	if err := ctx.Err(); err != nil {
		return Shift{}, err
	}
	if err := sh.Validate(); err != nil {
		return Shift{}, err
	}
	if sh.ID == "" {
		sh.ID = newID()
	}
	now := time.Now().UTC()
	sh.ShiftDate = sh.ShiftDate.UTC()
	sh.CreatedAt = now
	sh.UpdatedAt = now
	sh.Version = 1

	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if _, ok := s.employees[gkey(sh.OrganizationID, sh.EmployeeID)]; !ok {
		return Shift{}, fmt.Errorf("%w: employee %s", ErrNotFound, sh.EmployeeID)
	}
	if _, ok := tt.shifts[gkey(sh.OrganizationID, sh.ID)]; ok {
		return Shift{}, fmt.Errorf("%w: shift %s", ErrAlreadyExists, sh.ID)
	}
	tt.shifts[gkey(sh.OrganizationID, sh.ID)] = sh
	return copyShift(sh), nil
}

func (s *MemoryStore) GetShift(ctx context.Context, orgID, id string) (Shift, error) {
	if err := ctx.Err(); err != nil {
		return Shift{}, err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	sh, ok := tt.shifts[gkey(orgID, id)]
	if !ok {
		return Shift{}, fmt.Errorf("%w: shift %s", ErrNotFound, id)
	}
	return copyShift(sh), nil
}

func (s *MemoryStore) UpdateShift(ctx context.Context, sh Shift) (Shift, error) {
	if err := ctx.Err(); err != nil {
		return Shift{}, err
	}
	if err := sh.Validate(); err != nil {
		return Shift{}, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	k := gkey(sh.OrganizationID, sh.ID)
	cur, ok := tt.shifts[k]
	if !ok {
		return Shift{}, fmt.Errorf("%w: shift %s", ErrNotFound, sh.ID)
	}
	if sh.Version != cur.Version {
		return Shift{}, fmt.Errorf("%w: shift %s", ErrVersionConflict, sh.ID)
	}
	if sh.EmployeeID != cur.EmployeeID {
		if _, ok := s.employees[gkey(sh.OrganizationID, sh.EmployeeID)]; !ok {
			return Shift{}, fmt.Errorf("%w: employee %s", ErrNotFound, sh.EmployeeID)
		}
	}
	cur.EmployeeID = sh.EmployeeID
	cur.ShiftDate = sh.ShiftDate.UTC()
	cur.StartsAt = sh.StartsAt
	cur.EndsAt = sh.EndsAt
	cur.Published = sh.Published
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	tt.shifts[k] = cur
	return copyShift(cur), nil
}

func (s *MemoryStore) DeleteShift(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	k := gkey(orgID, id)
	if _, ok := tt.shifts[k]; !ok {
		return fmt.Errorf("%w: shift %s", ErrNotFound, id)
	}
	delete(tt.shifts, k)
	return nil
}

func (s *MemoryStore) ListShifts(ctx context.Context, orgID string, f ShiftFilter, pg Page) ([]Shift, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	tt := timeTables(s)
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	var out []Shift
	for _, sh := range tt.shifts {
		if sh.OrganizationID != orgID {
			continue
		}
		if f.EmployeeID != "" && sh.EmployeeID != f.EmployeeID {
			continue
		}
		if f.From != nil && sh.ShiftDate.Before(*f.From) {
			continue
		}
		if f.To != nil && !sh.ShiftDate.Before(*f.To) {
			continue
		}
		if f.Published != nil && sh.Published != *f.Published {
			continue
		}
		out = append(out, copyShift(sh))
	}
	sortByCursor(out, func(sh Shift) cursorKey { return cursorKey{sh.CreatedAt, sh.ID} })
	return paginate(out, func(sh Shift) cursorKey { return cursorKey{sh.CreatedAt, sh.ID} }, pg)
}

func (s *MemoryStore) PublishShifts(ctx context.Context, orgID string, ids []string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	tt := timeTables(s)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	var n int64
	now := time.Now().UTC()
	for _, id := range ids {
		k := gkey(orgID, id)
		sh, ok := tt.shifts[k]
		if !ok {
			return 0, fmt.Errorf("%w: shift %s", ErrNotFound, id)
		}
		if !sh.Published {
			sh.Published = true
			sh.UpdatedAt = now
			sh.Version++
			tt.shifts[k] = sh
			n++
		}
	}
	return n, nil
}
