package payroll

import (
	"encoding/csv"
	"fmt"
	"strings"
)

// Statutory filing exports (PayrollPanda parity: EPF i-Akaun CSV, e-Data PCB
// txt, Borang A). Layouts below are functional placeholders — every one is
// marked VERIFY against the authority's current upload spec before filing.

type EPFRow struct {
	EmployeeID    string
	EmployeeName  string
	WagesMinor    int64 // contribution wages, sen
	EmployeeMinor int64 // employee share, sen
	EmployerMinor int64 // employer share, sen
}

type PCBRow struct {
	EmployeeID   string
	EmployeeName string
	TaxIDNo      string // TIN / MyKad
	PCBMinor     int64  // PCB for the month, sen
}

type BorangAEntry struct {
	EmployeeID    string
	EmployeeName  string
	WagesMinor    int64
	EmployeeMinor int64
	EmployerMinor int64
}

// fmtRM formats sen as "1234.56".
func fmtRM(sen int64) string {
	neg := sen < 0
	if neg {
		sen = -sen
	}
	s := fmt.Sprintf("%d.%02d", sen/100, sen%100)
	if neg {
		s = "-" + s
	}
	return s
}

// RenderEPFCsv renders an EPF contribution file, one row per employee.
// VERIFY: column order, header requirements, and amount format against the
// current KWSP i-Akaun (Employer) CSV upload spec before submission.
func RenderEPFCsv(period string, rows []EPFRow) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"period", "employee_id", "employee_name", "wages_rm", "employee_rm", "employer_rm"})
	for _, r := range rows {
		_ = w.Write([]string{
			period, r.EmployeeID, r.EmployeeName,
			fmtRM(r.WagesMinor), fmtRM(r.EmployeeMinor), fmtRM(r.EmployerMinor),
		})
	}
	w.Flush()
	return b.String()
}

// RenderPCBDataTxt renders the monthly PCB data file.
// VERIFY: record layout, field widths, and amount format against LHDN's
// current e-Data PCB / e-PCB Plus specification before submission.
func RenderPCBDataTxt(period string, rows []PCBRow) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%s|%s|%s|%s|%s\n",
			sanitizeField(period),
			sanitizeField(r.EmployeeID),
			sanitizeField(r.EmployeeName),
			sanitizeField(r.TaxIDNo),
			fmtRM(r.PCBMinor),
		)
	}
	return b.String()
}

// RenderBorangASummary renders a plain-text Borang A (EPF Form A) summary for
// the period: totals plus a per-employee breakdown.
func RenderBorangASummary(period string, rows []BorangAEntry) string {
	var b strings.Builder
	var tw, tee, ter int64
	fmt.Fprintf(&b, "BORANG A SUMMARY — period %s\n", sanitizeField(period))
	fmt.Fprintf(&b, "%-12s %-30s %12s %12s %12s\n", "employee_id", "name", "wages_rm", "employee_rm", "employer_rm")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-12s %-30s %12s %12s %12s\n",
			sanitizeField(r.EmployeeID), sanitizeField(r.EmployeeName),
			fmtRM(r.WagesMinor), fmtRM(r.EmployeeMinor), fmtRM(r.EmployerMinor))
		tw += r.WagesMinor
		tee += r.EmployeeMinor
		ter += r.EmployerMinor
	}
	fmt.Fprintf(&b, "%-12s %-30s %12s %12s %12s\n", "TOTAL", "", fmtRM(tw), fmtRM(tee), fmtRM(ter))
	return b.String()
}
