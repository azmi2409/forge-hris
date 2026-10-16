package payroll

import "testing"

func TestBPJSKesehatanCaps(t *testing.T) {
	// Below cap: 4% / 1% of full wage.
	out := CalcIDStatutory(IDInput{ContributionWagesMinor: 5000000, TaxableMonthlyMinor: 5000000, JKKRiskClass: 1, PTKPStatus: "TK/0"})
	if out.BPJSKesEmployer != 200000 || out.BPJSKesEmployee != 50000 {
		t.Errorf("BPJSKes = (%d,%d), want (200000,50000)",
			out.BPJSKesEmployer, out.BPJSKesEmployee)
	}

	// Above cap: computed on Rp12,000,000.
	out = CalcIDStatutory(IDInput{ContributionWagesMinor: 20000000, TaxableMonthlyMinor: 20000000, JKKRiskClass: 1, PTKPStatus: "TK/0"})
	if out.BPJSKesEmployer != 480000 || out.BPJSKesEmployee != 120000 {
		t.Errorf("BPJSKes capped = (%d,%d), want (480000,120000)",
			out.BPJSKesEmployer, out.BPJSKesEmployee)
	}
}

func TestJKKRiskClasses(t *testing.T) {
	wages := int64(10000000)
	want := map[int]int64{1: 24000, 2: 54000, 3: 89000, 4: 127000, 5: 174000}
	for class, w := range want {
		out := CalcIDStatutory(IDInput{ContributionWagesMinor: wages, TaxableMonthlyMinor: wages, JKKRiskClass: class, PTKPStatus: "TK/0"})
		if out.JKKEmployer != w {
			t.Errorf("JKK class %d = %d, want %d", class, out.JKKEmployer, w)
		}
	}
}

func TestJHTJKM(t *testing.T) {
	out := CalcIDStatutory(IDInput{ContributionWagesMinor: 10000000, TaxableMonthlyMinor: 10000000, JKKRiskClass: 3, PTKPStatus: "TK/0"})
	if out.JHTEmployer != 370000 || out.JHTEmployee != 200000 {
		t.Errorf("JHT = (%d,%d), want (370000,200000)", out.JHTEmployer, out.JHTEmployee)
	}
	if out.JKMEmployer != 30000 {
		t.Errorf("JKM = %d, want 30000", out.JKMEmployer)
	}
}

func TestJPCap(t *testing.T) {
	// Below cap.
	out := CalcIDStatutory(IDInput{ContributionWagesMinor: 10000000, TaxableMonthlyMinor: 10000000, JKKRiskClass: 1, PTKPStatus: "TK/0"})
	if out.JPEmployer != 200000 || out.JPEmployee != 100000 {
		t.Errorf("JP = (%d,%d), want (200000,100000)", out.JPEmployer, out.JPEmployee)
	}

	// Above the 2026 cap Rp11,086,300: 2% = 221,726; 1% = 110,863.
	out = CalcIDStatutory(IDInput{ContributionWagesMinor: 15000000, TaxableMonthlyMinor: 15000000, JKKRiskClass: 1, PTKPStatus: "TK/0"})
	if out.JPEmployer != 221726 || out.JPEmployee != 110863 {
		t.Errorf("JP capped = (%d,%d), want (221726,110863)", out.JPEmployer, out.JPEmployee)
	}
}

func TestTERCategories(t *testing.T) {
	cases := []struct {
		name    string
		ptkp    string
		gross   int64
		wantBps int64
		wantPPh int64
	}{
		// Category A edges.
		{"A zero band", "TK/0", 5400000, 0, 0},
		{"A first taxable rupiah", "TK/0", 5400001, 25, 13500},
		{"A 10jt -> 2%", "K/0", 10000000, 200, 200000},
		{"A bracket edge 9650000", "TK/1", 9650000, 175, 168875},
		{"A bracket edge 9650001", "TK/1", 9650001, 200, 193000},
		// Category B edges.
		{"B zero band", "TK/2", 6200000, 0, 0},
		{"B first taxable rupiah", "K/1", 6200001, 25, 15500},
		{"B 10jt -> 1.5%", "TK/3", 10000000, 150, 150000},
		{"B edge 9200000 -> 1%", "K/2", 9200000, 100, 92000},
		{"B edge 9200001 -> 1.5%", "K/2", 9200001, 150, 138000},
		// Category C edges.
		{"C zero band", "K/3", 6600000, 0, 0},
		{"C first taxable rupiah", "K/3", 6600001, 25, 16500},
		{"C 10jt -> 1.5%", "K/3", 10000000, 150, 150000},
		{"C edge 8850000 -> 1%", "K/3", 8850000, 100, 88500},
		{"C edge 8850001 -> 1.25%", "K/3", 8850001, 125, 110625},
		// Unknown defaults to A.
		{"unknown PTKP -> A", "XX/9", 10000000, 200, 200000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := CalcIDStatutory(IDInput{
				ContributionWagesMinor: c.gross, TaxableMonthlyMinor: c.gross,
				JKKRiskClass: 1, PTKPStatus: c.ptkp,
			})
			if got := terRateBps(terCategory(c.ptkp), c.gross); got != c.wantBps {
				t.Errorf("TER rate = %d bps, want %d", got, c.wantBps)
			}
			if out.PPh21 != c.wantPPh {
				t.Errorf("PPh21 = %d, want %d", out.PPh21, c.wantPPh)
			}
		})
	}
}

func TestTERCaseInsensitive(t *testing.T) {
	if terCategory("tk/0") != 'A' || terCategory("k/3") != 'C' || terCategory("K/1") != 'B' {
		t.Error("TER category mapping should be case-insensitive")
	}
	if terKnownStatus("bogus") {
		t.Error("bogus PTKP status should not be known")
	}
}

func TestPPh21ZeroForLowIncome(t *testing.T) {
	out := CalcIDStatutory(IDInput{ContributionWagesMinor: 4000000, TaxableMonthlyMinor: 4000000, JKKRiskClass: 2, PTKPStatus: "TK/0"})
	if out.PPh21 != 0 {
		t.Errorf("low income PPh21 = %d, want 0", out.PPh21)
	}
}
