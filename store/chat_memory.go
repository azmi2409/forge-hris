package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func copySlackWorkspace(w SlackWorkspace) SlackWorkspace { return w }

func copyTeamsConfig(c TeamsConfig) TeamsConfig { return c }

func copyESSToken(t ESSToken) ESSToken {
	if t.UsedAt != nil {
		u := *t.UsedAt
		t.UsedAt = &u
	}
	return t
}

// ---------------------------------------------------------------------------
// Slack workspaces
// ---------------------------------------------------------------------------

func (s *MemoryStore) SaveSlackWorkspace(ctx context.Context, w SlackWorkspace) (SlackWorkspace, error) {
	if err := ctx.Err(); err != nil {
		return SlackWorkspace{}, err
	}
	if err := w.Validate(); err != nil {
		return SlackWorkspace{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[w.OrganizationID]; !ok {
		return SlackWorkspace{}, fmt.Errorf("%w: organization %s", ErrNotFound, w.OrganizationID)
	}
	now := time.Now().UTC()
	if cur, ok := s.slackWorkspaces[w.TeamID]; ok {
		if w.Version != cur.Version {
			return SlackWorkspace{}, fmt.Errorf("%w: slack workspace %s", ErrVersionConflict, w.TeamID)
		}
		w.Version = cur.Version + 1
		w.CreatedAt = cur.CreatedAt
		w.UpdatedAt = now
	} else {
		w.Version = 1
		w.CreatedAt = now
		w.UpdatedAt = now
	}
	s.slackWorkspaces[w.TeamID] = w
	return copySlackWorkspace(w), nil
}

func (s *MemoryStore) GetSlackWorkspace(ctx context.Context, teamID string) (SlackWorkspace, error) {
	if err := ctx.Err(); err != nil {
		return SlackWorkspace{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.slackWorkspaces[teamID]
	if !ok {
		return SlackWorkspace{}, fmt.Errorf("%w: slack workspace %s", ErrNotFound, teamID)
	}
	return copySlackWorkspace(w), nil
}

func (s *MemoryStore) DeleteSlackWorkspace(ctx context.Context, teamID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.slackWorkspaces[teamID]; !ok {
		return fmt.Errorf("%w: slack workspace %s", ErrNotFound, teamID)
	}
	delete(s.slackWorkspaces, teamID)
	for k := range s.slackUserMap {
		if strings.HasPrefix(k, teamID+"\x00") {
			delete(s.slackUserMap, k)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Slack user identity map
// ---------------------------------------------------------------------------

func (s *MemoryStore) MapSlackUser(ctx context.Context, teamID, slackUserID, employeeID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(slackUserID) == "" {
		return &ValidationError{Field: "slack_user_id", Message: "required"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.slackWorkspaces[teamID]
	if !ok {
		return fmt.Errorf("%w: slack workspace %s", ErrNotFound, teamID)
	}
	emp, ok := s.employees[gkey(ws.OrganizationID, employeeID)]
	if !ok || emp.OrganizationID != ws.OrganizationID {
		return fmt.Errorf("%w: employee %s", ErrNotFound, employeeID)
	}
	s.slackUserMap[teamID+"\x00"+slackUserID] = employeeID
	return nil
}

func (s *MemoryStore) GetSlackUserEmployee(ctx context.Context, teamID, slackUserID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	empID, ok := s.slackUserMap[teamID+"\x00"+slackUserID]
	if !ok {
		return "", fmt.Errorf("%w: slack user %s", ErrNotFound, slackUserID)
	}
	return empID, nil
}

func (s *MemoryStore) UnmapSlackUser(ctx context.Context, teamID, slackUserID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.slackUserMap, teamID+"\x00"+slackUserID)
	return nil
}

// ---------------------------------------------------------------------------
// Teams configs
// ---------------------------------------------------------------------------

func (s *MemoryStore) SaveTeamsConfig(ctx context.Context, c TeamsConfig) (TeamsConfig, error) {
	if err := ctx.Err(); err != nil {
		return TeamsConfig{}, err
	}
	if err := c.Validate(); err != nil {
		return TeamsConfig{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[c.OrganizationID]; !ok {
		return TeamsConfig{}, fmt.Errorf("%w: organization %s", ErrNotFound, c.OrganizationID)
	}
	now := time.Now().UTC()
	if cur, ok := s.teamsConfigs[c.OrganizationID]; ok {
		if c.Version != cur.Version {
			return TeamsConfig{}, fmt.Errorf("%w: teams config %s", ErrVersionConflict, c.OrganizationID)
		}
		c.Version = cur.Version + 1
		c.CreatedAt = cur.CreatedAt
		c.UpdatedAt = now
	} else {
		c.Version = 1
		c.CreatedAt = now
		c.UpdatedAt = now
	}
	s.teamsConfigs[c.OrganizationID] = c
	return copyTeamsConfig(c), nil
}

func (s *MemoryStore) GetTeamsConfig(ctx context.Context, orgID string) (TeamsConfig, error) {
	if err := ctx.Err(); err != nil {
		return TeamsConfig{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.teamsConfigs[orgID]
	if !ok {
		return TeamsConfig{}, fmt.Errorf("%w: teams config %s", ErrNotFound, orgID)
	}
	return copyTeamsConfig(c), nil
}

func (s *MemoryStore) ListTeamsConfigs(ctx context.Context, p Page) ([]TeamsConfig, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []TeamsConfig
	for _, c := range s.teamsConfigs {
		out = append(out, copyTeamsConfig(c))
	}
	sortByCursor(out, func(c TeamsConfig) cursorKey { return cursorKey{c.CreatedAt, c.OrganizationID} })
	return paginate(out, func(c TeamsConfig) cursorKey { return cursorKey{c.CreatedAt, c.OrganizationID} }, p)
}

func (s *MemoryStore) DeleteTeamsConfig(ctx context.Context, orgID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.teamsConfigs[orgID]; !ok {
		return fmt.Errorf("%w: teams config %s", ErrNotFound, orgID)
	}
	delete(s.teamsConfigs, orgID)
	for k := range s.teamsUserMap {
		if strings.HasPrefix(k, orgID+"\x00") {
			delete(s.teamsUserMap, k)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Teams user identity map
// ---------------------------------------------------------------------------

func (s *MemoryStore) MapTeamsUser(ctx context.Context, orgID, teamsUserID, employeeID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(teamsUserID) == "" {
		return &ValidationError{Field: "teams_user_id", Message: "required"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[orgID]; !ok {
		return fmt.Errorf("%w: organization %s", ErrNotFound, orgID)
	}
	emp, ok := s.employees[gkey(orgID, employeeID)]
	if !ok || emp.OrganizationID != orgID {
		return fmt.Errorf("%w: employee %s", ErrNotFound, employeeID)
	}
	s.teamsUserMap[orgID+"\x00"+teamsUserID] = employeeID
	return nil
}

func (s *MemoryStore) GetTeamsUserEmployee(ctx context.Context, orgID, teamsUserID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	empID, ok := s.teamsUserMap[orgID+"\x00"+teamsUserID]
	if !ok {
		return "", fmt.Errorf("%w: teams user %s", ErrNotFound, teamsUserID)
	}
	return empID, nil
}

func (s *MemoryStore) UnmapTeamsUser(ctx context.Context, orgID, teamsUserID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.teamsUserMap, orgID+"\x00"+teamsUserID)
	return nil
}

// ---------------------------------------------------------------------------
// ESS magic-link tokens
// ---------------------------------------------------------------------------

func (s *MemoryStore) IssueESSToken(ctx context.Context, t ESSToken) (ESSToken, error) {
	if err := ctx.Err(); err != nil {
		return ESSToken{}, err
	}
	if err := t.Validate(); err != nil {
		return ESSToken{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[t.OrganizationID]; !ok {
		return ESSToken{}, fmt.Errorf("%w: organization %s", ErrNotFound, t.OrganizationID)
	}
	emp, ok := s.employees[gkey(t.OrganizationID, t.EmployeeID)]
	if !ok || emp.OrganizationID != t.OrganizationID {
		return ESSToken{}, fmt.Errorf("%w: employee %s", ErrNotFound, t.EmployeeID)
	}
	if _, ok := s.essTokens[t.TokenHash]; ok {
		return ESSToken{}, fmt.Errorf("%w: ess token", ErrAlreadyExists)
	}
	t.CreatedAt = time.Now().UTC()
	t.UsedAt = nil
	s.essTokens[t.TokenHash] = t
	return copyESSToken(t), nil
}

func (s *MemoryStore) GetESSToken(ctx context.Context, tokenHash string) (ESSToken, error) {
	if err := ctx.Err(); err != nil {
		return ESSToken{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.essTokens[tokenHash]
	if !ok {
		return ESSToken{}, fmt.Errorf("%w: ess token", ErrNotFound)
	}
	return copyESSToken(t), nil
}

func (s *MemoryStore) MarkESSTokenUsed(ctx context.Context, tokenHash string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.essTokens[tokenHash]
	if !ok {
		return fmt.Errorf("%w: ess token", ErrNotFound)
	}
	if t.UsedAt == nil {
		u := at.UTC()
		t.UsedAt = &u
		s.essTokens[tokenHash] = t
	}
	return nil
}

func (s *MemoryStore) DeleteExpiredESSTokens(ctx context.Context, before time.Time) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for h, t := range s.essTokens {
		if t.ExpiresAt.Before(before) {
			delete(s.essTokens, h)
			n++
		}
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Cross-org employee lookup (ESS login)
// ---------------------------------------------------------------------------

func (s *MemoryStore) FindEmployeeByEmail(ctx context.Context, email string) (string, Employee, error) {
	if err := ctx.Err(); err != nil {
		return "", Employee{}, err
	}
	want := strings.ToLower(strings.TrimSpace(email))
	if want == "" {
		return "", Employee{}, &ValidationError{Field: "email", Message: "required"}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var (
		orgID string
		emp   Employee
		found bool
	)
	// The empEmails index keys exact-case emails, so login does its own
	// case-insensitive scan: logins are infrequent, correctness matters.
	for _, e := range s.employees {
		if e.Status != InvitationJoined || !strings.EqualFold(e.Email, want) {
			continue
		}
		if found && emp.OrganizationID != e.OrganizationID {
			return "", Employee{}, fmt.Errorf("store: email %q is ambiguous across organizations", email)
		}
		if !found {
			orgID, emp, found = e.OrganizationID, copyEmployee(e), true
		}
	}
	if !found {
		return "", Employee{}, fmt.Errorf("%w: employee with email %q", ErrNotFound, email)
	}
	return orgID, emp, nil
}
