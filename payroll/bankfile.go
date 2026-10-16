package payroll

import (
	"fmt"
	"strconv"
	"strings"
)

// Bank file generation (PayrollPanda parity: bank "integration" is file
// generation for manual bank-portal upload, not bank API debits).

// BankCredit is one credit line in a bank file.
type BankCredit struct {
	AccountNo   string
	AccountName string
	AmountMinor int64 // minor units (sen / rupiah)
	Reference   string
}

// sanitizeField strips characters that would break delimited formats.
func sanitizeField(s string) string {
	s = strings.ReplaceAll(s, "|", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

// RenderPipeDelimited renders a generic pipe-delimited bulk credit file:
//
//	orgCode|creditingDate|seq|accountNo|accountName|amountMinor|reference
//
// creditingDate is YYYYMMDD. seq starts at 1. amountMinor is the integer
// minor-unit amount (sen for MYR, rupiah for IDR).
func RenderPipeDelimited(orgCode, creditingDate string, credits []BankCredit) string {
	var b strings.Builder
	for i, c := range credits {
		fmt.Fprintf(&b, "%s|%s|%d|%s|%s|%d|%s\n",
			sanitizeField(orgCode),
			sanitizeField(creditingDate),
			i+1,
			sanitizeField(c.AccountNo),
			sanitizeField(c.AccountName),
			c.AmountMinor,
			sanitizeField(c.Reference),
		)
	}
	return b.String()
}

// RenderMaybank renders a Maybank-style bulk salary-credit file.
//
// ⚠ TEMPLATE — NOT the actual Maybank specification. Field order, header
// semantics, amount formatting (sen vs RM), and trailer requirements must be
// verified against Maybank's current bulk-credit / salary-crediting file spec
// (and the organisation code issued by the bank) before use. Never submit a
// file generated from this template to a bank without that verification.
func RenderMaybank(orgCode, creditingDate string, credits []BankCredit) string {
	var b strings.Builder
	var total int64
	for _, c := range credits {
		total += c.AmountMinor
	}
	// Header: record type, organisation code, crediting date, batch count, total.
	fmt.Fprintf(&b, "H|%s|%s|%d|%d\n",
		sanitizeField(orgCode), sanitizeField(creditingDate), len(credits), total)
	for i, c := range credits {
		fmt.Fprintf(&b, "D|%d|%s|%s|%d|%s\n",
			i+1,
			sanitizeField(c.AccountNo),
			sanitizeField(c.AccountName),
			c.AmountMinor,
			sanitizeField(c.Reference),
		)
	}
	// Trailer: record type + hash total (sum of account numbers is NOT a real
	// Maybank control — placeholder only, see the TEMPLATE warning above).
	var hash int64
	for _, c := range credits {
		if n, err := strconv.ParseInt(strings.TrimSpace(c.AccountNo), 10, 64); err == nil {
			hash += n
		}
	}
	fmt.Fprintf(&b, "T|%d|%d\n", len(credits), hash)
	return b.String()
}
