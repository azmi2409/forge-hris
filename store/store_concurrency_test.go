package store

// Concurrency contract tests for the MemoryStore. Run with -race; every
// test here spawns parallel writers and then asserts the store's invariants
// still hold (uniqueness, hash-chain integrity, atomic upserts, and exactly-
// one-wins optimistic concurrency).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func newRaceOrg(t *testing.T, s *MemoryStore, slug string) (context.Context, Organization) {
	t.Helper()
	ctx := context.Background()
	org, err := s.CreateOrganization(ctx, Organization{
		Name: "Race " + slug, Slug: slug, CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	return ctx, org
}

// TestConcurrentEmployeeCreation: 32 goroutines create 10 employees each.
// All 320 must land with unique employee numbers, and a deliberate duplicate
// must fail deterministically with ErrAlreadyExists.
func TestConcurrentEmployeeCreation(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx, org := newRaceOrg(t, s, "race-emp")

	const workers = 32
	const perWorker = 10

	var wg sync.WaitGroup
	errCh := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				num := fmt.Sprintf("W%02d-E%03d", w, i)
				_, err := s.CreateEmployee(ctx, Employee{
					OrganizationID: org.ID,
					EmployeeNumber: num,
					FirstName:      "Racer",
					Email:          fmt.Sprintf("w%02d-e%03d@race.test", w, i),
				})
				if err != nil {
					errCh <- fmt.Errorf("worker %d create %d: %w", w, i, err)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	emps, _, err := s.ListEmployees(ctx, org.ID, EmployeeFilter{}, Page{Limit: MaxListLimit})
	if err != nil {
		t.Fatalf("ListEmployees: %v", err)
	}
	if len(emps) != workers*perWorker {
		t.Fatalf("want %d employees, got %d", workers*perWorker, len(emps))
	}
	seen := make(map[string]bool, len(emps))
	for _, e := range emps {
		if seen[e.EmployeeNumber] {
			t.Fatalf("duplicate employee_number %q", e.EmployeeNumber)
		}
		seen[e.EmployeeNumber] = true
	}

	// A duplicate number raced in after the fact must be rejected.
	if _, err := s.CreateEmployee(ctx, Employee{
		OrganizationID: org.ID,
		EmployeeNumber: "W00-E000",
		FirstName:      "Dupe",
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate number: want ErrAlreadyExists, got %v", err)
	}
}

// TestConcurrentAuditChain: 16 goroutines append 25 events each. The store
// serializes appends, so the resulting chain must be a single linear history:
// IDs 1..N exactly once, each PrevHash == previous Hash, first PrevHash == "",
// and every hash recomputes from its canonical body.
func TestConcurrentAuditChain(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx, org := newRaceOrg(t, s, "race-audit")

	const workers = 16
	const perWorker = 25
	total := workers * perWorker

	var wg sync.WaitGroup
	errCh := make(chan error, total)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				_, err := s.AppendAuditEvent(ctx, AuditEvent{
					OrganizationID: org.ID,
					ActorType:      ActorAgent,
					ActorID:        fmt.Sprintf("agent-%d", w),
					Action:         "test.append",
					ResourceType:   "test",
				})
				if err != nil {
					errCh <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	events, _, err := s.ListAuditEvents(ctx, org.ID, AuditFilter{}, Page{Limit: MaxListLimit})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(events) != total {
		t.Fatalf("want %d events, got %d", total, len(events))
	}
	// Order by ID (append order); created_at ties are broken by ID in the
	// listing, but sorting explicitly makes the invariant check airtight.
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	for i, ev := range events {
		if ev.ID != int64(i+1) {
			t.Fatalf("event %d: want ID %d, got %d (chain forked or lost events)", i, i+1, ev.ID)
		}
		if i == 0 {
			if ev.PrevHash != "" {
				t.Fatalf("first event PrevHash = %q, want empty", ev.PrevHash)
			}
		} else if ev.PrevHash != events[i-1].Hash {
			t.Fatalf("event %d: PrevHash %q != previous Hash %q", ev.ID, ev.PrevHash, events[i-1].Hash)
		}
		if want, err := computeAuditHash(ev.PrevHash, ev); err != nil || want != ev.Hash {
			t.Fatalf("event %d: hash does not recompute (want %q, err %v)", ev.ID, want, err)
		}
	}
}

// TestConcurrentIdempotencyPutGet: 32 goroutines upsert the same key with
// distinct payloads while readers hammer Get. A put replaces the whole record
// atomically, so the final record must be exactly one writer's payload —
// never a torn mix — and every observed record must be self-consistent.
func TestConcurrentIdempotencyPutGet(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx, org := newRaceOrg(t, s, "race-idem")

	const writers = 32
	const readers = 8

	var wg sync.WaitGroup
	errCh := make(chan error, writers+readers*50)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rec := IdempotencyRecord{
				OrganizationID: org.ID,
				Key:            "shared-key",
				Fingerprint:    fmt.Sprintf("fp-%d", w),
				StatusCode:     200 + w,
				ResponseBody:   []byte(fmt.Sprintf("body-%d", w)),
			}
			if err := s.IdempotencyPut(ctx, rec); err != nil {
				errCh <- err
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				rec, found, err := s.IdempotencyGet(ctx, org.ID, "shared-key")
				if err != nil {
					errCh <- err
					return
				}
				if !found {
					continue // a writer may not have landed yet
				}
				if !checkIdemConsistent(rec) {
					errCh <- fmt.Errorf("torn idempotency record: %+v", rec)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	rec, found, err := s.IdempotencyGet(ctx, org.ID, "shared-key")
	if err != nil || !found {
		t.Fatalf("IdempotencyGet final: %v found=%v", err, found)
	}
	if !checkIdemConsistent(rec) {
		t.Fatalf("final idempotency record torn: %+v", rec)
	}
	if rec.StatusCode < 200 || rec.StatusCode >= 200+writers {
		t.Fatalf("final record not from any writer: %+v", rec)
	}
}

func checkIdemConsistent(rec IdempotencyRecord) bool {
	n := rec.StatusCode - 200
	return rec.Fingerprint == fmt.Sprintf("fp-%d", n) &&
		string(rec.ResponseBody) == fmt.Sprintf("body-%d", n)
}

// TestOptimisticConcurrencyRace: two updaters read the same version and race.
// Exactly one must win; the loser must get ErrVersionConflict.
func TestOptimisticConcurrencyRace(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx, org := newRaceOrg(t, s, "race-occ")

	// Two stale copies at version 1.
	a, b := org, org
	a.Name = "Winner A"
	b.Name = "Winner B"

	start := make(chan struct{})
	var wins, conflicts atomic.Int64
	var wg sync.WaitGroup
	for _, upd := range []Organization{a, b} {
		wg.Add(1)
		go func(u Organization) {
			defer wg.Done()
			<-start
			if _, err := s.UpdateOrganization(ctx, u); err != nil {
				if errors.Is(err, ErrVersionConflict) {
					conflicts.Add(1)
				} else {
					t.Errorf("UpdateOrganization: unexpected error %v", err)
				}
				return
			}
			wins.Add(1)
		}(upd)
	}
	close(start)
	wg.Wait()

	if wins.Load() != 1 || conflicts.Load() != 1 {
		t.Fatalf("want exactly 1 win and 1 conflict, got wins=%d conflicts=%d",
			wins.Load(), conflicts.Load())
	}
	final, err := s.GetOrganization(ctx, org.ID)
	if err != nil {
		t.Fatalf("GetOrganization: %v", err)
	}
	if final.Version != 2 {
		t.Fatalf("want version 2 after one win, got %d", final.Version)
	}
	if final.Name != "Winner A" && final.Name != "Winner B" {
		t.Fatalf("unexpected final name %q", final.Name)
	}
}

// TestConcurrentCredentialIssue: duplicate token hashes raced from many
// goroutines must yield exactly one winner; the rest get ErrAlreadyExists.
func TestConcurrentCredentialIssue(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx, org := newRaceOrg(t, s, "race-cred")

	emp, err := s.CreateEmployee(ctx, Employee{
		OrganizationID: org.ID,
		EmployeeNumber: "E1",
		FirstName:      "Pat",
		Status:         InvitationJoined,
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}

	const racers = 16
	start := make(chan struct{})
	var issued, dupes atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.IssueCredential(ctx, Credential{
				OrganizationID: org.ID,
				Kind:           CredentialPAT,
				Name:           fmt.Sprintf("racer-%d", i),
				EmployeeID:     &emp.ID,
			}, "same-plaintext-token")
			switch {
			case err == nil:
				issued.Add(1)
			case errors.Is(err, ErrAlreadyExists):
				dupes.Add(1)
			default:
				t.Errorf("IssueCredential: unexpected error %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if issued.Load() != 1 || dupes.Load() != racers-1 {
		t.Fatalf("want 1 issued and %d dupes, got issued=%d dupes=%d",
			racers-1, issued.Load(), dupes.Load())
	}
}

// TestGetCredential round-trips the new admin read path: it returns the
// stored credential (including revoked ones, for the audit trail) and 404s
// across organizations.
func TestGetCredential(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx, orgA := newRaceOrg(t, s, "race-getcred-a")
	_, orgB := newRaceOrg(t, s, "race-getcred-b")

	emp, err := s.CreateEmployee(ctx, Employee{
		OrganizationID: orgA.ID, EmployeeNumber: "E1", FirstName: "Pat",
		Status: InvitationJoined,
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	cred, err := s.IssueCredential(ctx, Credential{
		OrganizationID: orgA.ID, Kind: CredentialPAT, Name: "cli", EmployeeID: &emp.ID,
	}, "getcred-token")
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}

	got, err := s.GetCredential(ctx, orgA.ID, cred.ID)
	if err != nil || got.ID != cred.ID || got.Name != "cli" {
		t.Fatalf("GetCredential: %v %+v", err, got)
	}
	// The hash must never escape through serialization (json:"-").
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), "token_hash") {
		t.Fatalf("GetCredential serialized output leaks token_hash: %s", raw)
	}
	// Cross-org lookup must not leak.
	if _, err := s.GetCredential(ctx, orgB.ID, cred.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetCredential cross-org: want ErrNotFound, got %v", err)
	}
	// Revoked credentials are still readable (admin/audit path).
	if err := s.RevokeCredential(ctx, orgA.ID, cred.ID); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}
	if _, err := s.GetCredential(ctx, orgA.ID, cred.ID); err != nil {
		t.Fatalf("GetCredential revoked: %v", err)
	}
}
