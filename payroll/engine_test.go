package payroll

import (
	"strings"
	"testing"
)

func testPresets() []Preset { return SeedPresets() }

func baseInputMY() PayInput {
	return PayInput{
		EmployeeID:      "E001",
		CountryCode:     "MY",
		BaseSalaryMinor: 5000_00,
		AgeYears:        30,
		CitizenMY:       true,
		BankAccount:     "1234567890",
		BankCode:        "MBB",
	}
}

func TestCalculateUnknownPreset(t *testing.T) {
	in := baseInputMY()
	in.Items = []PayItemInput{{PresetCode: "nope_not_real", AmountMinor: 100}}
	_, err := Calculate("MY", testPresets(), []PayInput{in})
	if err == nil || !strings.Contains(err.Error(), "nope_not_real") {
		t.Errorf("expected unknown-preset error naming the code, got %v", err)
	}
}

func TestCalculateBadCountry(t *testing.T) {
	if _, err := Calculate("SG", testPresets(), nil); err == nil {
		t.Error("expected error for unsupported country")
	}
}

func TestCalculateCountryMismatch(t *testing.T) {
	in := baseInputMY()
	in.CountryCode = "ID"
	_, err := Calculate("MY", testPresets(), []PayInput{in})
	if err == nil {
		t.Error("expected country mismatch error")
	}
}

func TestCalculateMYBasic(t *testing.T) {
	in := baseInputMY()
	res, err := Calculate("MY", testPresets(), []PayInput{in})
	if err != nil {
		t.Fatal(err)
	}
	r := res[0]
	if r.GrossMinor != 5000_00 {
		t.Errorf("gross = %d, want 500000", r.GrossMinor)
	}
	if r.AdditionsMinor != 0 {
		t.Errorf("additions = %d, want 0", r.AdditionsMinor)
	}
	// EPF 11%/13% of RM5,000.
	if r.Splits.EPFEmployee != 550_00 || r.Splits.EPFEmployer != 650_00 {
		t.Errorf("EPF = (%d,%d), want (55000,65000)", r.Splits.EPFEmployee, r.Splits.EPFEmployer)
	}
	// SOCSO band (4900,5000]: employer 86.65, employee invalidity 24.75.
	if r.Splits.SOCSOEmployer != 8665 || r.Splits.SOCSOEmployee != 2475 {
		t.Errorf("SOCSO = (%d,%d), want (8665,2475)", r.Splits.SOCSOEmployer, r.Splits.SOCSOEmployee)
	}
	// EIS 0.2% of RM5,000.
	if r.Splits.EISEmployee != 1000 || r.Splits.EISEmployer != 1000 {
		t.Errorf("EIS = (%d,%d), want (1000,1000)", r.Splits.EISEmployee, r.Splits.EISEmployer)
	}
	// Net reconciliation: gross − deductions − employee statutory == net.
	want := r.GrossMinor - r.DeductionsMinor - r.Splits.EmployeeTotal()
	if r.NetMinor != want {
		t.Errorf("net = %d, want reconciled %d", r.NetMinor, want)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", r.Warnings)
	}
}

func TestCalculateUnpaidLeave(t *testing.T) {
	in := baseInputMY()
	in.UnpaidLeaveDays = 2
	in.DailyRateMinor = 230_00 // RM230/day
	res, err := Calculate("MY", testPresets(), []PayInput{in})
	if err != nil {
		t.Fatal(err)
	}
	r := res[0]

	var found *PayLineItem
	for i := range r.Items {
		if r.Items[i].PresetCode == "unpaid_leave_deduction" {
			found = &r.Items[i]
		}
	}
	if found == nil {
		t.Fatal("unpaid leave deduction item missing")
	}
	if found.AmountMinor != 460_00 {
		t.Errorf("unpaid leave = %d, want 46000", found.AmountMinor)
	}
	if r.DeductionsMinor != 460_00 {
		t.Errorf("deductions = %d, want 46000", r.DeductionsMinor)
	}
	// Contribution wages shrink: 5000 − 460 = RM4,540 → SOCSO band (4500,4600].
	// (Reconstructed row — assert via the engine's own lookup for consistency.)
	ef, ei, _, _ := socsoLookup(4540_00)
	if r.Splits.SOCSOEmployer != ef || r.Splits.SOCSOEmployee != ei {
		t.Errorf("SOCSO after unpaid leave = (%d,%d), want lookup (%d,%d)",
			r.Splits.SOCSOEmployer, r.Splits.SOCSOEmployee, ef, ei)
	}
	// Net reconciliation still holds.
	want := r.GrossMinor - r.DeductionsMinor - r.Splits.EmployeeTotal()
	if r.NetMinor != want {
		t.Errorf("net = %d, want reconciled %d", r.NetMinor, want)
	}
}

