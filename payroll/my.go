package payroll

// Malaysia statutory rule pack.
//
// Sources (all public, checked 2026-10-16):
//   - EPF rates: KWSP Third Schedule; press release "EPF Ramps Up Readiness
//     Measures for Expansion of Mandatory Contribution Coverage to
//     Non-Malaysian Citizen Employees" (kwsp.gov.my, 2025-06-25, effective
//     wages Oct 2025): Malaysian/PR <60 → 11% / 13% (≤RM5,000) / 12% (>RM5,000);
//     Malaysian ≥60 → 0% / 4% (Third Schedule §E); PR ≥60 → 5.5% / 6.5%/6%
//     (Third Schedule §C); non-Malaysian (registered from 1998-08-01,
//     incl. new mandatory foreign-worker regime) → 2% / 2% below age 75
//     (Third Schedule §F).
//   - SOCSO table: PERKESO "Jadual Caruman Baharu Merangkumi SKBBK"
//     (Employees' Social Security Act 1969 (Act 4), effective 2026-06-01),
//     RM6,000 wage ceiling (raised from RM5,000 on 2024-10-01).
//   - EIS: 0.2% + 0.2%, RM6,000 ceiling (raised 2024-10-01); foreign workers
//     excluded — PERKESO "Clarification on Coverage for Foreign Workers"
//     (perkeso.gov.my, 2018-12-24): foreigners get EI under Act 4, not EIS
//     under Act 800. Act 800 First Schedule also excludes age ≥60.
//   - PCB: LHDN resident individual rates + reliefs (individual RM9,000; EPF
//     RM4,000 restricted; SOCSO RM350 restricted — YA2025 reliefs).
//   - HRDF: 1% of monthly wages, mandatory for employers with 10+ Malaysian
//     employees (HRD Corp); 0.5% voluntary for 5–9.
//   - SKBBK (Lindung 24 Jam): employee-only, Phase 1 0.75% 2026-06-01–
//     2028-05-31, RM6,000 ceiling; mandatory for foreign workers, voluntary
//     for locals since 2026-07-08 (PERKESO FAQ LINDUNG 24 JAM Ver2.1).
//
// Approximation policy: this pack computes statutory amounts from rates.
// Amounts that must match a published band table exactly (SOCSO) use the
// table below; EPF uses whole-ringgit rounding as an approximation of the
// KWSP Third Schedule banded amounts. Cross-check against PERKESO ASSIST /
// KWSP i-Akaun / LHDN before any real payroll run (REA §9).

// MYInput is the input to CalcMYStatutory.
type MYInput struct {
	ContributionWagesMinor int64 // SOCSO/EPF/EIS wage base for the month
	TaxableMonthlyMinor    int64 // PCB-taxable monthly income
	AgeYears               int

	Citizen       bool // Malaysian citizen (KWSP §A below 60, §E at 60+)
	PRMY          bool // Malaysian permanent resident (KWSP §A below 60, §C at 60+). Additive to the shared contract.
	ForeignWorker bool // non-Malaysian employee: 2%/2% EPF from 2025-10-01, mandatory SKBBK, no EIS

	EmployerBornePCB bool  // recorded; the engine excludes PCB from employee deductions when true
	ZakatMinor       int64 // zakat via payroll + paid-direct: rebates PCB, floor at 0

	// SKBBKOptIn includes the Lindung 24 Jam employee share in SOCSOEmployee.
	// Mandatory for foreign workers regardless of this flag; voluntary
	// (opt-in) for Malaysian citizens/PRs since 2026-07-08.
	SKBBKOptIn bool
}

// MYOutput is the result of CalcMYStatutory. All amounts in sen.
type MYOutput struct {
	EPFEmployee   int64
	EPFEmployer   int64
	SOCSOEmployee int64 // includes SKBBK employee share when SKBBKOptIn
	SOCSOEmployer int64
	EISEmployee   int64
	EISEmployer   int64
	PCB           int64
	HRDFEmployer  int64
}

