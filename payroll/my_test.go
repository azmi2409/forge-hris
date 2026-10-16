package payroll

import "testing"

func TestEPFAgeBands(t *testing.T) {
	cases := []struct {
		name           string
		in             MYInput
		wantEE, wantER int64 // sen
	}{
		{
			"citizen 59, RM5000",
			MYInput{ContributionWagesMinor: 5000_00, AgeYears: 59, Citizen: true},
			550_00, 650_00, // 11% / 13%
		},
		{
			"citizen 59, RM5000.01 -> employer 12%",
			MYInput{ContributionWagesMinor: 5000_01, AgeYears: 59, Citizen: true},
			550_00, 600_00, // 11% of 5000.01 rounds to 550; 12%
		},
		{
			"citizen 59, RM6000 -> employer 12%",
			MYInput{ContributionWagesMinor: 6000_00, AgeYears: 59, Citizen: true},
			660_00, 720_00,
		},
		{
			"citizen exactly 60 -> Section E 0%/4%",
			MYInput{ContributionWagesMinor: 5000_00, AgeYears: 60, Citizen: true},
			0, 200_00,
		},
		{
			"citizen 61 -> Section E 0%/4%",
			MYInput{ContributionWagesMinor: 8000_00, AgeYears: 61, Citizen: true},
			0, 320_00,
		},
		{
			"PR 60 -> Section C 5.5%/6.5%",
			MYInput{ContributionWagesMinor: 5000_00, AgeYears: 60, PRMY: true},
			275_00, 325_00,
		},
		{
			"PR 65, RM6000 -> Section C 5.5%/6%",
			MYInput{ContributionWagesMinor: 6000_00, AgeYears: 65, PRMY: true},
			330_00, 360_00,
		},
		{
			"PR 45 -> same as citizen (Section A)",
			MYInput{ContributionWagesMinor: 4000_00, AgeYears: 45, PRMY: true},
			440_00, 520_00,
		},
		{
			"foreign worker 30 -> 2%/2%",
			MYInput{ContributionWagesMinor: 3000_00, AgeYears: 30, ForeignWorker: true},
			60_00, 60_00,
		},
		{
			"foreign worker 74 -> 2%/2%",
			MYInput{ContributionWagesMinor: 3000_00, AgeYears: 74, ForeignWorker: true},
			60_00, 60_00,
		},
		{
			"foreign worker 75 -> exempt",
			MYInput{ContributionWagesMinor: 3000_00, AgeYears: 75, ForeignWorker: true},
			0, 0,
		},
		{
			"whole-RM rounding: 11% of RM4999 = RM549.89 -> RM550",
			MYInput{ContributionWagesMinor: 4999_00, AgeYears: 30, Citizen: true},
			550_00, 650_00, // employer 13% of 4999 = 649.87 -> 650
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := CalcMYStatutory(c.in)
			if out.EPFEmployee != c.wantEE || out.EPFEmployer != c.wantER {
				t.Errorf("EPF = (%d, %d), want (%d, %d)",
					out.EPFEmployee, out.EPFEmployer, c.wantEE, c.wantER)
			}
		})
	}
}

func TestSOCSOTable(t *testing.T) {
	// Verbatim official rows (PERKESO Jadual Caruman Baharu Merangkumi SKBBK).
	cases := []struct {
		name                                string
		wagesSen                            int64
		empFirst, eeInv, eeSKBBK, empSecond int64
	}{
		{"band up to RM30", 3000, 40, 10, 20, 30},
		{"band (30,50]", 5000, 70, 20, 30, 50},
		{"band (400,500]", 50000, 785, 225, 335, 560},
		{"band (2900,3000]", 300000, 5165, 1475, 2215, 3690}, // empSecond VERIFY-marked
		{"band (4600,4700]", 465000, 8135, 2325, 3485, 5810},
		{"band (5300,5400]", 540000, 9365, 2675, 4015, 6690},
		{"ceiling band (5900,6000]", 600000, 10415, 2975, 4465, 7440},
		{"above ceiling clamps to RM6000 band", 700000, 10415, 2975, 4465, 7440},
		{"zero wages", 0, 40, 10, 20, 30},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ef, ei, es, e2 := socsoLookup(c.wagesSen)
			if ef != c.empFirst || ei != c.eeInv || es != c.eeSKBBK || e2 != c.empSecond {
				t.Errorf("socsoLookup(%d) = (%d,%d,%d,%d), want (%d,%d,%d,%d)",
					c.wagesSen, ef, ei, es, e2, c.empFirst, c.eeInv, c.eeSKBBK, c.empSecond)
			}
		})
	}
}

func TestSOCSOCategoryByAge(t *testing.T) {
	in := MYInput{ContributionWagesMinor: 3000_00, Citizen: true}

	in.AgeYears = 59
	out := CalcMYStatutory(in)
	if out.SOCSOEmployer != 5165 || out.SOCSOEmployee != 1475 {
		t.Errorf("age 59 first category = (%d,%d), want (5165,1475)",
			out.SOCSOEmployer, out.SOCSOEmployee)
	}

	in.AgeYears = 60
	out = CalcMYStatutory(in)
	if out.SOCSOEmployee != 0 {
		t.Errorf("age 60 second category employee should be 0 without SKBBK, got %d", out.SOCSOEmployee)
	}
	if out.SOCSOEmployer == 0 {
		t.Error("age 60 second category employer should be non-zero")
	}
}

