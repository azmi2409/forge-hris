// Package xero is the forge-hris Xero integration (component 6, Worker A):
// a stdlib HTTP client for the Xero API, OAuth connect/callback handlers,
// account/contact mappings, and per-employee bill posting from payroll
// runs with an auto-post hook on payroll approval.
//
// No live Xero calls exist anywhere in tests: the client is exercised
// against httptest servers serving hand-written JSON fixtures in testdata/.
package xero

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Xero endpoints.
const (
	// IdentityBase is the Xero OAuth2 host.
	IdentityBase = "https://identity.xero.com"
	// APIBase is the Xero accounting API host.
	APIBase = "https://api.xero.com"
	// AuthorizeURL is the OAuth2 authorization endpoint users visit.
	AuthorizeURL = "https://login.xero.com/identity/connect/authorize"
)

// DefaultScopes are the OAuth scopes forge-hris requests: OpenID identity,
// accounting transactions (bills), accounting settings (chart of accounts),
// and offline_access for refresh tokens.
var DefaultScopes = []string{
	"openid", "profile", "email",
	"accounting.transactions", "accounting.settings",
	"offline_access",
}

// Tokens is an OAuth2 token set from Xero's identity service.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// BillContact is the Xero contact a bill is addressed to. ContactID wins
// when set; otherwise Name is sent and Xero creates/looks up the contact.
type BillContact struct {
	ContactID string
	Name      string
}

// BillLineItem is one line on a bill.
type BillLineItem struct {
	Description string
	Quantity    float64
	UnitAmount  float64 // major units (dollars / ringgit / rupiah)
	AccountCode string
	TaxType     string // e.g. "NONE"
}

// Bill is one accounts-payable bill (Type is always "ACCPAY" here).
type Bill struct {
	Type      string // "ACCPAY"
	Contact   BillContact
	Date      string // YYYY-MM-DD
	DueDate   string // YYYY-MM-DD
	Reference string
	LineItems []BillLineItem
}

// BillResult is what Xero returns for a created bill.
type BillResult struct {
	InvoiceID     string
	InvoiceNumber string
	Status        string
}

// Tenant is one Xero organisation the token can access.
type Tenant struct {
	ID         string
	TenantID   string
	TenantName string
	TenantType string
}

// XeroError is a typed error for Xero API failures: the HTTP status plus
// Xero's machine-readable type and the human messages it returned.
type XeroError struct {
	StatusCode int
	Type       string
	Message    string
	Details    []string
}

func (e *XeroError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Type
	}
	if msg == "" {
		msg = "xero request failed"
	}
	return fmt.Sprintf("xero: HTTP %d: %s", e.StatusCode, msg)
}

// XeroClient is the narrow surface forge-hris needs from Xero.
type XeroClient interface {
	// ExchangeCode swaps an OAuth authorization code for tokens.
	ExchangeCode(ctx context.Context, code, redirectURI string) (Tokens, error)
	// RefreshTokens swaps a refresh token for a fresh token set.
	RefreshTokens(ctx context.Context, refreshToken string) (Tokens, error)
	// CreateBill posts one ACCPAY bill and returns the created invoice.
	CreateBill(ctx context.Context, accessToken, tenantID string, bill Bill) (BillResult, error)
}

// tenantLister is an optional extension: clients that can list the Xero
// tenants a token may act on. The OAuth callback uses it to capture the
// tenant ID; clients without it leave the tenant unset.
type tenantLister interface {
	ListTenants(ctx context.Context, accessToken string) ([]Tenant, error)
}

// Client is the real Xero HTTP client (stdlib only).
type Client struct {
	http         *http.Client
	clientID     string
	clientSecret string
	identityBase string
	apiBase      string
}

// NewClient builds a client against the live Xero endpoints.
func NewClient(clientID, clientSecret string) *Client {
	return &Client{
		http:         &http.Client{Timeout: 30 * time.Second},
		clientID:     clientID,
		clientSecret: clientSecret,
		identityBase: IdentityBase,
		apiBase:      APIBase,
	}
}