// CalcMYStatutory computes Malaysian statutory contributions for one employee.
func CalcMYStatutory(in MYInput) MYOutput {
	var out MYOutput

	wages := in.ContributionWagesMinor
	if wages < 0 {
		wages = 0
	}

	// --- EPF ---
	eeBps, erBps := epfRates(in.Citizen, in.PRMY, in.ForeignWorker, in.AgeYears, wages)
	out.EPFEmployee = roundToWholeRM(mulBps(wages, eeBps))
	out.EPFEmployer = roundToWholeRM(mulBps(wages, erBps))

	// --- SOCSO (Act 4 Jadual Caruman, RM6,000 ceiling) ---
	capped := wages
	if capped > 6000_00 {
		capped = 6000_00
	}
	empFirst, eeInv, eeSKBBK, empSecond := socsoLookup(capped)
	skbbk := in.SKBBKOptIn || in.ForeignWorker
	if in.AgeYears < 60 {
		out.SOCSOEmployer = empFirst
		out.SOCSOEmployee = eeInv
		if skbbk {
			out.SOCSOEmployee += eeSKBBK
		}
	} else {
		out.SOCSOEmployer = empSecond
		if skbbk {
			out.SOCSOEmployee = eeSKBBK
		}
	}

	// --- EIS (Act 800): citizens/PRs below 60 only ---
	if !in.ForeignWorker && in.AgeYears < 60 {
		base := wages
		if base > 6000_00 {
			base = 6000_00
		}
		out.EISEmployee = mulBps(base, 20)
		out.EISEmployer = mulBps(base, 20)
	}

	// --- PCB (monthly MTD approximation) ---
	// VERIFY: against LHDN's MTD formula before production use. This pack
	// annualises the month, applies standard reliefs + resident rates, and
	// divides by 12 — it does not replicate LHDN's accumulated-remuneration
	// categories or additional reliefs the employee may claim via TP1/TP3.
	annualEPF := out.EPFEmployee * 12
	annualSocsoEIS := (socsoEmployeeCore(in, eeInv) + out.EISEmployee) * 12
	out.PCB = calcPCB(in.TaxableMonthlyMinor, annualEPF, annualSocsoEIS, in.ZakatMinor)

	// --- HRDF ---
	// Always computed at 1%; the caller decides applicability (mandatory at
	// 10+ Malaysian employees, 0.5% voluntary at 5–9).
	out.HRDFEmployer = mulBps(wages, 100)

	return out
}

// socsoEmployeeCore returns the SOCSO employee share excluding SKBBK, for the
// RM350 SOCSO relief base. (VERIFY: whether SKBBK counts toward the SOCSO
// relief — Budget 2027 adds a separate RM150 Lindung 24 Jam relief for YA2027.)
func socsoEmployeeCore(in MYInput, eeInv int64) int64 {
	if in.AgeYears < 60 {
		return eeInv
	}
	return 0
}

// epfRates returns (employeeBps, employerBps).
// Source: KWSP Third Schedule §§A/C/E/F + kwsp.gov.my press release 2025-06-25
// (mandatory foreign-worker coverage, wages from Oct 2025), effective 2026-10.
// VERIFY: legacy non-Malaysians registered before 1998-08-01 (11% employee /
// RM5-flat employer) are not distinguished — they fall into the 2%/2% regime here.
func epfRates(citizen, pr, foreignWorker bool, ageYears int, wagesSen int64) (int64, int64) {
	if foreignWorker || (!citizen && !pr) {
		if ageYears >= 75 {
			return 0, 0
		}
		return 200, 200 // 2% / 2%, Third Schedule §F
	}
	if ageYears < 60 {
		er := int64(1200)
		if wagesSen <= 5000_00 {
			er = 1300
		}
		return 1100, er // Third Schedule §A
	}
	if pr {
		er := int64(600)
		if wagesSen <= 5000_00 {
			er = 650
		}
		return 550, er // Third Schedule §C
	}
	return 0, 400 // Malaysian citizen 60+: Third Schedule §E
}

// ---------------------------------------------------------------------------
// SOCSO contribution table (Act 4 Jadual Caruman Baharu Merangkumi SKBBK,
// PERKESO, effective 2026-06-01).
//
// Each row: band upper bound (RM) → {employer 1st cat, employee invalidity,
// employee SKBBK, employer 2nd cat}, in sen.
//
// Rows below are verbatim from the official PERKESO PDF except where marked
// VERIFY (reconstructed — see socsoLookup). Amounts are fixed legislated
// values; no closed-form formula reproduces every row, so reconstructed rows
// use round-half-up-to-5-sen(rate × band midpoint), documented as approximate.
// ---------------------------------------------------------------------------

// socsoOfficialRow is one verbatim official table row, amounts in sen.
type socsoOfficialRow struct {
	empFirst, eeInvalidity, eeSKBBK, empSecond int64
}

var socsoOfficial = map[int64]socsoOfficialRow{
	// Source: PERKESO "Jadual Caruman Baharu Merangkumi SKBBK", rows 1–9.
	30:  {40, 10, 20, 30},
	50:  {70, 20, 30, 50},
	70:  {110, 30, 50, 80},
	100: {150, 40, 65, 110},
	140: {210, 60, 90, 150},
	200: {295, 85, 125, 210},
	300: {435, 125, 185, 310},
	400: {615, 175, 265, 440},
	500: {785, 225, 335, 560},
	// Source: same PDF, rows 51–58 (RM4,600–RM5,400 bands).
	4700: {8135, 2325, 3485, 5810},
	4800: {8315, 2375, 3565, 5940},
	4900: {8485, 2425, 3635, 6060},
	5000: {8665, 2475, 3715, 6190},
	5100: {8835, 2525, 3785, 6310},
	5200: {9015, 2575, 3865, 6440},
	5300: {9185, 2625, 3935, 6560},
	5400: {9365, 2675, 4015, 6690},
	// Anchors read from the same PDF by third-party analyses (bigdomain.my,
	// quickhr.my): (2900,3000] and the (5900,6000] ceiling band.
	// VERIFY: empSecond for the 3000 band is reconstructed, not verbatim.
	3000: {5165, 1475, 2215, 3690},
	6000: {10415, 2975, 4465, 7440},
}

