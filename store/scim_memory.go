package store

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemoryStore SCIM link implementation (component 4, Worker C).
//
// The registry is NOT a field on MemoryStore: the struct is owned by the
// component-1 worker and this file must not touch it. Instead each
// *MemoryStore gets its own registry, keyed by pointer in a process-wide
// sync.Map. Registries are independent per store instance, so tests that
// build fresh stores never see each other's links; entries live as long as
// the store does, which is the whole process in tests.

// scimLinkRegistry is the per-MemoryStore SCIM link table, mirroring the
// scim_links Postgres table (see migrations/005_scim.sql).
type scimLinkRegistry struct {
	mu    sync.Mutex
	byExt map[string]SCIMLink // orgID + "\x00" + externalID -> link
	byEmp map[string]SCIMLink // orgID + "\x00" + employeeID -> link
}

// scimLinkRegistries attaches one registry to each *MemoryStore.
var scimLinkRegistries sync.Map // *MemoryStore -> *scimLinkRegistry

func (s *MemoryStore) scimReg() *scimLinkRegistry {
	reg, _ := scimLinkRegistries.LoadOrStore(s, &scimLinkRegistry{
		byExt: make(map[string]SCIMLink),
		byEmp: make(map[string]SCIMLink),
	})
	return reg.(*scimLinkRegistry)
}

// Compile-time contract check.
var _ SCIMLinkStore = (*MemoryStore)(nil)

func (s *MemoryStore) CreateSCIMLink(ctx context.Context, l SCIMLink) (SCIMLink, error) {
	if err := ctx.Err(); err != nil {
		return SCIMLink{}, err
	}
	if err := l.Validate(); err != nil {
		return SCIMLink{}, err
	}
	// Existence checks under the store lock; the registry mutation follows
	// under its own lock. Employees are never hard-deleted, so no TOCTOU
	// hazard between the check and the insert.
	s.mu.RLock()
	_, orgOK := s.orgs[l.OrganizationID]
	_, empOK := s.employees[gkey(l.OrganizationID, l.EmployeeID)]
	s.mu.RUnlock()
	if !orgOK {
		return SCIMLink{}, fmt.Errorf("%w: organization %s", ErrNotFound, l.OrganizationID)
	}
	if !empOK {
		return SCIMLink{}, fmt.Errorf("%w: employee %s", ErrNotFound, l.EmployeeID)
	}
	reg := s.scimReg()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	extKey := gkey(l.OrganizationID, l.ExternalID)
	if _, taken := reg.byExt[extKey]; taken {
		return SCIMLink{}, fmt.Errorf("%w: scim externalId %q", ErrAlreadyExists, l.ExternalID)
	}
	empKey := gkey(l.OrganizationID, l.EmployeeID)
	if _, taken := reg.byEmp[empKey]; taken {
		return SCIMLink{}, fmt.Errorf("%w: employee %s already has a scim link", ErrAlreadyExists, l.EmployeeID)
	}
	l.CreatedAt = time.Now().UTC()
	reg.byExt[extKey] = l
	reg.byEmp[empKey] = l
	return l, nil
}

func (s *MemoryStore) GetSCIMLinkByExternalID(ctx context.Context, orgID, externalID string) (SCIMLink, error) {
	if err := ctx.Err(); err != nil {
		return SCIMLink{}, err
	}
	reg := s.scimReg()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	l, ok := reg.byExt[gkey(orgID, externalID)]
	if !ok {
		return SCIMLink{}, fmt.Errorf("%w: scim externalId %q", ErrNotFound, externalID)
	}
	return l, nil
}

func (s *MemoryStore) GetSCIMLinkByEmployee(ctx context.Context, orgID, employeeID string) (SCIMLink, error) {
	if err := ctx.Err(); err != nil {
		return SCIMLink{}, err
	}
	reg := s.scimReg()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	l, ok := reg.byEmp[gkey(orgID, employeeID)]
	if !ok {
		return SCIMLink{}, fmt.Errorf("%w: scim link for employee %s", ErrNotFound, employeeID)
	}
	return l, nil
}

func (s *MemoryStore) DeleteSCIMLinkByEmployee(ctx context.Context, orgID, employeeID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	reg := s.scimReg()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	l, ok := reg.byEmp[gkey(orgID, employeeID)]
	if !ok {
		return nil // idempotent
	}
	delete(reg.byEmp, gkey(orgID, employeeID))
	delete(reg.byExt, gkey(orgID, l.ExternalID))
	return nil
}
