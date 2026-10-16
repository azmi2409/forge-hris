package payroll

import (
	"strings"
	"testing"
)

func TestFormatMinor(t *testing.T) {
	if got := formatMinor(123456789, "MYR"); got != "RM 1,234,567.89" {
		t.Errorf("MYR = %q", got)
	}
	if got := formatMinor(123456789, "IDR"); got != "Rp 123.456.789" {
		t.Errorf("IDR = %q", got)
	}
	if got := formatMinor(-500, "MYR"); got != "-RM 5.00" {
		t.Errorf("negative = %q", got)
	}
	if got := formatMinor(0, ""); got != "RM 0.00" {
		t.Errorf("default currency = %q", got)
	}
}

func TestRenderPayslipHTML(t *testing.T) {
	p := PayslipData{
		CompanyName: "Acme Sdn Bhd", EmployeeName: "Ahmad", EmployeeNumber: "E1",
		Period: "October 2026", Currency: "MYR",
		Items: []PayLineItem{
			{PresetCode: "basic_salary", Name: "Basic Salary", Kind: PresetAddition, AmountMinor: 500000},
			{PresetCode: "loan_repayment", Name: "Loan Repayment", Kind: PresetDeduction, AmountMinor: 20000},
		},
		GrossMinor: 500000, DeductionsMinor: 20000, NetMinor: 400000,
		Splits: StatutorySplits{EPFEmployee: 55000, PCB: 5000, EPFEmployer: 65000},
	}
	out := RenderPayslipHTML(p)
	for _, want := range []string{
		"Acme Sdn Bhd", "Ahmad", "October 2026",
		"RM 5,000.00", "RM 200.00", "RM 4,000.00",
		"EPF (employee)", "RM 550.00",
		"EPF (employer)", "RM 650.00",
		"@media print",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("payslip missing %q", want)
		}
	}
}

func TestRenderPayslipHTMLEscaped(t *testing.T) {
	p := PayslipData{
		CompanyName:  "<script>alert(1)</script>",
		EmployeeName: "A & B", EmployeeNumber: "E1", Period: "Oct 2026",
	}
	out := RenderPayslipHTML(p)
	if strings.Contains(out, "<script>") {
		t.Error("company name not escaped")
	}
	if !strings.Contains(out, "A &amp; B") {
		t.Error("employee name not escaped")
	}
}

func TestRenderPayslipIDR(t *testing.T) {
	p := PayslipData{
		CompanyName: "PT Maju", EmployeeName: "Budi", EmployeeNumber: "ID1",
		Period: "Oktober 2026", Currency: "IDR",
		Items: []PayLineItem{
			{PresetCode: "basic_salary", Name: "Gaji Pokok", Kind: PresetAddition, AmountMinor: 10000000},
		},
		GrossMinor: 10000000, NetMinor: 9000000,
		Splits: StatutorySplits{PPh21: 200000, JHTEmployee: 200000},
	}
	out := RenderPayslipHTML(p)
	if !strings.Contains(out, "Rp 10.000.000") {
		t.Errorf("IDR gross not formatted: %q", out[:2000])
	}
	if !strings.Contains(out, "PPh 21") {
		t.Error("missing PPh 21 line")
	}
}