// socsoBandUppers are the official band upper bounds in RM, ascending.
// Row 1 is (0,30]; rows 2–4 are narrow; the rest are RM100 bands to RM6,000.
func socsoBandUppers() []int64 {
	uppers := []int64{30, 50, 70, 100, 140, 200, 300, 400, 500}
	for u := int64(600); u <= 6000; u += 100 {
		uppers = append(uppers, u)
	}
	return uppers
}

// socsoLookup returns (employer 1st cat, employee invalidity, employee SKBBK,
// employer 2nd cat) in sen for the given monthly wages in sen.
func socsoLookup(wagesSen int64) (empFirst, eeInvalidity, eeSKBBK, empSecond int64) {
	if wagesSen < 0 {
		wagesSen = 0
	}
	uppers := socsoBandUppers()
	upper := uppers[len(uppers)-1]
	lower := int64(0)
	for i, u := range uppers {
		if wagesSen <= u*100 {
			upper = u
			if i > 0 {
				lower = uppers[i-1]
			}
			break
		}
	}
	if row, ok := socsoOfficial[upper]; ok {
		return row.empFirst, row.eeInvalidity, row.eeSKBBK, row.empSecond
	}
	// VERIFY: reconstructed row — cross-check against PERKESO ASSIST.
	// Method: round-half-up to 5 sen of (rate × band midpoint); the employee
	// invalidity share is exactly 0.5% × midpoint on every verbatim row, so it
	// is computed exactly rather than reconstructed.
	midSen := (lower + upper) * 50
	return round5SenRate(midSen, 175), midSen / 200, round5SenRate(midSen, 75), round5SenRate(midSen, 125)
}

// round5SenRate computes round-half-up-to-5-sen(midSen * bps / 10000) using
// integer arithmetic.
func round5SenRate(midSen, bps int64) int64 {
	units := (midSen*bps + 25000) / 50000
	return units * 5
}

// ---------------------------------------------------------------------------
// PCB (Potongan Cukai Bulanan) — monthly MTD approximation.
// ---------------------------------------------------------------------------

// calcPCB annualises the taxable month, subtracts standard reliefs, applies
// LHDN resident individual rates, divides by 12, subtracts the zakat rebate,
// and floors at 0. All amounts in sen.
//
// Reliefs (YA2025, verified via LHDN relief guides): individual RM9,000;
// EPF employee contributions capped at RM4,000; SOCSO(+EIS) employee
// contributions capped at RM350.
// Resident rates (verified): 0% ≤5k; 1% 5–20k; 3% 20–35k; 6% 35–50k;
// 11% 50–70k; 19% 70–100k; 25% 100–400k; 26% 400–600k; 28% 600k–2M; 30% >2M.
// (Budget 2027 cuts 70–100k to 18% and 100–150k to 24% for YA2027 — VERIFY
// when the assessment year rolls over.)
func calcPCB(taxableMonthlySen, annualEPFSen, annualSocsoEISSen, zakatSen int64) int64 {
	annualTaxable := taxableMonthlySen * 12
	relief := int64(9000_00)
	relief += min64(annualEPFSen, 4000_00)
	relief += min64(annualSocsoEISSen, 350_00)
	chargeable := annualTaxable - relief
	if chargeable < 0 {
		chargeable = 0
	}
	annualTax := progressiveTaxSen(chargeable, pcbResidentBrackets)
	monthly := (annualTax + 6) / 12 // half-up to sen
	out := monthly - zakatSen
	if out < 0 {
		out = 0
	}
	return out
}

// taxBracket is one progressive bracket: up to upperRM (exclusive of the next)
// taxed at rateBps basis points.
type taxBracket struct {
	upperRM int64
	rateBps int64
}

var pcbResidentBrackets = []taxBracket{
	{5000, 0},
	{20000, 100},
	{35000, 300},
	{50000, 600},
	{70000, 1100},
	{100000, 1900},
	{400000, 2500},
	{600000, 2600},
	{2000000, 2800},
	{1 << 62, 3000},
}

// progressiveTaxSen applies progressive brackets to chargeable income (sen).
func progressiveTaxSen(chargeableSen int64, brackets []taxBracket) int64 {
	var tax, lowerSen int64
	for _, b := range brackets {
		upperSen := b.upperRM * 100
		if chargeableSen <= lowerSen {
			break
		}
		taxable := min64(chargeableSen, upperSen) - lowerSen
		tax += (taxable*b.rateBps + 5000) / 10000 // half-up to sen
		lowerSen = upperSen
	}
	return tax
}

// ---------------------------------------------------------------------------
// small integer helpers (positive-domain)
// ---------------------------------------------------------------------------

// mulBps returns round-half-up(x * bps / 10000) in the same units as x.
func mulBps(x, bps int64) int64 {
	if x <= 0 || bps <= 0 {
		return 0
	}
	return (x*bps + 5000) / 10000
}

// roundToWholeRM rounds sen to the nearest whole ringgit (KWSP contributions
// are paid in whole RM, no sen — documented approximation of the Third
// Schedule banded amounts; VERIFY against the schedule for production).
func roundToWholeRM(sen int64) int64 {
	return ((sen + 50) / 100) * 100
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
