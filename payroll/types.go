// Package payroll is the pure payroll calculation engine for forge-hris
// (wave 3 / component 5).
//
// It is stdlib-only and deliberately does NOT import the store package:
// the store layer imports payroll, never the reverse.
//
// All money is int64 in MINOR units: sen for MYR, whole rupiah for IDR
// (rupiah has no minor unit, so 1 unit = Rp 1).
//
// Design notes carried over from the PayrollPanda REA (docs/rea-hris.md §3):
//   - Statutory behaviour is derived from per-item flags + worker status,
//     never hard-coded per payroll run.
//   - Payroll runs are Draft → Approved → Locked; the engine only computes.
//     The two-phase preview → submit gate lives in preview.go and the store
//     layer (REA §5.6).
package payroll

// PresetKind classifies a payroll item preset.
type PresetKind string

const (
	// PresetAddition increases gross pay (salary, overtime, allowances, bonus...).
	PresetAddition PresetKind = "addition"
	// PresetDeduction decreases net pay (advances, loans, zakat via payroll...).
	PresetDeduction PresetKind = "deduction"
	// PresetEmployerContribution is an employer-side statutory cost that never
	// touches the employee's net (kept for catalog completeness).
	PresetEmployerContribution PresetKind = "employer_contribution"
)

// Preset is one entry of the payroll item catalog (PayrollPanda-style).
// The SubjectTo* flags decide which statutory wage bases an addition feeds;
// ReducesContributionWages marks deductions (e.g. unpaid leave, advance
// repaid via payroll) that shrink every contribution-wage base.
type Preset struct {
	Code string     `json:"code"`
	Name string     `json:"name"`
	Kind PresetKind `json:"kind"`

	// Malaysia bases.
	SubjectToEPF   bool `json:"subject_to_epf"`
	SubjectToSOCSO bool `json:"subject_to_socso"`
	SubjectToEIS   bool `json:"subject_to_eis"`
	SubjectToPCB   bool `json:"subject_to_pcb"`

	// Indonesia bases.
	SubjectToBPJSKes bool `json:"subject_to_bpjs_kes"`
	SubjectToJHT     bool `json:"subject_to_jht"`
	SubjectToJKK     bool `json:"subject_to_jkk"`
	SubjectToJKM     bool `json:"subject_to_jkm"`
	SubjectToJP      bool `json:"subject_to_jp"`
	SubjectToPPh21   bool `json:"subject_to_pph21"`

	// ReducesContributionWages, when true on a deduction, subtracts the
	// deduction amount from every statutory contribution-wage base.
	ReducesContributionWages bool `json:"reduces_contribution_wages"`
}

// StatutorySplits holds the computed statutory amounts for one employee.
// Shared contract with the store layer: fields are only ever appended to,
// never removed or retyped.
//
// Note: SOCSOEmployee includes the SKBBK (Lindung 24 Jam) employee share when
// it applies — SKBBK is administered by PERKESO and deducted with SOCSO.
// See my.go.
type StatutorySplits struct {
	// Malaysia.
	EPFEmployee   int64 `json:"epf_employee"`
	EPFEmployer   int64 `json:"epf_employer"`
	SOCSOEmployee int64 `json:"socso_employee"` // includes SKBBK employee share when applicable
	SOCSOEmployer int64 `json:"socso_employer"`
	EISEmployee   int64 `json:"eis_employee"`
	EISEmployer   int64 `json:"eis_employer"`
	PCB           int64 `json:"pcb"`
	HRDFEmployer  int64 `json:"hrdf_employer"`

	// Indonesia.
	BPJSKesEmployer int64 `json:"bpjs_kes_employer"`
	BPJSKesEmployee int64 `json:"bpjs_kes_employee"`
	JHTEmployer     int64 `json:"jht_employer"`
	JHTEmployee     int64 `json:"jht_employee"`
	JKKEmployer     int64 `json:"jkk_employer"`
	JKMEmployer     int64 `json:"jkm_employer"`
	JPEmployer      int64 `json:"jp_employer"`
	JPEmployee      int64 `json:"jp_employee"`
	PPh21           int64 `json:"pph21"`
}

