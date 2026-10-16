package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// copyXeroConnection deep-copies a connection (all fields are value types).
func copyXeroConnection(c XeroConnection) XeroConnection { return c }

// copyXeroBillPosting deep-copies a posting (all fields are value types).
func copyXeroBillPosting(p XeroBillPosting) XeroBillPosting { return p }

// ---------------------------------------------------------------------------
// Xero connections
// ---------------------------------------------------------------------------

func (s *MemoryStore) GetXeroConnection(ctx context.Context, orgID string) (XeroConnection, error) {
	if err := ctx.Err(); err != nil {
		return XeroConnection{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.xeroConns[orgID]
	if !ok {
		return XeroConnection{}, fmt.Errorf("%w: xero connection", ErrNotFound)
	}
	return copyXeroConnection(c), nil
}

// SaveXeroConnection upserts the connection. A zero Version inserts a new
// row; a nonzero Version updates iff it matches the stored version.
func (s *MemoryStore) SaveXeroConnection(ctx context.Context, c XeroConnection) (XeroConnection, error) {
	if err := ctx.Err(); err != nil {
		return XeroConnection{}, err
	}
	if err := c.Validate(); err != nil {
		return XeroConnection{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[c.OrganizationID]; !ok {
		return XeroConnection{}, fmt.Errorf("%w: organization %s", ErrNotFound, c.OrganizationID)
	}
	now := time.Now().UTC()
	if cur, ok := s.xeroConns[c.OrganizationID]; ok {
		if c.Version == 0 {
			return XeroConnection{}, fmt.Errorf("%w: xero connection already exists", ErrAlreadyExists)
		}
		if c.Version != cur.Version {
			return XeroConnection{}, fmt.Errorf("%w: xero connection", ErrVersionConflict)
		}
		c.Version = cur.Version + 1
		c.CreatedAt = cur.CreatedAt
		c.UpdatedAt = now
	} else {
		if c.Version != 0 {
			return XeroConnection{}, fmt.Errorf("%w: xero connection", ErrVersionConflict)
		}
		c.Version = 1
		c.CreatedAt = now
		c.UpdatedAt = now
	}
	s.xeroConns[c.OrganizationID] = copyXeroConnection(c)
	return copyXeroConnection(c), nil
}

func (s *MemoryStore) DeleteXeroConnection(ctx context.Context, orgID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.xeroConns, orgID)
	// Mappings die with the connection (mirrors the Postgres ON DELETE
	// CASCADE on organization_id); the posting ledger is kept for audit.
	for k, m := range s.xeroAcctMaps {
		if m.OrganizationID == orgID {
			delete(s.xeroAcctMaps, k)
		}
	}
	for k, m := range s.xeroContMaps {
		if m.OrganizationID == orgID {
			delete(s.xeroContMaps, k)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Xero mappings
// ---------------------------------------------------------------------------

func (s *MemoryStore) SaveXeroMappings(ctx context.Context, orgID string, accounts []XeroAccountMapping, contacts []XeroContactMapping) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, a := range accounts {
		a.OrganizationID = orgID
		if err := a.Validate(); err != nil {
			return err
		}
	}
	for _, c := range contacts {
		c.OrganizationID = orgID
		if err := c.Validate(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[orgID]; !ok {
		return fmt.Errorf("%w: organization %s", ErrNotFound, orgID)
	}
	// Replace both sets atomically under the one lock.
	for k, m := range s.xeroAcctMaps {
		if m.OrganizationID == orgID {
			delete(s.xeroAcctMaps, k)
		}
	}
	for k, m := range s.xeroContMaps {
		if m.OrganizationID == orgID {
			delete(s.xeroContMaps, k)
		}
	}
	for _, a := range accounts {
		a.OrganizationID = orgID
		s.xeroAcctMaps[gkey(orgID, a.ComponentKey)] = a
	}
	for _, c := range contacts {
		c.OrganizationID = orgID
		s.xeroContMaps[gkey(orgID, c.EmployeeID)] = c
	}
	return nil
}

func (s *MemoryStore) GetXeroMappings(ctx context.Context, orgID string) ([]XeroAccountMapping, []XeroContactMapping, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var accounts []XeroAccountMapping
	var contacts []XeroContactMapping
	for _, m := range s.xeroAcctMaps {
		if m.OrganizationID == orgID {
			accounts = append(accounts, m)
		}
	}
	for _, m := range s.xeroContMaps {
		if m.OrganizationID == orgID {
			contacts = append(contacts, m)
		}
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ComponentKey < accounts[j].ComponentKey })
	sort.Slice(contacts, func(i, j int) bool { return contacts[i].EmployeeID < contacts[j].EmployeeID })
	return accounts, contacts, nil
}

// ---------------------------------------------------------------------------
// Xero bill postings
// ---------------------------------------------------------------------------

func (s *MemoryStore) RecordBillPosting(ctx context.Context, p XeroBillPosting) (XeroBillPosting, error) {
	if err := ctx.Err(); err != nil {
		return XeroBillPosting{}, err
	}
	if err := p.Validate(); err != nil {
		return XeroBillPosting{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[p.OrganizationID]; !ok {
		return XeroBillPosting{}, fmt.Errorf("%w: organization %s", ErrNotFound, p.OrganizationID)
	}
	idxKey := p.OrganizationID + "\x00" + p.RunID + "\x00" + p.EmployeeID
	if _, ok := s.xeroPostIndex[idxKey]; ok {
		return XeroBillPosting{}, fmt.Errorf("%w: bill posting for run %s employee %s", ErrAlreadyExists, p.RunID, p.EmployeeID)
	}
	if p.ID == "" {
		p.ID = newID()
	}
	p.Version = 1
	p.CreatedAt = time.Now().UTC()
	s.xeroPostings[p.ID] = copyXeroBillPosting(p)
	s.xeroPostIndex[idxKey] = p.ID
	return copyXeroBillPosting(p), nil
}

func (s *MemoryStore) ListBillPostings(ctx context.Context, orgID, runID string) ([]XeroBillPosting, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []XeroBillPosting
	for _, p := range s.xeroPostings {
		if p.OrganizationID == orgID && p.RunID == runID {
			out = append(out, copyXeroBillPosting(p))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
