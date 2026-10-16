package xero

import (
	"context"
	"sync"
)

// FakeClient is a scriptable XeroClient for tests. Program the result
// fields; errors take precedence over results when both are set. All calls
// are recorded so tests can assert what the handler attempted.
type FakeClient struct {
	mu sync.Mutex

	ExchangeTokens Tokens
	ExchangeErr    error
	RefreshTokens_ Tokens //nolint:revive // trailing underscore avoids clashing with the method name
	RefreshErr     error
	BillResult     BillResult
	BillErr        error
	Tenants        []Tenant
	TenantsErr     error

	ExchangeCalls []fakeExchangeCall
	RefreshCalls  []string // refresh tokens presented
	BillCalls     []fakeBillCall
	TenantCalls   int
}

type fakeExchangeCall struct {
	Code        string
	RedirectURI string
}

type fakeBillCall struct {
	AccessToken string
	TenantID    string
	Bill        Bill
}

func (f *FakeClient) ExchangeCode(ctx context.Context, code, redirectURI string) (Tokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ExchangeCalls = append(f.ExchangeCalls, fakeExchangeCall{Code: code, RedirectURI: redirectURI})
	if f.ExchangeErr != nil {
		return Tokens{}, f.ExchangeErr
	}
	return f.ExchangeTokens, nil
}

func (f *FakeClient) RefreshTokens(ctx context.Context, refreshToken string) (Tokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.RefreshCalls = append(f.RefreshCalls, refreshToken)
	if f.RefreshErr != nil {
		return Tokens{}, f.RefreshErr
	}
	return f.RefreshTokens_, nil
}

func (f *FakeClient) CreateBill(ctx context.Context, accessToken, tenantID string, bill Bill) (BillResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.BillCalls = append(f.BillCalls, fakeBillCall{AccessToken: accessToken, TenantID: tenantID, Bill: bill})
	if f.BillErr != nil {
		return BillResult{}, f.BillErr
	}
	return f.BillResult, nil
}

// ListTenants implements tenantLister so handler callback tests can capture
// a tenant ID without the network.
func (f *FakeClient) ListTenants(ctx context.Context, accessToken string) ([]Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.TenantCalls++
	if f.TenantsErr != nil {
		return nil, f.TenantsErr
	}
	return f.Tenants, nil
}

// BillCallCount returns how many bills the fake has seen.
func (f *FakeClient) BillCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.BillCalls)
}