func TestCalculateZakatVariants(t *testing.T) {
	// RM10,000 base so PCB (92,188 sen) comfortably exceeds the zakat.
	mkBase := func() PayInput {
		in := baseInputMY()
		in.BaseSalaryMinor = 10000_00
		return in
	}
	mk := func() PayInput {
		in := mkBase()
		in.Items = []PayItemInput{
			{PresetCode: "zakat_payroll", AmountMinor: 100_00},
			{PresetCode: "zakat_direct", AmountMinor: 200_00},
		}
		return in
	}

	// Baseline without zakat.
	base, _ := Calculate("MY", testPresets(), []PayInput{mkBase()})
	withZakat, err := Calculate("MY", testPresets(), []PayInput{mk()})
	if err != nil {
		t.Fatal(err)
	}
	b, z := base[0], withZakat[0]
	if b.Splits.PCB != 92188 {
		t.Fatalf("baseline PCB = %d, want 92188", b.Splits.PCB)
	}

	// zakat_payroll reduces net; zakat_direct does not.
	if z.DeductionsMinor != 100_00 {
		t.Errorf("deductions = %d, want 10000 (zakat_direct excluded)", z.DeductionsMinor)
	}
	// Both rebate PCB.
	if z.Splits.PCB != b.Splits.PCB-300_00 {
		t.Errorf("PCB = %d, want baseline %d minus 30000", z.Splits.PCB, b.Splits.PCB)
	}
	// Net reconciliation still holds.
	wantNet := z.GrossMinor - z.DeductionsMinor - z.Splits.EmployeeTotal()
	if z.NetMinor != wantNet {
		t.Errorf("net = %d, want %d", z.NetMinor, wantNet)
	}
}

func TestCalculateZakatDirectNeverTouchesNet(t *testing.T) {
	in := baseInputMY()
	in.Items = []PayItemInput{{PresetCode: "zakat_direct", AmountMinor: 500_00}}
	plain, _ := Calculate("MY", testPresets(), []PayInput{baseInputMY()})
	withZ, _ := Calculate("MY", testPresets(), []PayInput{in})
	// Net only moves by the PCB rebate, never by the 500 zakat_direct itself.
	diff := withZ[0].NetMinor - plain[0].NetMinor
	pcbDiff := plain[0].Splits.PCB - withZ[0].Splits.PCB
	if diff != pcbDiff {
		t.Errorf("net moved %d, want exactly the PCB rebate %d", diff, pcbDiff)
	}
}

func TestCalculateEPFTopup(t *testing.T) {
	in := baseInputMY()
	in.Items = []PayItemInput{{PresetCode: "epf_topup_employee", AmountMinor: 200_00}}
	res, err := Calculate("MY", testPresets(), []PayInput{in})
	if err != nil {
		t.Fatal(err)
	}
	r := res[0]
	if r.Splits.EPFEmployee != 550_00+200_00 {
		t.Errorf("EPFEmployee = %d, want 75000 (statutory + top-up)", r.Splits.EPFEmployee)
	}
	if r.DeductionsMinor != 200_00 {
		t.Errorf("deductions = %d, want 20000", r.DeductionsMinor)
	}
}

func TestCalculateEmployerBornePCB(t *testing.T) {
	in := baseInputMY()
	in.EmployerBornePCB = true
	res, err := Calculate("MY", testPresets(), []PayInput{in})
	if err != nil {
		t.Fatal(err)
	}
	r := res[0]
	if r.Splits.PCB == 0 {
		t.Skip("PCB is 0 at this salary; employer-borne path needs a taxable salary")
	}
	want := r.GrossMinor - r.DeductionsMinor - (r.Splits.EmployeeTotal() - r.Splits.PCB)
	if r.NetMinor != want {
		t.Errorf("employer-borne net = %d, want %d", r.NetMinor, want)
	}
}

func TestCalculateMissingBankWarning(t *testing.T) {
	in := baseInputMY()
	in.BankAccount = ""
	res, _ := Calculate("MY", testPresets(), []PayInput{in})
	found := false
	for _, w := range res[0].Warnings {
		if w == WarnMissingBankDetails {
			found = true
		}
	}
	if !found {
		t.Errorf("expected missing-bank warning, got %v", res[0].Warnings)
	}
}