// newTestClient builds a client against test servers (used by tests with
// fixtures in testdata/).
func newTestClient(clientID, clientSecret, identityBase, apiBase string) *Client {
	return &Client{
		http:         &http.Client{Timeout: 10 * time.Second},
		clientID:     clientID,
		clientSecret: clientSecret,
		identityBase: identityBase,
		apiBase:      apiBase,
	}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// doToken calls the identity token endpoint with form-encoded params and
// HTTP Basic client authentication, per Xero's OAuth2 implementation.
func (c *Client) doToken(ctx context.Context, params url.Values) (Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.identityBase+"/connect/token", strings.NewReader(params.Encode()))
	if err != nil {
		return Tokens{}, fmt.Errorf("xero: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+
		base64.StdEncoding.EncodeToString([]byte(c.clientID+":"+c.clientSecret)))

	resp, err := c.http.Do(req)
	if err != nil {
		return Tokens{}, fmt.Errorf("xero: token request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Tokens{}, fmt.Errorf("xero: read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Tokens{}, parseTokenError(resp.StatusCode, body)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return Tokens{}, fmt.Errorf("xero: decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return Tokens{}, &XeroError{StatusCode: resp.StatusCode, Type: "invalid_token_response", Message: "token response missing access_token"}
	}
	return Tokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

// parseTokenError maps an identity-service error body to a XeroError.
func parseTokenError(status int, body []byte) *XeroError {
	var er struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	xe := &XeroError{StatusCode: status}
	if err := json.Unmarshal(body, &er); err == nil {
		xe.Type = er.Error
		xe.Message = er.ErrorDescription
		if xe.Message == "" {
			xe.Message = er.Error
		}
	}
	return xe
}

// ExchangeCode implements XeroClient.
func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI string) (Tokens, error) {
	params := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
	}
	toks, err := c.doToken(ctx, params)
	if err != nil {
		return Tokens{}, fmt.Errorf("xero: exchange code: %w", err)
	}
	return toks, nil
}

// RefreshTokens implements XeroClient.
func (c *Client) RefreshTokens(ctx context.Context, refreshToken string) (Tokens, error) {
	params := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	toks, err := c.doToken(ctx, params)
	if err != nil {
		return Tokens{}, fmt.Errorf("xero: refresh tokens: %w", err)
	}
	return toks, nil
}

// ListTenants returns the Xero organisations the access token may act on.
func (c *Client) ListTenants(ctx context.Context, accessToken string) ([]Tenant, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/connections", nil)
	if err != nil {
		return nil, fmt.Errorf("xero: build connections request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xero: connections request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("xero: read connections response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseXeroError(resp.StatusCode, body)
	}
	var tenants []Tenant
	if err := json.Unmarshal(body, &tenants); err != nil {
		return nil, fmt.Errorf("xero: decode connections response: %w", err)
	}
	return tenants, nil
}

type invoiceRequest struct {
	Invoices []invoicePayload `json:"Invoices"`
}

type invoicePayload struct {
	Type      string            `json:"Type"`
	Contact   contactPayload    `json:"Contact"`
	Date      string            `json:"Date,omitempty"`
	DueDate   string            `json:"DueDate,omitempty"`
	Reference string            `json:"Reference,omitempty"`
	Status    string            `json:"Status,omitempty"`
	LineItems []lineItemPayload `json:"LineItems"`
}

type contactPayload struct {
	ContactID string `json:"ContactID,omitempty"`
	Name      string `json:"Name,omitempty"`
}

type lineItemPayload struct {
	Description string  `json:"Description"`
	Quantity    float64 `json:"Quantity"`
	UnitAmount  float64 `json:"UnitAmount"`
	AccountCode string  `json:"AccountCode,omitempty"`
	TaxType     string  `json:"TaxType,omitempty"`
}

type invoiceResponse struct {
	Invoices []struct {
		InvoiceID     string `json:"InvoiceID"`
		InvoiceNumber string `json:"InvoiceNumber"`
		Status        string `json:"Status"`
	} `json:"Invoices"`
}

// CreateBill implements XeroClient: POSTs one ACCPAY invoice.
func (c *Client) CreateBill(ctx context.Context, accessToken, tenantID string, bill Bill) (BillResult, error) {
	if bill.Type == "" {
		bill.Type = "ACCPAY"
	}
	payload := invoiceRequest{Invoices: []invoicePayload{{
		Type:      bill.Type,
		Contact:   contactPayload{ContactID: bill.Contact.ContactID, Name: bill.Contact.Name},
		Date:      bill.Date,
		DueDate:   bill.DueDate,
		Reference: bill.Reference,
		LineItems: make([]lineItemPayload, 0, len(bill.LineItems)),
	}}}
	for _, li := range bill.LineItems {
		payload.Invoices[0].LineItems = append(payload.Invoices[0].LineItems, lineItemPayload{
			Description: li.Description,
			Quantity:    li.Quantity,
			UnitAmount:  li.UnitAmount,
			AccountCode: li.AccountCode,
			TaxType:     li.TaxType,
		})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return BillResult{}, fmt.Errorf("xero: marshal bill: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiBase+"/api.xro/2.0/Invoices", bytes.NewReader(body))
	if err != nil {
		return BillResult{}, fmt.Errorf("xero: build invoice request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Xero-Tenant-Id", tenantID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return BillResult{}, fmt.Errorf("xero: invoice request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return BillResult{}, fmt.Errorf("xero: read invoice response: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return BillResult{}, parseXeroError(resp.StatusCode, respBody)
	}
	var ir invoiceResponse
	if err := json.Unmarshal(respBody, &ir); err != nil {
		return BillResult{}, fmt.Errorf("xero: decode invoice response: %w", err)
	}
	if len(ir.Invoices) == 0 || ir.Invoices[0].InvoiceID == "" {
		return BillResult{}, &XeroError{StatusCode: resp.StatusCode, Type: "invalid_invoice_response", Message: "invoice response missing InvoiceID"}
	}
	return BillResult{
		InvoiceID:     ir.Invoices[0].InvoiceID,
		InvoiceNumber: ir.Invoices[0].InvoiceNumber,
		Status:        ir.Invoices[0].Status,
	}, nil
}

// parseXeroError maps an accounting-API error body to a XeroError, folding
// Xero's per-element validation errors into Details.
func parseXeroError(status int, body []byte) *XeroError {
	xe := &XeroError{StatusCode: status}
	var er struct {
		ErrorNumber int    `json:"ErrorNumber"`
		Type        string `json:"Type"`
		Message     string `json:"Message"`
		Elements    []struct {
			ValidationErrors []struct {
				Message string `json:"Message"`
			} `json:"ValidationErrors"`
		} `json:"Elements"`
	}
	if err := json.Unmarshal(body, &er); err != nil {
		xe.Message = strings.TrimSpace(string(body))
		return xe
	}
	xe.Type = er.Type
	xe.Message = er.Message
	for _, el := range er.Elements {
		for _, ve := range el.ValidationErrors {
			if ve.Message != "" {
				xe.Details = append(xe.Details, ve.Message)
			}
		}
	}
	return xe
}
