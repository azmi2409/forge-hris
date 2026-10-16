package payroll

// SeedPresets returns the global PayrollPanda-style payroll item catalog.
//
// Flag semantics (see docs/rea-hris.md §3.1 — the design being cloned):
//   - "Subject to X" on an addition feeds that statutory wage base.
//   - ReducesContributionWages on a deduction shrinks every statutory base.
//   - A plain deduction (no ReducesContributionWages) only reduces net pay.
//
// Special codes with engine behaviour beyond the flags:
//   - "zakat_payroll": deducted from net AND rebates PCB (MY).
//   - "zakat_direct":  NOT deducted from net; recorded for reporting and
//     still rebates PCB (MY). The employee paid it directly, outside payroll.
//   - "epf_topup_employee": voluntary employee EPF top-up; deducted from net
//     and added to Splits.EPFEmployee.
//   - "unpaid_leave_deduction": appended automatically by Calculate when
//     PayInput.UnpaidLeaveDays > 0.
func SeedPresets() []Preset {
	allMY := Preset{SubjectToEPF: true, SubjectToSOCSO: true, SubjectToEIS: true, SubjectToPCB: true}
	allID := Preset{SubjectToBPJSKes: true, SubjectToJHT: true, SubjectToJKK: true, SubjectToJKM: true, SubjectToJP: true, SubjectToPPh21: true}

	with := func(base Preset, code, name string, kind PresetKind) Preset {
		p := base
		p.Code, p.Name, p.Kind = code, name, kind
		return p
	}
	merge := func(a, b Preset) Preset {
		a.SubjectToEPF = a.SubjectToEPF || b.SubjectToEPF
		a.SubjectToSOCSO = a.SubjectToSOCSO || b.SubjectToSOCSO
		a.SubjectToEIS = a.SubjectToEIS || b.SubjectToEIS
		a.SubjectToPCB = a.SubjectToPCB || b.SubjectToPCB
		a.SubjectToBPJSKes = a.SubjectToBPJSKes || b.SubjectToBPJSKes
		a.SubjectToJHT = a.SubjectToJHT || b.SubjectToJHT
		a.SubjectToJKK = a.SubjectToJKK || b.SubjectToJKK
		a.SubjectToJKM = a.SubjectToJKM || b.SubjectToJKM
		a.SubjectToJP = a.SubjectToJP || b.SubjectToJP
		a.SubjectToPPh21 = a.SubjectToPPh21 || b.SubjectToPPh21
		return a
	}
	full := merge(allMY, allID)

	return []Preset{
		with(full, "basic_salary", "Basic Salary", PresetAddition),

		with(full, "overtime", "Overtime", PresetAddition),

		// Fixed allowances feed EPF/SOCSO/EIS and all ID bases, but are NOT
		// PCB-taxable by default (configurable per org — flip SubjectToPCB).
		with(merge(Preset{SubjectToEPF: true, SubjectToSOCSO: true, SubjectToEIS: true}, allID),
			"allowance_fixed", "Fixed Allowance", PresetAddition),

		with(full, "allowance_transport", "Transport Allowance", PresetAddition),

		with(full, "bonus", "Bonus", PresetAddition),

		with(full, "commission", "Commission", PresetAddition),

		// Advance paid out via payroll: a plain deduction — reduces net only.
		// (PayrollPanda: "Advance Payment" subject to all deductions.)
		with(full, "advance_payment", "Advance Payment", PresetDeduction),

		// Advance repaid through payroll: shrinks every contribution-wage base.
		// (PayrollPanda: "Advance (Payment Made via Payroll)".)
		func() Preset {
			p := with(full, "advance_repayment_via_payroll", "Advance Repayment via Payroll", PresetDeduction)
			p.ReducesContributionWages = true
			return p
		}(),

		// Auto-appended by the engine for unpaid leave days.
		func() Preset {
			p := with(Preset{}, "unpaid_leave_deduction", "Unpaid Leave Deduction", PresetDeduction)
			p.ReducesContributionWages = true
			return p
		}(),

		with(Preset{}, "loan_repayment", "Loan Repayment", PresetDeduction),

		// Zakat deducted from payroll: reduces net, and rebates PCB (MY).
		// (PayrollPanda: "Zakat (Deducted From Payroll)".)
		with(Preset{}, "zakat_payroll", "Zakat (Deducted From Payroll)", PresetDeduction),

		// Zakat paid directly by the employee: NEVER reduces net — it is only
		// recorded here so the PCB rebate and statutory reporting pick it up.
		// (PayrollPanda: "Zakat (Paid Direct By Employee)".)
		with(Preset{}, "zakat_direct", "Zakat (Paid Direct By Employee)", PresetDeduction),

		// Voluntary employee EPF top-up: deducted from net, added on top of
		// the statutory EPF employee share in the result splits.
		with(Preset{}, "epf_topup_employee", "EPF Top-Up (Employee)", PresetDeduction),
	}
}
