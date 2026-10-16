package payroll

import (
	"strings"
	"testing"
)

func TestRenderEPFCsv(t *testing.T) {
	rows := []EPFRow{
		{EmployeeID: "E1", EmployeeName: "Ahmad", WagesMinor: 500000, EmployeeMinor: 55000, EmployerMinor: 65000},
		{EmployeeID: "E2", EmployeeName: "Siti, Jr.", WagesMinor: 600000, EmployeeMinor: 66000, EmployerMinor: 72000},
	}
	out := RenderEPFCsv("2026-10", rows)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if !strings.HasPrefix(lines[0], "period,employee_id,") {
		t.Errorf("header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "550.00") || !strings.Contains(lines[1], "650.00") {
		t.Errorf("row 1 amounts wrong: %q", lines[1])
	}
	if !strings.Contains(lines[2], `"Siti, Jr."`) {
		t.Errorf("CSV quoting broken: %q", lines[2])
	}
}

func TestRenderPCBDataTxt(t *testing.T) {
	rows := []PCBRow{{EmployeeID: "E1", EmployeeName: "Ahmad", TaxIDNo: "IG123", PCBMinor: 92188}}
	out := RenderPCBDataTxt("2026-10", rows)
	if strings.TrimSpace(out) != "2026-10|E1|Ahmad|IG123|921.88" {
		t.Errorf("got %q", strings.TrimSpace(out))
	}
}

func TestRenderBorangASummary(t *testing.T) {
	rows := []BorangAEntry{
		{EmployeeID: "E1", EmployeeName: "Ahmad", WagesMinor: 500000, EmployeeMinor: 55000, EmployerMinor: 65000},
		{EmployeeID: "E2", EmployeeName: "Siti", WagesMinor: 600000, EmployeeMinor: 66000, EmployerMinor: 72000},
	}
	out := RenderBorangASummary("2026-10", rows)
	if !strings.Contains(out, "TOTAL") {
		t.Errorf("missing TOTAL line:\n%s", out)
	}
	if !strings.Contains(out, "11000.00") || !strings.Contains(out, "1210.00") || !strings.Contains(out, "1370.00") {
		t.Errorf("totals wrong:\n%s", out)
	}
}
