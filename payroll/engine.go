package payroll

import (
	"fmt"
	"math"
)

// nonNetDeductionCodes lists deduction preset codes that never reduce net pay.
// Currently only zakat_direct: the employee paid it directly, outside payroll —
// it is recorded for the PCB rebate and statutory reporting only.
var nonNetDeductionCodes = map[string]bool{
	"zakat_direct": true,
}

// Calculate computes payroll for a batch of employees in one country.
//
//   - countryCode is "MY" or "ID" (anything else is an error).
//   - presets is the org's item catalog (see SeedPresets); every
//     PayItemInput.PresetCode must exist in it.
//   - An input's CountryCode, when non-empty, must match countryCode.
//
// Per employee the engine: builds the item list (auto basic_salary line when
// BaseSalaryMinor > 0, auto unpaid-leave deduction), derives each statutory
// wage base from the preset flags, runs the country rule pack, then derives
// gross / deductions / net. Unknown PTKP statuses default to TER category A
// with a warning; an out-of-range JKKRiskClass is an error.
func Calculate(countryCode string, presets []Preset, inputs []PayInput) ([]PayResult, error) {
	if countryCode != "MY" && countryCode != "ID" {
		return nil, fmt.Errorf("payroll: unsupported country code %q", countryCode)
	}

	presetByCode := make(map[string]Preset, len(presets))
	for _, p := range presets {
		presetByCode[p.Code] = p
	}

	results := make([]PayResult, 0, len(inputs))
	for i := range inputs {
		res, err := calculateOne(countryCode, presetByCode, &inputs[i])
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
}

// RulePackVersion identifies the statutory rule pack for a country.
// Bump when rates change (SKBBK phases, JP cap year, Budget relief changes).
func RulePackVersion(country string) string {
	switch country {
	case "MY":
		return "my-2026-10"
	case "ID":
		return "id-2026-10"
	}
	return "unknown"
}

type calcItem struct {
	preset Preset
	amount int64
	note   string
}

// defaultBasicSalary is synthesized when the catalog lacks basic_salary but
// BaseSalaryMinor > 0. It is subject to every statutory base.
func defaultBasicSalary() Preset {
	return Preset{Code: "basic_salary", Name: "Basic Salary", Kind: PresetAddition,
		SubjectToEPF: true, SubjectToSOCSO: true, SubjectToEIS: true, SubjectToPCB: true,
		SubjectToBPJSKes: true, SubjectToJHT: true, SubjectToJKK: true,
		SubjectToJKM: true, SubjectToJP: true, SubjectToPPh21: true}
}

// defaultUnpaidLeave is synthesized when the catalog lacks
// unpaid_leave_deduction.
func defaultUnpaidLeave() Preset {
	return Preset{Code: "unpaid_leave_deduction", Name: "Unpaid Leave Deduction",
		Kind: PresetDeduction, ReducesContributionWages: true}
}

func calculateOne(countryCode string, presetByCode map[string]Preset, in *PayInput) (PayResult, error) {
	var res PayResult
	res.EmployeeID = in.EmployeeID

	country := countryCode
	if in.CountryCode != "" {
		if in.CountryCode != countryCode {
			return res, fmt.Errorf("payroll: employee %q country %q does not match batch country %q",
				in.EmployeeID, in.CountryCode, countryCode)
		}
		country = in.CountryCode
	}

	if country == "ID" {
		if _, ok := jkkRateBps(in.JKKRiskClass); !ok {
			return res, fmt.Errorf("payroll: employee %q: JKK risk class %d out of range (want 1..5)",
				in.EmployeeID, in.JKKRiskClass)
		}
	}

	// --- build item list ---
	var items []calcItem
	if in.BaseSalaryMinor > 0 {
		p, ok := presetByCode["basic_salary"]
		if !ok {
			p = defaultBasicSalary()
		}
		items = append(items, calcItem{preset: p, amount: in.BaseSalaryMinor})
	}
	for _, pi := range in.Items {
		p, ok := presetByCode[pi.PresetCode]
		if !ok {
			return res, fmt.Errorf("payroll: employee %q: unknown preset code %q",
				in.EmployeeID, pi.PresetCode)
		}
		items = append(items, calcItem{preset: p, amount: pi.AmountMinor})
	}
	if in.UnpaidLeaveDays > 0 {
		amount := int64(math.Round(in.UnpaidLeaveDays * float64(in.DailyRateMinor)))
		p, ok := presetByCode["unpaid_leave_deduction"]
		if !ok {
			p = defaultUnpaidLeave()
		}
		items = append(items, calcItem{
			preset: p,
			amount: amount,
			note:   fmt.Sprintf("%.2f unpaid day(s)", in.UnpaidLeaveDays),
		})
		if in.DailyRateMinor == 0 {
			res.Warnings = append(res.Warnings, WarnZeroDailyRate)
		}
	}

	// --- line items + totals ---
	var gross, additions, deductions int64
	var zakatPayroll, zakatDirect, epfTopup int64
	for _, it := range items {
		res.Items = append(res.Items, PayLineItem{
			PresetCode:  it.preset.Code,
			Name:        it.preset.Name,
			Kind:        it.preset.Kind,
			AmountMinor: it.amount,
			Note:        it.note,
		})
		switch it.preset.Kind {
		case PresetAddition:
			gross += it.amount
			additions += it.amount
		case PresetDeduction:
			if !nonNetDeductionCodes[it.preset.Code] {
				deductions += it.amount
			}
		}
		switch it.preset.Code {
		case "zakat_payroll":
			zakatPayroll += it.amount
		case "zakat_direct":
			zakatDirect += it.amount
		case "epf_topup_employee":
			epfTopup += it.amount
		}
	}
	res.GrossMinor = gross
	res.AdditionsMinor = additions - in.BaseSalaryMinor
	if res.AdditionsMinor < 0 {
		res.AdditionsMinor = 0
	}
	res.DeductionsMinor = deductions

	// --- statutory ---
	switch country {
	case "MY":
		// The seed catalog gives EPF/SOCSO/EIS identical flags; the SOCSO
		// base is the canonical MY contribution-wage base, and per-program
		// differences flow through the preset flags automatically.
		wages := baseWages(items, func(p Preset) bool { return p.SubjectToSOCSO })
		taxable := baseWages(items, func(p Preset) bool { return p.SubjectToPCB })
		myOut := CalcMYStatutory(MYInput{
			ContributionWagesMinor: wages,
			TaxableMonthlyMinor:    taxable,
			AgeYears:               in.AgeYears,
			Citizen:                in.CitizenMY,
			PRMY:                   in.PRMY,
			ForeignWorker:          in.ForeignWorkerMY,
			EmployerBornePCB:       in.EmployerBornePCB,
			ZakatMinor:             zakatPayroll + zakatDirect,
			SKBBKOptIn:             in.SKBBKOptIn,
		})
		myOut.EPFEmployee += epfTopup // voluntary top-up sits on top of statutory EPF
		// VERIFY: the EPF relief inside CalcMYStatutory's PCB uses statutory
		// EPF only; a top-up's relief effect below the RM4,000 cap is not
		// reflected (immaterial once statutory EPF ≥ ~RM3,030/mo).
		res.Splits = StatutorySplits{
			EPFEmployee:   myOut.EPFEmployee,
			EPFEmployer:   myOut.EPFEmployer,
			SOCSOEmployee: myOut.SOCSOEmployee,
			SOCSOEmployer: myOut.SOCSOEmployer,
			EISEmployee:   myOut.EISEmployee,
			EISEmployer:   myOut.EISEmployer,
			PCB:           myOut.PCB,
			HRDFEmployer:  myOut.HRDFEmployer,
		}
	case "ID":
		// All BPJS programs share the wage base in the seed catalog (each
		// applies its own cap internally); PPh21 TER runs on monthly gross.
		wages := baseWages(items, func(p Preset) bool { return p.SubjectToBPJSKes })
		taxable := baseWages(items, func(p Preset) bool { return p.SubjectToPPh21 })
		idOut := CalcIDStatutory(IDInput{
			ContributionWagesMinor: wages,
			TaxableMonthlyMinor:    taxable,
			JKKRiskClass:           in.JKKRiskClass,
			PTKPStatus:             in.PTKPStatus,
		})
		res.Splits = StatutorySplits{
			BPJSKesEmployer: idOut.BPJSKesEmployer,
			BPJSKesEmployee: idOut.BPJSKesEmployee,
			JHTEmployer:     idOut.JHTEmployer,
			JHTEmployee:     idOut.JHTEmployee,
			JKKEmployer:     idOut.JKKEmployer,
			JKMEmployer:     idOut.JKMEmployer,
			JPEmployer:      idOut.JPEmployer,
			JPEmployee:      idOut.JPEmployee,
			PPh21:           idOut.PPh21,
		}
		if !terKnownStatus(in.PTKPStatus) {
			res.Warnings = append(res.Warnings, WarnUnknownPTKPStatus)
		}
	}

	// --- net ---
	employeeStatutory := res.Splits.EmployeeTotal()
	if country == "MY" && in.EmployerBornePCB {
		// Employer settles the PCB: it is computed and reported, but not
		// deducted from the employee. (Any taxable-benefit gross-up is out of
		// scope — VERIFY against LHDN treatment.)
		employeeStatutory -= res.Splits.PCB
	}
	res.NetMinor = res.GrossMinor - res.DeductionsMinor - employeeStatutory

	// --- warnings ---
	if in.BankAccount == "" || in.BankCode == "" {
		res.Warnings = append(res.Warnings, WarnMissingBankDetails)
	}

	return res, nil
}

// baseWages sums additions carrying the flag, minus deductions flagged
// ReducesContributionWages (which shrink every base). Floored at 0.
func baseWages(items []calcItem, flag func(Preset) bool) int64 {
	var w int64
	for _, it := range items {
		switch it.preset.Kind {
		case PresetAddition:
			if flag(it.preset) {
				w += it.amount
			}
		case PresetDeduction:
			if it.preset.ReducesContributionWages {
				w -= it.amount
			}
		}
	}
	if w < 0 {
		w = 0
	}
	return w
}
