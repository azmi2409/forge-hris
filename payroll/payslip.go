package payroll

import (
	"fmt"
	"html/template"
	"strings"
)

// PayslipData is the input to RenderPayslipHTML.
type PayslipData struct {
	CompanyName    string
	EmployeeName   string
	EmployeeNumber string
	Period         string
	Currency       string // "MYR" (default) or "IDR"; additive to the shared contract

	Items []PayLineItem

	GrossMinor      int64
	DeductionsMinor int64
	NetMinor        int64

	Splits StatutorySplits
}

// formatMinor formats minor units for display: "RM 1,234.56" for MYR,
// "Rp 1.234.567" for IDR.
func formatMinor(minor int64, currency string) string {
	neg := minor < 0
	if neg {
		minor = -minor
	}
	group := func(n int64, sep string) string {
		s := fmt.Sprintf("%d", n)
		var out []byte
		for i, c := range s {
			if i > 0 && (len(s)-i)%3 == 0 {
				out = append(out, sep...)
			}
			out = append(out, byte(c))
		}
		return string(out)
	}
	var s string
	if strings.ToUpper(currency) == "IDR" {
		s = "Rp " + group(minor, ".")
	} else {
		s = "RM " + group(minor/100, ",") + fmt.Sprintf(".%02d", minor%100)
	}
	if neg {
		s = "-" + s
	}
	return s
}

var payslipTemplate = template.Must(template.New("payslip").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Payslip — {{.EmployeeName}} — {{.Period}}</title>
<style>
  body { font-family: Arial, Helvetica, sans-serif; color: #1a1a1a; margin: 0; padding: 24px; }
  .sheet { max-width: 720px; margin: 0 auto; border: 1px solid #ccc; }
  .head { padding: 20px 24px; border-bottom: 2px solid #1a1a1a; }
  .head h1 { margin: 0 0 4px; font-size: 20px; }
  .head p { margin: 2px 0; font-size: 13px; color: #444; }
  table { width: 100%; border-collapse: collapse; font-size: 13px; }
  th, td { padding: 8px 24px; text-align: left; }
  th { background: #f4f4f4; font-size: 11px; text-transform: uppercase; letter-spacing: .04em; }
  td.num, th.num { text-align: right; font-variant-numeric: tabular-nums; }
  tr.total td { font-weight: bold; border-top: 2px solid #1a1a1a; }
  tr.net td { font-weight: bold; font-size: 15px; background: #f4f4f4; }
  .sec { padding: 12px 24px 4px; font-size: 12px; font-weight: bold; text-transform: uppercase; letter-spacing: .04em; color: #555; }
  .foot { padding: 16px 24px; font-size: 11px; color: #777; border-top: 1px solid #ccc; }
  @media print { body { padding: 0; } .sheet { border: none; max-width: none; } }
</style>
</head>
<body>
<div class="sheet">
  <div class="head">
    <h1>{{.CompanyName}}</h1>
    <p>Payslip for the period {{.Period}}</p>
    <p>{{.EmployeeName}} ({{.EmployeeNumber}})</p>
  </div>

  <div class="sec">Earnings</div>
  <table>
    <tr><th>Description</th><th class="num">Amount</th></tr>
    {{range .Earnings}}<tr><td>{{.Name}}{{if .Note}} <span style="color:#777">({{.Note}})</span>{{end}}</td><td class="num">{{.Amount}}</td></tr>{{end}}
    <tr class="total"><td>Gross pay</td><td class="num">{{.Gross}}</td></tr>
  </table>

  <div class="sec">Deductions</div>
  <table>
    <tr><th>Description</th><th class="num">Amount</th></tr>
    {{range .Deductions}}<tr><td>{{.Name}}{{if .Note}} <span style="color:#777">({{.Note}})</span>{{end}}</td><td class="num">{{.Amount}}</td></tr>{{end}}
    {{range .Statutory}}<tr><td>{{.Name}}</td><td class="num">{{.Amount}}</td></tr>{{end}}
    <tr class="total"><td>Total deductions</td><td class="num">{{.TotalDeductions}}</td></tr>
  </table>

  <table>
    <tr class="net"><td>Net pay</td><td class="num">{{.Net}}</td></tr>
  </table>

  <div class="sec">Employer contributions (for information)</div>
  <table>
    <tr><th>Description</th><th class="num">Amount</th></tr>
    {{range .EmployerStat}}<tr><td>{{.Name}}</td><td class="num">{{.Amount}}</td></tr>{{end}}
  </table>

  <div class="foot">This is a computer-generated payslip. Statutory amounts are computed
  by the forge-hris payroll engine; verify against the authorities before filing.</div>
</div>
</body>
</html>`))

type payslipLine struct {
	Name   string
	Note   string
	Amount string
}

type payslipView struct {
	CompanyName     string
	EmployeeName    string
	EmployeeNumber  string
	Period          string
	Earnings        []payslipLine
	Deductions      []payslipLine
	Statutory       []payslipLine
	EmployerStat    []payslipLine
	Gross           string
	TotalDeductions string
	Net             string
}

// RenderPayslipHTML renders a clean, printable, no-JS payslip with inline CSS.
// All dynamic content goes through html/template escaping.
func RenderPayslipHTML(p PayslipData) string {
	cur := p.Currency
	if cur == "" {
		cur = "MYR"
	}
	f := func(m int64) string { return formatMinor(m, cur) }

	v := payslipView{
		CompanyName: p.CompanyName, EmployeeName: p.EmployeeName,
		EmployeeNumber: p.EmployeeNumber, Period: p.Period,
		Gross: f(p.GrossMinor), Net: f(p.NetMinor),
	}
	for _, it := range p.Items {
		l := payslipLine{Name: it.Name, Note: it.Note, Amount: f(it.AmountMinor)}
		if it.Kind == PresetAddition {
			v.Earnings = append(v.Earnings, l)
		} else if it.Kind == PresetDeduction {
			v.Deductions = append(v.Deductions, l)
		}
	}
	s := p.Splits
	addStat := func(name string, m int64) {
		if m != 0 {
			v.Statutory = append(v.Statutory, payslipLine{Name: name, Amount: f(m)})
		}
	}
	addEr := func(name string, m int64) {
		if m != 0 {
			v.EmployerStat = append(v.EmployerStat, payslipLine{Name: name, Amount: f(m)})
		}
	}
	if cur == "IDR" {
		addStat("BPJS Kesehatan (employee)", s.BPJSKesEmployee)
		addStat("JHT (employee)", s.JHTEmployee)
		addStat("JP (employee)", s.JPEmployee)
		addStat("PPh 21", s.PPh21)
		addEr("BPJS Kesehatan (employer)", s.BPJSKesEmployer)
		addEr("JHT (employer)", s.JHTEmployer)
		addEr("JKK (employer)", s.JKKEmployer)
		addEr("JKM (employer)", s.JKMEmployer)
		addEr("JP (employer)", s.JPEmployer)
	} else {
		addStat("EPF (employee)", s.EPFEmployee)
		addStat("SOCSO (employee, incl. SKBBK where applicable)", s.SOCSOEmployee)
		addStat("EIS (employee)", s.EISEmployee)
		addStat("PCB", s.PCB)
		addEr("EPF (employer)", s.EPFEmployer)
		addEr("SOCSO (employer)", s.SOCSOEmployer)
		addEr("EIS (employer)", s.EISEmployer)
		addEr("HRDF (employer)", s.HRDFEmployer)
	}
	v.TotalDeductions = f(p.DeductionsMinor + s.EmployeeTotal())

	var b strings.Builder
	_ = payslipTemplate.Execute(&b, v)
	return b.String()
}
