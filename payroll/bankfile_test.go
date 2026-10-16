package payroll

import (
	"strings"
	"testing"
)

func TestRenderPipeDelimited(t *testing.T) {
	credits := []BankCredit{
		{AccountNo: "123", AccountName: "Ahmad", AmountMinor: 450000, Reference: "Gaji Jan"},
		{AccountNo: "456", AccountName: "Siti", AmountMinor: 520050, Reference: "Gaji Jan"},
	}
	out := RenderPipeDelimited("ORG001", "20261031", credits)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if lines[0] != "ORG001|20261031|1|123|Ahmad|450000|Gaji Jan" {
		t.Errorf("line 1 = %q", lines[0])
	}
	if lines[1] != "ORG001|20261031|2|456|Siti|520050|Gaji Jan" {
		t.Errorf("line 2 = %q", lines[1])
	}
}

func TestRenderPipeDelimitedSanitizes(t *testing.T) {
	credits := []BankCredit{{AccountNo: "1|2", AccountName: "A\nB", AmountMinor: 100, Reference: "x"}}
	out := RenderPipeDelimited("O", "20261031", credits)
	if strings.Count(out, "|") != 6 {
		t.Errorf("delimiters broken by unsanitized input: %q", out)
	}
}

func TestRenderMaybankTemplate(t *testing.T) {
	credits := []BankCredit{
		{AccountNo: "123", AccountName: "Ahmad", AmountMinor: 450000, Reference: "PAY"},
	}
	out := RenderMaybank("ORG001", "20261031", credits)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want header+detail+trailer", len(lines))
	}
	if !strings.HasPrefix(lines[0], "H|ORG001|20261031|1|450000") {
		t.Errorf("header = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "D|1|123|Ahmad|450000|PAY") {
		t.Errorf("detail = %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "T|1|") {
		t.Errorf("trailer = %q", lines[2])
	}
}

func TestRenderEmptyCredits(t *testing.T) {
	if RenderPipeDelimited("O", "20261031", nil) != "" {
		t.Error("empty credits should render empty")
	}
}
