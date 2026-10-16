package store

import (
	"context"
	"crypto/rand"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryStore is a concurrency-safe in-memory Store.
//
// It is the reference implementation used by tests and local development. It
// mirrors the Postgres adapter's semantics (see migrations/001_init.sql):
// UUID identities, optimistic-concurrency updates, soft employee deletes,
// and a per-organization hash-chained audit log.
type MemoryStore struct {
	mu          sync.RWMutex
	orgs        map[string]Organization
	orgSlugs    map[string]string // slug -> organization ID
	groups      map[string]Group  // orgID + "\x00" + id
	employees   map[string]Employee
	empNumbers  map[string]map[string]string // orgID -> employee_number -> employee ID
	empEmails   map[string]map[string]string // orgID -> email -> employee ID
	memberships map[string]Membership        // orgID + "\x00" + employeeID
	creds       map[string]Credential        // orgID + "\x00" + id
	credHashes  map[string]string            // token hash -> orgID + "\x00" + id
	audit       []AuditEvent                 // append-only, ordered by ID
	auditLast   map[string]string            // orgID -> hash of latest event
	idem        map[string]IdempotencyRecord // orgID + "\x00" + key
	auditSeq    atomic.Int64
	// --- leave (component 3; Worker A owns the time-tracking block) ---
	leavePolicies    map[string]TimeOffPolicy   // orgID + "\x00" + id
	leaveBalances    map[string]LeaveBalance    // orgID + "\x00" + employeeID + "\x00" + policyID + "\x00" + period
	leaveRequests    map[string]TimeOffRequest  // orgID + "\x00" + id
	leaveApprovals   map[string]TimeOffApproval // orgID + "\x00" + id
	holidayCalendars map[string]HolidayCalendar // orgID + "\x00" + id
	holidays         map[string]Holiday         // orgID + "\x00" + id
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		orgs:        make(map[string]Organization),
		orgSlugs:    make(map[string]string),
		groups:      make(map[string]Group),
		employees:   make(map[string]Employee),
		empNumbers:  make(map[string]map[string]string),
		empEmails:   make(map[string]map[string]string),
		memberships: make(map[string]Membership),
		creds:       make(map[string]Credential),
		credHashes:  make(map[string]string),
		auditLast:   make(map[string]string),
		idem:        make(map[string]IdempotencyRecord),
		// --- leave (component 3) ---
		leavePolicies:    make(map[string]TimeOffPolicy),
		leaveBalances:    make(map[string]LeaveBalance),
		leaveRequests:    make(map[string]TimeOffRequest),
		leaveApprovals:   make(map[string]TimeOffApproval),
		holidayCalendars: make(map[string]HolidayCalendar),
		holidays:         make(map[string]Holiday),
	}
}

// Compile-time contract check.
var _ Store = (*MemoryStore)(nil)

// newID generates a random UUID-formatted identity.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("store: random id: %v", err))
	}
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// gkey builds the composite map key for org-scoped records.
func gkey(orgID, id string) string { return orgID + "\x00" + id }

func strPtr(s string) *string { return &s }

func copyStrPtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyTimePtr(p *time.Time) *time.Time {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

func copyStrings(s []string) []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s...)
}

func copyOrganization(o Organization) Organization { return o }

func copyGroup(g Group) Group {
	g.ParentID = copyStrPtr(g.ParentID)
	return g
}

func copyEmployee(e Employee) Employee {
	e.GroupID = copyStrPtr(e.GroupID)
	e.ManagerID = copyStrPtr(e.ManagerID)
	e.WorkStartDate = copyTimePtr(e.WorkStartDate)
	return e
}

func copyMembership(m Membership) Membership {
	m.GrantedBy = copyStrPtr(m.GrantedBy)
	return m
}

func copyCredential(c Credential) Credential {
	c.EmployeeID = copyStrPtr(c.EmployeeID)
	c.Scopes = copyStrings(c.Scopes)
	c.ExpiresAt = copyTimePtr(c.ExpiresAt)
	c.RevokedAt = copyTimePtr(c.RevokedAt)
	c.LastUsedAt = copyTimePtr(c.LastUsedAt)
	return c
}