func TestSKBBK(t *testing.T) {
	// Local opt-in: SKBBK added to the invalidity share.
	in := MYInput{ContributionWagesMinor: 3000_00, AgeYears: 30, Citizen: true, SKBBKOptIn: true}
	out := CalcMYStatutory(in)
	if out.SOCSOEmployee != 1475+2215 {
		t.Errorf("SKBBK opt-in employee = %d, want %d", out.SOCSOEmployee, 1475+2215)
	}
	if out.SOCSOEmployer != 5165 {
		t.Errorf("employer unchanged by SKBBK = %d, want 5165", out.SOCSOEmployer)
	}

	// No opt-in: no SKBBK.
	in.SKBBKOptIn = false
	out = CalcMYStatutory(in)
	if out.SOCSOEmployee != 1475 {
		t.Errorf("no opt-in employee = %d, want 1475", out.SOCSOEmployee)
	}

	// Foreign worker: mandatory even without opt-in.
	in = MYInput{ContributionWagesMinor: 3000_00, AgeYears: 30, ForeignWorker: true}
	out = CalcMYStatutory(in)
	if out.SOCSOEmployee != 1475+2215 {
		t.Errorf("foreign worker SKBBK mandatory, employee = %d, want %d", out.SOCSOEmployee, 1475+2215)
	}

	// Age 60+ with SKBBK: employee pays SKBBK only.
	in = MYInput{ContributionWagesMinor: 3000_00, AgeYears: 65, Citizen: true, SKBBKOptIn: true}
	out = CalcMYStatutory(in)
	if out.SOCSOEmployee != 2215 {
		t.Errorf("60+ with SKBBK employee = %d, want 2215", out.SOCSOEmployee)
	}
}

func TestEIS(t *testing.T) {
	// Citizen below 60: 0.2% + 0.2%.
	out := CalcMYStatutory(MYInput{ContributionWagesMinor: 3000_00, AgeYears: 30, Citizen: true})
	if out.EISEmployee != 600 || out.EISEmployer != 600 {
		t.Errorf("EIS = (%d,%d), want (600,600)", out.EISEmployee, out.EISEmployer)
	}

	// Ceiling RM6,000.
	out = CalcMYStatutory(MYInput{ContributionWagesMinor: 9000_00, AgeYears: 30, Citizen: true})
	if out.EISEmployee != 1200 || out.EISEmployer != 1200 {
		t.Errorf("EIS ceiling = (%d,%d), want (1200,1200)", out.EISEmployee, out.EISEmployer)
	}

	// Foreign workers excluded (PERKESO clarification).
	out = CalcMYStatutory(MYInput{ContributionWagesMinor: 3000_00, AgeYears: 30, ForeignWorker: true})
	if out.EISEmployee != 0 || out.EISEmployer != 0 {
		t.Errorf("foreign worker EIS should be 0, got (%d,%d)", out.EISEmployee, out.EISEmployer)
	}

	// Age 60+ excluded (Act 800 First Schedule).
	out = CalcMYStatutory(MYInput{ContributionWagesMinor: 3000_00, AgeYears: 60, Citizen: true})
	if out.EISEmployee != 0 || out.EISEmployer != 0 {
		t.Errorf("age 60 EIS should be 0, got (%d,%d)", out.EISEmployee, out.EISEmployer)
	}
}

func TestPCB(t *testing.T) {
	// Low income -> 0.
	out := CalcMYStatutory(MYInput{
		ContributionWagesMinor: 1000_00, TaxableMonthlyMinor: 1000_00,
		AgeYears: 30, Citizen: true,
	})
	if out.PCB != 0 {
		t.Errorf("low income PCB = %d, want 0", out.PCB)
	}

	// Hand-computed: RM10,000/month taxable, citizen 30.
	// Reliefs: 9000 + min(13200,4000)=4000 + min(501,350)=350 → chargeable RM106,650.
	// Annual tax RM11,062.50 → monthly RM921.88.
	out = CalcMYStatutory(MYInput{
		ContributionWagesMinor: 10000_00, TaxableMonthlyMinor: 10000_00,
		AgeYears: 30, Citizen: true,
	})
	if out.PCB != 92188 {
		t.Errorf("PCB = %d, want 92188", out.PCB)
	}

	// Zakat rebate reduces PCB.
	out = CalcMYStatutory(MYInput{
		ContributionWagesMinor: 10000_00, TaxableMonthlyMinor: 10000_00,
		AgeYears: 30, Citizen: true, ZakatMinor: 500_00,
	})
	if out.PCB != 92188-50000 {
		t.Errorf("PCB with zakat = %d, want %d", out.PCB, 92188-50000)
	}

	// Zakat beyond PCB floors at 0.
	out = CalcMYStatutory(MYInput{
		ContributionWagesMinor: 10000_00, TaxableMonthlyMinor: 10000_00,
		AgeYears: 30, Citizen: true, ZakatMinor: 99999_00,
	})
	if out.PCB != 0 {
		t.Errorf("PCB floored = %d, want 0", out.PCB)
	}
}

func TestHRDF(t *testing.T) {
	out := CalcMYStatutory(MYInput{ContributionWagesMinor: 5000_00, AgeYears: 30, Citizen: true})
	if out.HRDFEmployer != 5000 {
		t.Errorf("HRDF = %d, want 5000 (1%% of RM5000)", out.HRDFEmployer)
	}
}

func TestProgressiveTax(t *testing.T) {
	// RM60,000 chargeable: 1%×15k + 3%×15k + 6%×15k + 11%×10k = 150+450+900+1100 = RM2,600.
	got := progressiveTaxSen(60000_00, pcbResidentBrackets)
	if got != 2600_00 {
		t.Errorf("progressive tax = %d, want %d", got, 2600_00)
	}
	if progressiveTaxSen(0, pcbResidentBrackets) != 0 {
		t.Error("zero chargeable should give zero tax")
	}
}