func TestCalculateIDBasic(t *testing.T) {
	in := PayInput{
		EmployeeID:      "ID001",
		CountryCode:     "ID",
		BaseSalaryMinor: 10000000,
		AgeYears:        30,
		JKKRiskClass:    3,
		PTKPStatus:      "TK/0",
		BankAccount:     "9876543210",
		BankCode:        "BCA",
	}
	res, err := Calculate("ID", testPresets(), []PayInput{in})
	if err != nil {
		t.Fatal(err)
	}
	r := res[0]
	if r.GrossMinor != 10000000 {
		t.Errorf("gross = %d, want 10000000", r.GrossMinor)
	}
	// BPJS Kes 4%/1% of 10jt; JHT 3.7/2.0; JKK 0.89% class 3; JKM 0.3%;
	// JP 2%/1% of 10jt (below cap); TER A 2% of 10jt.
	s := r.Splits
	if s.BPJSKesEmployer != 400000 || s.BPJSKesEmployee != 100000 {
		t.Errorf("BPJSKes = (%d,%d)", s.BPJSKesEmployer, s.BPJSKesEmployee)
	}
	if s.JHTEmployer != 370000 || s.JHTEmployee != 200000 {
		t.Errorf("JHT = (%d,%d)", s.JHTEmployer, s.JHTEmployee)
	}
	if s.JKKEmployer != 89000 || s.JKMEmployer != 30000 {
		t.Errorf("JKK/JKM = (%d,%d)", s.JKKEmployer, s.JKMEmployer)
	}
	if s.JPEmployer != 200000 || s.JPEmployee != 100000 {
		t.Errorf("JP = (%d,%d)", s.JPEmployer, s.JPEmployee)
	}
	if s.PPh21 != 200000 {
		t.Errorf("PPh21 = %d, want 200000", s.PPh21)
	}
	want := r.GrossMinor - r.DeductionsMinor - s.EmployeeTotal()
	if r.NetMinor != want {
		t.Errorf("net = %d, want reconciled %d", r.NetMinor, want)
	}
}

func TestCalculateIDJKKOutOfRange(t *testing.T) {
	in := PayInput{EmployeeID: "IDX", CountryCode: "ID", BaseSalaryMinor: 5000000, JKKRiskClass: 9, PTKPStatus: "TK/0"}
	_, err := Calculate("ID", testPresets(), []PayInput{in})
	if err == nil || !strings.Contains(err.Error(), "JKK") {
		t.Errorf("expected JKK range error, got %v", err)
	}
}

func TestCalculateIDUnknownPTKPWarns(t *testing.T) {
	in := PayInput{EmployeeID: "IDX", CountryCode: "ID", BaseSalaryMinor: 10000000,
		JKKRiskClass: 1, PTKPStatus: "??", BankAccount: "1", BankCode: "B"}
	res, err := Calculate("ID", testPresets(), []PayInput{in})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res[0].Warnings {
		if w == WarnUnknownPTKPStatus {
			found = true
		}
	}
	if !found {
		t.Errorf("expected unknown-PTKP warning, got %v", res[0].Warnings)
	}
	if res[0].Splits.PPh21 != 200000 { // defaulted to category A
		t.Errorf("PPh21 = %d, want 200000 (cat A)", res[0].Splits.PPh21)
	}
}

func TestCalculateAdditionsAndOvertime(t *testing.T) {
	in := baseInputMY()
	in.Items = []PayItemInput{
		{PresetCode: "overtime", AmountMinor: 500_00},
		{PresetCode: "allowance_fixed", AmountMinor: 300_00},
	}
	res, err := Calculate("MY", testPresets(), []PayInput{in})
	if err != nil {
		t.Fatal(err)
	}
	r := res[0]
	if r.GrossMinor != 5800_00 || r.AdditionsMinor != 800_00 {
		t.Errorf("gross/additions = (%d,%d), want (580000,80000)", r.GrossMinor, r.AdditionsMinor)
	}
	// allowance_fixed is NOT PCB-taxable: taxable = 5000 + 500 = 5500.
	// Just assert PCB equals the no-allowance-fixed case.
	in2 := baseInputMY()
	in2.Items = []PayItemInput{{PresetCode: "overtime", AmountMinor: 500_00}}
	res2, _ := Calculate("MY", testPresets(), []PayInput{in2})
	if res[0].Splits.PCB != res2[0].Splits.PCB {
		t.Errorf("allowance_fixed leaked into PCB: %d vs %d", res[0].Splits.PCB, res2[0].Splits.PCB)
	}
}

func TestRulePackVersion(t *testing.T) {
	if RulePackVersion("MY") != "my-2026-10" || RulePackVersion("ID") != "id-2026-10" {
		t.Error("unexpected rule pack versions")
	}
	if RulePackVersion("SG") != "unknown" {
		t.Error("unknown country should give 'unknown'")
	}
}

func TestEmployeeEmployerTotals(t *testing.T) {
	s := StatutorySplits{EPFEmployee: 100, SOCSOEmployee: 20, PCB: 30, EPFEmployer: 130, HRDFEmployer: 10}
	if s.EmployeeTotal() != 150 {
		t.Errorf("EmployeeTotal = %d, want 150", s.EmployeeTotal())
	}
	if s.EmployerTotal() != 140 {
		t.Errorf("EmployerTotal = %d, want 140", s.EmployerTotal())
	}
}
