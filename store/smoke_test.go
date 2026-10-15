package store

import (
	"errors"
	"testing"
	"time"
)

// TestSmoke exercises the full component-1 happy path on the MemoryStore:
// organization -> group -> employee -> role -> credential -> audit event ->
// listings. The comprehensive contract suite is owned by the tests agent;
// this file only proves the store wires together.
func TestSmoke(t *testing.T) {
	ctx := t.Context()
	s := NewMemoryStore()

	org, err := s.CreateOrganization(ctx, Organization{
		Name: "Acme Sdn Bhd", Slug: "acme", CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	if org.Version != 1 || org.ID == "" {
		t.Fatalf("CreateOrganization: bad defaults: %+v", org)
	}
	if _, err := s.CreateOrganization(ctx, Organization{Name: "Dupe", Slug: "acme", CountryCode: "MY"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("CreateOrganization duplicate slug: want ErrAlreadyExists, got %v", err)
	}
	got, err := s.GetOrganizationBySlug(ctx, "acme")
	if err != nil || got.ID != org.ID {
		t.Fatalf("GetOrganizationBySlug: %v %+v", err, got)
	}

	grp, err := s.CreateGroup(ctx, Group{OrganizationID: org.ID, Name: "Engineering"})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	child, err := s.CreateGroup(ctx, Group{OrganizationID: org.ID, Name: "Backend", ParentID: &grp.ID})
	if err != nil {
		t.Fatalf("CreateGroup child: %v", err)
	}
	// Cycle: make the parent a child of its own child.
	grp.ParentID = &child.ID
	if _, err := s.UpdateGroup(ctx, grp); err == nil {
		t.Fatalf("UpdateGroup cycle: want error, got nil")
	}

	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	emp, err := s.CreateEmployee(ctx, Employee{
		OrganizationID: org.ID,
		EmployeeNumber: "E001",
		FirstName:      "Azmi",
		LastName:       "Muwahid",
		Email:          "azmi@example.com",
		Position:       "Tech Lead",
		EmploymentType: EmploymentFullTime,
		GroupID:        &grp.ID,
		TimeZone:       "Asia/Jakarta",
		WorkStartDate:  &start,
		Status:         InvitationJoined,
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	if _, err := s.CreateEmployee(ctx, Employee{
		OrganizationID: org.ID, EmployeeNumber: "E001", FirstName: "Dupe",
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("CreateEmployee duplicate number: want ErrAlreadyExists, got %v", err)
	}

	if err := s.SetRole(ctx, org.ID, emp.ID, RoleAdmin, "system"); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	role, err := s.GetRole(ctx, org.ID, emp.ID)
	if err != nil || role != RoleAdmin {
		t.Fatalf("GetRole: %v %q", err, role)
	}

	cred, err := s.IssueCredential(ctx, Credential{
		OrganizationID: org.ID,
		Kind:           CredentialPAT,
		Name:           "cli",
		EmployeeID:     &emp.ID,
	}, "super-secret-token")
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}
	if cred.TokenHash == "" || cred.TokenHash == "super-secret-token" {
		t.Fatalf("IssueCredential: token not hashed: %q", cred.TokenHash)
	}
	gotCred, err := s.GetCredentialByTokenHash(ctx, cred.TokenHash)
	if err != nil || gotCred.ID != cred.ID {
		t.Fatalf("GetCredentialByTokenHash: %v %+v", err, gotCred)
	}
	if err := s.TouchCredential(ctx, org.ID, cred.ID, time.Now()); err != nil {
		t.Fatalf("TouchCredential: %v", err)
	}
	if err := s.RevokeCredential(ctx, org.ID, cred.ID); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}
	if _, err := s.GetCredentialByTokenHash(ctx, cred.TokenHash); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("GetCredentialByTokenHash revoked: want ErrCredentialInvalid, got %v", err)
	}

	ev1, err := s.AppendAuditEvent(ctx, AuditEvent{
		OrganizationID: org.ID,
		ActorType:      ActorHuman,
		ActorID:        emp.ID,
		Action:         "employee.created",
		ResourceType:   "employee",
		ResourceID:     emp.ID,
	})
	if err != nil {
		t.Fatalf("AppendAuditEvent: %v", err)
	}
	if ev1.ID == 0 || ev1.Hash == "" || ev1.PrevHash != "" {
		t.Fatalf("AppendAuditEvent first: bad chain fields: %+v", ev1)
	}
	ev2, err := s.AppendAuditEvent(ctx, AuditEvent{
		OrganizationID: org.ID,
		ActorType:      ActorHuman,
		ActorID:        emp.ID,
		Action:         "role.granted",
		ResourceType:   "membership",
		ResourceID:     emp.ID,
	})
	if err != nil {
		t.Fatalf("AppendAuditEvent 2: %v", err)
	}
	if ev2.PrevHash != ev1.Hash {
		t.Fatalf("audit chain broken: prev %q != %q", ev2.PrevHash, ev1.Hash)
	}

	// Version conflict path.
	stale := emp
	stale.Version = 999
	stale.Position = "CTO"
	if _, err := s.UpdateEmployee(ctx, stale); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("UpdateEmployee stale: want ErrVersionConflict, got %v", err)
	}
	emp.Position = "CTO"
	emp2, err := s.UpdateEmployee(ctx, emp)
	if err != nil || emp2.Version != 2 {
		t.Fatalf("UpdateEmployee: %v %+v", err, emp2)
	}
	dead, err := s.DeactivateEmployee(ctx, org.ID, emp.ID, emp2.Version)
	if err != nil || dead.Status != InvitationRemoved {
		t.Fatalf("DeactivateEmployee: %v %+v", err, dead)
	}

	// Listings.
	if _, _, err := s.ListOrganizations(ctx, Page{Limit: 10}); err != nil {
		t.Fatalf("ListOrganizations: %v", err)
	}
	groups, next, err := s.ListGroups(ctx, org.ID, Page{Limit: 1})
	if err != nil || len(groups) != 1 || next == "" {
		t.Fatalf("ListGroups page1: %v len=%d next=%q", err, len(groups), next)
	}
	groups2, next2, err := s.ListGroups(ctx, org.ID, Page{Limit: 1, Cursor: next})
	if err != nil || len(groups2) != 1 || next2 != "" || groups2[0].ID == groups[0].ID {
		t.Fatalf("ListGroups page2: %v len=%d next=%q", err, len(groups2), next2)
	}
	emps, _, err := s.ListEmployees(ctx, org.ID, EmployeeFilter{Search: "azmi"}, Page{})
	if err != nil || len(emps) != 1 {
		t.Fatalf("ListEmployees search: %v len=%d", err, len(emps))
	}
	members, _, err := s.ListMemberships(ctx, org.ID, Page{})
	if err != nil || len(members) != 1 {
		t.Fatalf("ListMemberships: %v len=%d", err, len(members))
	}
	creds, _, err := s.ListCredentials(ctx, org.ID, Page{})
	if err != nil || len(creds) != 1 {
		t.Fatalf("ListCredentials: %v len=%d", err, len(creds))
	}
	events, _, err := s.ListAuditEvents(ctx, org.ID, AuditFilter{Action: "role.granted"}, Page{})
	if err != nil || len(events) != 1 || events[0].ID != ev2.ID {
		t.Fatalf("ListAuditEvents filter: %v len=%d", err, len(events))
	}

	// Idempotency round trip.
	rec := IdempotencyRecord{
		OrganizationID: org.ID,
		Key:            "req-1",
		Fingerprint:    "abc",
		StatusCode:     201,
		ResponseBody:   []byte(`{"ok":true}`),
	}
	if err := s.IdempotencyPut(ctx, rec); err != nil {
		t.Fatalf("IdempotencyPut: %v", err)
	}
	back, found, err := s.IdempotencyGet(ctx, org.ID, "req-1")
	if err != nil || !found || back.StatusCode != 201 || string(back.ResponseBody) != `{"ok":true}` {
		t.Fatalf("IdempotencyGet: %v found=%v %+v", err, found, back)
	}
	if _, found, err := s.IdempotencyGet(ctx, org.ID, "nope"); err != nil || found {
		t.Fatalf("IdempotencyGet missing: %v found=%v", err, found)
	}
}