// EmployeeTotal returns the employee-side statutory deductions,
// including PCB / PPh21 income tax.
func (s StatutorySplits) EmployeeTotal() int64 {
	return s.EPFEmployee + s.SOCSOEmployee + s.EISEmployee + s.PCB +
		s.BPJSKesEmployee + s.JHTEmployee + s.JPEmployee + s.PPh21
}

// EmployerTotal returns the employer-side statutory cost.
func (s StatutorySplits) EmployerTotal() int64 {
	return s.EPFEmployer + s.SOCSOEmployer + s.EISEmployer + s.HRDFEmployer +
		s.BPJSKesEmployer + s.JHTEmployer + s.JKKEmployer + s.JKMEmployer + s.JPEmployer
}

// PayLineItem is one computed payroll line (earning or deduction).
type PayLineItem struct {
	PresetCode  string     `json:"preset_code"`
	Name        string     `json:"name"`
	Kind        PresetKind `json:"kind"`
	AmountMinor int64      `json:"amount_minor"`
	Note        string     `json:"note,omitempty"`
}

// PayItemInput is one caller-supplied payroll item for an employee.
type PayItemInput struct {
	PresetCode  string `json:"preset_code"`
	AmountMinor int64  `json:"amount_minor"`
}

// PayInput is the per-employee input to Calculate.
type PayInput struct {
	EmployeeID  string `json:"employee_id"`
	CountryCode string `json:"country_code,omitempty"` // "MY" | "ID"; empty inherits the Calculate country param

	BaseSalaryMinor int64          `json:"base_salary_minor"`
	Items           []PayItemInput `json:"items,omitempty"`

	UnpaidLeaveDays float64 `json:"unpaid_leave_days,omitempty"`
	DailyRateMinor  int64   `json:"daily_rate_minor,omitempty"`

	AgeYears int `json:"age_years"`

	// Malaysia worker status.
	CitizenMY        bool `json:"citizen_my"`        // Malaysian citizen (KWSP Section A/E)
	PRMY             bool `json:"pr_my"`             // Malaysian permanent resident (KWSP Section A/C). Additive to the shared contract; zero value = not a PR.
	ForeignWorkerMY  bool `json:"foreign_worker_my"` // non-Malaysian employee (new 2%/2% EPF regime from 2025-10-01)
	EmployerBornePCB bool `json:"employer_borne_pcb"`

	// Set SKBBKOptIn for Malaysian employees who opted in to Lindung 24 Jam
	// (voluntary for locals since 2026-07-08). Foreign workers are always
	// covered (mandatory) regardless of this flag. Additive to the shared
	// contract; zero value = opted out.
	SKBBKOptIn bool `json:"skbbk_opt_in"`

	// Indonesia.
	JKKRiskClass int    `json:"jkk_risk_class"` // 1..5; out of range is an error
	PTKPStatus   string `json:"ptkp_status"`    // e.g. "TK/0"; unknown defaults to TER category A with a warning

	BankAccount string `json:"bank_account,omitempty"`
	BankCode    string `json:"bank_code,omitempty"`
}

// PayResult is the computed payroll for one employee.
type PayResult struct {
	EmployeeID string `json:"employee_id"`

	GrossMinor      int64 `json:"gross_minor"`      // base salary + all additions
	AdditionsMinor  int64 `json:"additions_minor"`  // additions excluding base salary
	DeductionsMinor int64 `json:"deductions_minor"` // non-statutory deductions (excludes zakat_direct, which never touches net)

	Items []PayLineItem `json:"items"`

	Splits StatutorySplits `json:"splits"`

	NetMinor int64 `json:"net_minor"`

	Warnings []string `json:"warnings,omitempty"`
}

// Warning strings emitted by Calculate. The preview step maps some of these
// to anomaly codes.
const (
	WarnMissingBankDetails = "missing bank account details"
	WarnUnknownPTKPStatus  = "unknown PTKP status; defaulted to TER category A"
	WarnZeroDailyRate      = "unpaid leave days given but daily rate is zero"
)