func copyAuditEvent(e AuditEvent) AuditEvent {
	e.OnBehalfOf = copyStrPtr(e.OnBehalfOf)
	e.Before = copyBytes(e.Before)
	e.After = copyBytes(e.After)
	e.IdempotencyKey = copyStrPtr(e.IdempotencyKey)
	return e
}

func copyIdempotencyRecord(r IdempotencyRecord) IdempotencyRecord {
	r.ResponseBody = copyBytes(r.ResponseBody)
	return r
}

// cursorKey is the (created_at, id) sort position of a listed record.
type cursorKey struct {
	t  time.Time
	id string
}

func sortByCursor[T any](items []T, key func(T) cursorKey) {
	sort.Slice(items, func(i, j int) bool {
		a, b := key(items[i]), key(items[j])
		if !a.t.Equal(b.t) {
			return a.t.Before(b.t)
		}
		return a.id < b.id
	})
}

// paginate applies a cursor page over items already sorted by (created_at,
// id). next is the cursor for the following page, "" when exhausted.
func paginate[T any](items []T, key func(T) cursorKey, p Page) ([]T, string, error) {
	limit := normalizeLimit(p.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if p.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(p.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	out := make([]T, 0, limit)
	next := ""
	for _, it := range items {
		k := key(it)
		if haveCursor && !afterCursor(k.t, k.id, ct, cid) {
			continue
		}
		if len(out) >= limit {
			last := key(out[len(out)-1])
			next = encodeCursor(last.t, last.id)
			break
		}
		out = append(out, it)
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Organizations
// ---------------------------------------------------------------------------

func (s *MemoryStore) CreateOrganization(ctx context.Context, o Organization) (Organization, error) {
	if err := ctx.Err(); err != nil {
		return Organization{}, err
	}
	if err := o.Validate(); err != nil {
		return Organization{}, err
	}
	if o.ID == "" {
		o.ID = newID()
	}
	now := time.Now().UTC()
	o.CreatedAt = now
	o.UpdatedAt = now
	o.Version = 1

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgSlugs[o.Slug]; ok {
		return Organization{}, fmt.Errorf("%w: organization slug %q", ErrAlreadyExists, o.Slug)
	}
	if _, ok := s.orgs[o.ID]; ok {
		return Organization{}, fmt.Errorf("%w: organization id %s", ErrAlreadyExists, o.ID)
	}
	s.orgs[o.ID] = o
	s.orgSlugs[o.Slug] = o.ID
	return copyOrganization(o), nil
}

func (s *MemoryStore) GetOrganization(ctx context.Context, id string) (Organization, error) {
	if err := ctx.Err(); err != nil {
		return Organization{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orgs[id]
	if !ok {
		return Organization{}, fmt.Errorf("%w: organization %s", ErrNotFound, id)
	}
	return copyOrganization(o), nil
}

func (s *MemoryStore) GetOrganizationBySlug(ctx context.Context, slug string) (Organization, error) {
	if err := ctx.Err(); err != nil {
		return Organization{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.orgSlugs[slug]
	if !ok {
		return Organization{}, fmt.Errorf("%w: organization slug %q", ErrNotFound, slug)
	}
	return copyOrganization(s.orgs[id]), nil
}

func (s *MemoryStore) UpdateOrganization(ctx context.Context, o Organization) (Organization, error) {
	if err := ctx.Err(); err != nil {
		return Organization{}, err
	}
	if err := o.Validate(); err != nil {
		return Organization{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.orgs[o.ID]
	if !ok {
		return Organization{}, fmt.Errorf("%w: organization %s", ErrNotFound, o.ID)
	}
	if o.Version != cur.Version {
		return Organization{}, fmt.Errorf("%w: organization %s", ErrVersionConflict, o.ID)
	}
	if o.Slug != cur.Slug {
		if _, taken := s.orgSlugs[o.Slug]; taken {
			return Organization{}, fmt.Errorf("%w: organization slug %q", ErrAlreadyExists, o.Slug)
		}
		delete(s.orgSlugs, cur.Slug)
		s.orgSlugs[o.Slug] = o.ID
	}
	cur.Name = o.Name
	cur.Slug = o.Slug
	cur.CountryCode = o.CountryCode
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	s.orgs[o.ID] = cur
	return copyOrganization(cur), nil
}

func (s *MemoryStore) ListOrganizations(ctx context.Context, p Page) ([]Organization, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Organization, 0, len(s.orgs))
	for _, o := range s.orgs {
		out = append(out, copyOrganization(o))
	}
	sortByCursor(out, func(o Organization) cursorKey { return cursorKey{o.CreatedAt, o.ID} })
	return paginate(out, func(o Organization) cursorKey { return cursorKey{o.CreatedAt, o.ID} }, p)
}

// ---------------------------------------------------------------------------
// Groups
// ---------------------------------------------------------------------------

// groupCycleLocked reports whether setting group's parent to parentID would
// create a cycle (walk the ancestor chain looking for groupID).
func (s *MemoryStore) groupCycleLocked(orgID, groupID, parentID string) bool {
	for pid := parentID; pid != ""; {
		if pid == groupID {
			return true
		}
		g, ok := s.groups[gkey(orgID, pid)]
		if !ok || g.ParentID == nil {
			return false
		}
		pid = *g.ParentID
	}
	return false
}

func (s *MemoryStore) CreateGroup(ctx context.Context, g Group) (Group, error) {
	if err := ctx.Err(); err != nil {
		return Group{}, err
	}
	if err := g.Validate(); err != nil {
		return Group{}, err
	}
	if g.ID == "" {
		g.ID = newID()
	}
	now := time.Now().UTC()
	g.CreatedAt = now
	g.UpdatedAt = now
	g.Version = 1

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[g.OrganizationID]; !ok {
		return Group{}, fmt.Errorf("%w: organization %s", ErrNotFound, g.OrganizationID)
	}
	if g.ParentID != nil {
		if _, ok := s.groups[gkey(g.OrganizationID, *g.ParentID)]; !ok {
			return Group{}, fmt.Errorf("%w: parent group %s", ErrNotFound, *g.ParentID)
		}
		if s.groupCycleLocked(g.OrganizationID, g.ID, *g.ParentID) {
			return Group{}, &ValidationError{Field: "parent_id", Message: "would create a cycle"}
		}
	}
	if _, ok := s.groups[gkey(g.OrganizationID, g.ID)]; ok {
		return Group{}, fmt.Errorf("%w: group %s", ErrAlreadyExists, g.ID)
	}
	s.groups[gkey(g.OrganizationID, g.ID)] = g
	return copyGroup(g), nil
}

func (s *MemoryStore) GetGroup(ctx context.Context, orgID, id string) (Group, error) {
	if err := ctx.Err(); err != nil {
		return Group{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.groups[gkey(orgID, id)]
	if !ok {
		return Group{}, fmt.Errorf("%w: group %s", ErrNotFound, id)
	}
	return copyGroup(g), nil
}

func (s *MemoryStore) UpdateGroup(ctx context.Context, g Group) (Group, error) {
	if err := ctx.Err(); err != nil {
		return Group{}, err
	}
	if err := g.Validate(); err != nil {
		return Group{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := gkey(g.OrganizationID, g.ID)
	cur, ok := s.groups[k]
	if !ok {
		return Group{}, fmt.Errorf("%w: group %s", ErrNotFound, g.ID)
	}
	if g.Version != cur.Version {
		return Group{}, fmt.Errorf("%w: group %s", ErrVersionConflict, g.ID)
	}
	if g.ParentID != nil {
		if _, ok := s.groups[gkey(g.OrganizationID, *g.ParentID)]; !ok {
			return Group{}, fmt.Errorf("%w: parent group %s", ErrNotFound, *g.ParentID)
		}
		if s.groupCycleLocked(g.OrganizationID, g.ID, *g.ParentID) {
			return Group{}, &ValidationError{Field: "parent_id", Message: "would create a cycle"}
		}
	}
	cur.Name = g.Name
	cur.ParentID = copyStrPtr(g.ParentID)
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	s.groups[k] = cur
	return copyGroup(cur), nil
}

func (s *MemoryStore) DeleteGroup(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := gkey(orgID, id)
	if _, ok := s.groups[k]; !ok {
		return fmt.Errorf("%w: group %s", ErrNotFound, id)
	}
	for _, e := range s.employees {
		if e.OrganizationID == orgID && e.GroupID != nil && *e.GroupID == id {
			return fmt.Errorf("%w: group %s still has members", ErrNotEmpty, id)
		}
	}
	for _, g := range s.groups {
		if g.OrganizationID == orgID && g.ParentID != nil && *g.ParentID == id {
			return fmt.Errorf("%w: group %s still has child groups", ErrNotEmpty, id)
		}
	}
	delete(s.groups, k)
	return nil
}

func (s *MemoryStore) ListGroups(ctx context.Context, orgID string, p Page) ([]Group, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Group
	for _, g := range s.groups {
		if g.OrganizationID == orgID {
			out = append(out, copyGroup(g))
		}
	}
	sortByCursor(out, func(g Group) cursorKey { return cursorKey{g.CreatedAt, g.ID} })
	return paginate(out, func(g Group) cursorKey { return cursorKey{g.CreatedAt, g.ID} }, p)
}

// ---------------------------------------------------------------------------
// Employees
// ---------------------------------------------------------------------------

func (s *MemoryStore) CreateEmployee(ctx context.Context, e Employee) (Employee, error) {
	if err := ctx.Err(); err != nil {
		return Employee{}, err
	}
	if err := e.Validate(); err != nil {
		return Employee{}, err
	}
	if e.ID == "" {
		e.ID = newID()
	}
	if e.Status == "" {
		e.Status = InvitationNotInvited
	}
	if e.EmploymentType == "" {
		e.EmploymentType = EmploymentFullTime
	}
	now := time.Now().UTC()
	e.CreatedAt = now
	e.UpdatedAt = now
	e.Version = 1

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[e.OrganizationID]; !ok {
		return Employee{}, fmt.Errorf("%w: organization %s", ErrNotFound, e.OrganizationID)
	}
	if e.GroupID != nil {
		if _, ok := s.groups[gkey(e.OrganizationID, *e.GroupID)]; !ok {
			return Employee{}, fmt.Errorf("%w: group %s", ErrNotFound, *e.GroupID)
		}
	}
	if e.ManagerID != nil {
		if _, ok := s.employees[gkey(e.OrganizationID, *e.ManagerID)]; !ok {
			return Employee{}, fmt.Errorf("%w: manager %s", ErrNotFound, *e.ManagerID)
		}
	}
	if s.empNumbers[e.OrganizationID] == nil {
		s.empNumbers[e.OrganizationID] = make(map[string]string)
	}
	if _, taken := s.empNumbers[e.OrganizationID][e.EmployeeNumber]; taken {
		return Employee{}, fmt.Errorf("%w: employee number %q", ErrAlreadyExists, e.EmployeeNumber)
	}
	if s.empEmails[e.OrganizationID] == nil {
		s.empEmails[e.OrganizationID] = make(map[string]string)
	}
	if e.Email != "" {
		if _, taken := s.empEmails[e.OrganizationID][e.Email]; taken {
			return Employee{}, fmt.Errorf("%w: email %q", ErrAlreadyExists, e.Email)
		}
	}
	if _, ok := s.employees[gkey(e.OrganizationID, e.ID)]; ok {
		return Employee{}, fmt.Errorf("%w: employee %s", ErrAlreadyExists, e.ID)
	}
	k := gkey(e.OrganizationID, e.ID)
	s.employees[k] = e
	s.empNumbers[e.OrganizationID][e.EmployeeNumber] = e.ID
	if e.Email != "" {
		s.empEmails[e.OrganizationID][e.Email] = e.ID
	}
	return copyEmployee(e), nil
}

func (s *MemoryStore) GetEmployee(ctx context.Context, orgID, id string) (Employee, error) {
	if err := ctx.Err(); err != nil {
		return Employee{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.employees[gkey(orgID, id)]
	if !ok {
		return Employee{}, fmt.Errorf("%w: employee %s", ErrNotFound, id)
	}
	return copyEmployee(e), nil
}

func (s *MemoryStore) UpdateEmployee(ctx context.Context, e Employee) (Employee, error) {
	if err := ctx.Err(); err != nil {
		return Employee{}, err
	}
	if err := e.Validate(); err != nil {
		return Employee{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := gkey(e.OrganizationID, e.ID)
	cur, ok := s.employees[k]
	if !ok {
		return Employee{}, fmt.Errorf("%w: employee %s", ErrNotFound, e.ID)
	}
	if e.Version != cur.Version {
		return Employee{}, fmt.Errorf("%w: employee %s", ErrVersionConflict, e.ID)
	}
	if e.GroupID != nil {
		if _, ok := s.groups[gkey(e.OrganizationID, *e.GroupID)]; !ok {
			return Employee{}, fmt.Errorf("%w: group %s", ErrNotFound, *e.GroupID)
		}
	}
	if e.ManagerID != nil {
		if *e.ManagerID == e.ID {
			return Employee{}, &ValidationError{Field: "manager_id", Message: "cannot manage self"}
		}
		if _, ok := s.employees[gkey(e.OrganizationID, *e.ManagerID)]; !ok {
			return Employee{}, fmt.Errorf("%w: manager %s", ErrNotFound, *e.ManagerID)
		}
	}
	if e.EmployeeNumber != cur.EmployeeNumber {
		if _, taken := s.empNumbers[e.OrganizationID][e.EmployeeNumber]; taken {
			return Employee{}, fmt.Errorf("%w: employee number %q", ErrAlreadyExists, e.EmployeeNumber)
		}
		delete(s.empNumbers[e.OrganizationID], cur.EmployeeNumber)
		s.empNumbers[e.OrganizationID][e.EmployeeNumber] = e.ID
	}
	if e.Email != cur.Email {
		if e.Email != "" {
			if _, taken := s.empEmails[e.OrganizationID][e.Email]; taken {
				return Employee{}, fmt.Errorf("%w: email %q", ErrAlreadyExists, e.Email)
			}
			s.empEmails[e.OrganizationID][e.Email] = e.ID
		}
		if cur.Email != "" {
			delete(s.empEmails[e.OrganizationID], cur.Email)
		}
	}
	cur.EmployeeNumber = e.EmployeeNumber
	cur.FirstName = e.FirstName
	cur.LastName = e.LastName
	cur.Email = e.Email
	cur.Position = e.Position
	cur.EmploymentType = e.EmploymentType
	cur.GroupID = copyStrPtr(e.GroupID)
	cur.ManagerID = copyStrPtr(e.ManagerID)
	cur.TimeZone = e.TimeZone
	cur.WorkStartDate = copyTimePtr(e.WorkStartDate)
	cur.Status = e.Status
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	s.employees[k] = cur
	return copyEmployee(cur), nil
}

func (s *MemoryStore) DeactivateEmployee(ctx context.Context, orgID, id string, version int64) (Employee, error) {
	if err := ctx.Err(); err != nil {
		return Employee{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := gkey(orgID, id)
	cur, ok := s.employees[k]
	if !ok {
		return Employee{}, fmt.Errorf("%w: employee %s", ErrNotFound, id)
	}
	if version != cur.Version {
		return Employee{}, fmt.Errorf("%w: employee %s", ErrVersionConflict, id)
	}
	cur.Status = InvitationRemoved
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	s.employees[k] = cur
	return copyEmployee(cur), nil
}

func (s *MemoryStore) ListEmployees(ctx context.Context, orgID string, f EmployeeFilter, p Page) ([]Employee, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Employee
	for _, e := range s.employees {
		if e.OrganizationID != orgID {
			continue
		}
		if f.GroupID != "" && (e.GroupID == nil || *e.GroupID != f.GroupID) {
			continue
		}
		if f.Status != "" && e.Status != f.Status {
			continue
		}
		if f.Search != "" {
			hay := strings.ToLower(e.FirstName + " " + e.LastName + " " + e.Email + " " + e.EmployeeNumber)
			if !strings.Contains(hay, strings.ToLower(f.Search)) {
				continue
			}
		}
		out = append(out, copyEmployee(e))
	}
	sortByCursor(out, func(e Employee) cursorKey { return cursorKey{e.CreatedAt, e.ID} })
	return paginate(out, func(e Employee) cursorKey { return cursorKey{e.CreatedAt, e.ID} }, p)
}

// ---------------------------------------------------------------------------
// Memberships
// ---------------------------------------------------------------------------

func (s *MemoryStore) SetRole(ctx context.Context, orgID, employeeID string, role Role, grantedBy string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !ValidRole(role) {
		return &ValidationError{Field: "role", Message: "unknown role"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.employees[gkey(orgID, employeeID)]
	if !ok {
		return fmt.Errorf("%w: employee %s", ErrNotFound, employeeID)
	}
	if e.Status != InvitationJoined {
		return fmt.Errorf("store: employee %s has not joined (status %s)", employeeID, e.Status)
	}
	k := gkey(orgID, employeeID)
	m := Membership{
		OrganizationID: orgID,
		EmployeeID:     employeeID,
		Role:           role,
		CreatedAt:      time.Now().UTC(),
	}
	if existing, ok := s.memberships[k]; ok {
		m.CreatedAt = existing.CreatedAt // keep original grant time on re-grant
	}
	if grantedBy != "" {
		m.GrantedBy = strPtr(grantedBy)
	}
	s.memberships[k] = m
	return nil
}

func (s *MemoryStore) GetRole(ctx context.Context, orgID, employeeID string) (Role, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.memberships[gkey(orgID, employeeID)]
	if !ok {
		return "", fmt.Errorf("%w: membership for employee %s", ErrNotFound, employeeID)
	}
	return m.Role, nil
}

func (s *MemoryStore) ListMemberships(ctx context.Context, orgID string, p Page) ([]Membership, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Membership
	for _, m := range s.memberships {
		if m.OrganizationID == orgID {
			out = append(out, copyMembership(m))
		}
	}
	sortByCursor(out, func(m Membership) cursorKey { return cursorKey{m.CreatedAt, m.EmployeeID} })
	return paginate(out, func(m Membership) cursorKey { return cursorKey{m.CreatedAt, m.EmployeeID} }, p)
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

func (s *MemoryStore) IssueCredential(ctx context.Context, c Credential, tokenPlaintext string) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	if err := c.Validate(); err != nil {
		return Credential{}, err
	}
	if tokenPlaintext == "" {
		return Credential{}, &ValidationError{Field: "token", Message: "plaintext token required"}
	}
	hash := credentialHash(tokenPlaintext)
	if c.ID == "" {
		c.ID = newID()
	}
	c.TokenHash = hash
	c.CreatedAt = time.Now().UTC()
	c.Version = 1

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[c.OrganizationID]; !ok {
		return Credential{}, fmt.Errorf("%w: organization %s", ErrNotFound, c.OrganizationID)
	}
	if c.EmployeeID != nil {
		if _, ok := s.employees[gkey(c.OrganizationID, *c.EmployeeID)]; !ok {
			return Credential{}, fmt.Errorf("%w: employee %s", ErrNotFound, *c.EmployeeID)
		}
	}
	if _, taken := s.credHashes[hash]; taken {
		return Credential{}, fmt.Errorf("%w: credential token", ErrAlreadyExists)
	}
	if _, ok := s.creds[gkey(c.OrganizationID, c.ID)]; ok {
		return Credential{}, fmt.Errorf("%w: credential %s", ErrAlreadyExists, c.ID)
	}
	s.creds[gkey(c.OrganizationID, c.ID)] = c
	s.credHashes[hash] = gkey(c.OrganizationID, c.ID)
	return copyCredential(c), nil
}

func (s *MemoryStore) GetCredentialByTokenHash(ctx context.Context, hash string) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.credHashes[hash]
	if !ok {
		return Credential{}, fmt.Errorf("%w: credential", ErrNotFound)
	}
	c := s.creds[k]
	if c.RevokedAt != nil {
		return Credential{}, fmt.Errorf("%w: credential %s revoked", ErrCredentialInvalid, c.ID)
	}
	if c.ExpiresAt != nil && !c.ExpiresAt.After(time.Now()) {
		return Credential{}, fmt.Errorf("%w: credential %s expired", ErrCredentialInvalid, c.ID)
	}
	return copyCredential(c), nil
}

func (s *MemoryStore) GetCredential(ctx context.Context, orgID, id string) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.creds[gkey(orgID, id)]
	if !ok {
		return Credential{}, fmt.Errorf("%w: credential %s", ErrNotFound, id)
	}
	return copyCredential(c), nil
}

func (s *MemoryStore) RevokeCredential(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := gkey(orgID, id)
	c, ok := s.creds[k]
	if !ok {
		return fmt.Errorf("%w: credential %s", ErrNotFound, id)
	}
	if c.RevokedAt == nil {
		now := time.Now().UTC()
		c.RevokedAt = &now
		s.creds[k] = c
	}
	return nil
}

func (s *MemoryStore) TouchCredential(ctx context.Context, orgID, id string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := gkey(orgID, id)
	c, ok := s.creds[k]
	if !ok {
		return fmt.Errorf("%w: credential %s", ErrNotFound, id)
	}
	c.LastUsedAt = &at
	s.creds[k] = c
	return nil
}

func (s *MemoryStore) ListCredentials(ctx context.Context, orgID string, p Page) ([]Credential, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Credential
	for _, c := range s.creds {
		if c.OrganizationID == orgID {
			out = append(out, copyCredential(c))
		}
	}
	sortByCursor(out, func(c Credential) cursorKey { return cursorKey{c.CreatedAt, c.ID} })
	return paginate(out, func(c Credential) cursorKey { return cursorKey{c.CreatedAt, c.ID} }, p)
}

// ---------------------------------------------------------------------------
// Audit log
// ---------------------------------------------------------------------------

func (s *MemoryStore) AppendAuditEvent(ctx context.Context, e AuditEvent) (AuditEvent, error) {
	if err := ctx.Err(); err != nil {
		return AuditEvent{}, err
	}
	if err := e.Validate(); err != nil {
		return AuditEvent{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[e.OrganizationID]; !ok {
		return AuditEvent{}, fmt.Errorf("%w: organization %s", ErrNotFound, e.OrganizationID)
	}
	e.ID = s.auditSeq.Add(1)
	e.CreatedAt = time.Now().UTC()
	e.PrevHash = s.auditLast[e.OrganizationID] // "" for the first event
	hash, err := computeAuditHash(e.PrevHash, e)
	if err != nil {
		return AuditEvent{}, err
	}
	e.Hash = hash
	s.audit = append(s.audit, e)
	s.auditLast[e.OrganizationID] = hash
	return copyAuditEvent(e), nil
}

func (s *MemoryStore) ListAuditEvents(ctx context.Context, orgID string, f AuditFilter, p Page) ([]AuditEvent, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []AuditEvent
	for _, e := range s.audit {
		if e.OrganizationID != orgID {
			continue
		}
		if f.ActorType != "" && e.ActorType != f.ActorType {
			continue
		}
		if f.Action != "" && e.Action != f.Action {
			continue
		}
		if f.ResourceType != "" && e.ResourceType != f.ResourceType {
			continue
		}
		if f.Since != nil && e.CreatedAt.Before(*f.Since) {
			continue
		}
		out = append(out, copyAuditEvent(e))
	}
	// s.audit is already in (id) order; sort by (created_at, id) for the cursor contract.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	key := func(e AuditEvent) cursorKey {
		return cursorKey{e.CreatedAt, fmt.Sprintf("%020d", e.ID)}
	}
	return paginate(out, key, p)
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

func (s *MemoryStore) IdempotencyGet(ctx context.Context, orgID, key string) (IdempotencyRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return IdempotencyRecord{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.idem[gkey(orgID, key)]
	if !ok {
		return IdempotencyRecord{}, false, nil
	}
	if time.Since(r.CreatedAt) > IdempotencyTTL {
		return IdempotencyRecord{}, false, nil
	}
	return copyIdempotencyRecord(r), true, nil
}

func (s *MemoryStore) IdempotencyPut(ctx context.Context, r IdempotencyRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.CreatedAt = time.Now().UTC()
	s.idem[gkey(r.OrganizationID, r.Key)] = r
	return nil
}
