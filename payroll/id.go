package payroll

import (
	"fmt"
	"strings"
)

// Indonesia statutory rule pack.
//
// Sources (all public, checked 2026-10-16):
//   - BPJS Kesehatan PPU: 5% (4% employer + 1% employee), max wage
//     Rp12,000,000/month — Perpres 64/2020; confirmed unchanged through
//     Apr 2026 by multiple outlets.
//   - BPJS Ketenagakerjaan: JHT 5.7% (3.7 + 2.0), JKK by risk class
//     I–V {0.24%, 0.54%, 0.89%, 1.27%, 1.74%} employer-only, JKM 0.3%
//     employer-only, JP 3% (2 + 1) — bpjsketenagakerjaan.go.id
//     ("Berapa Besaran Iuran JHT, JKK, JKM, JP dan JKP", per PP 6/2025).
//     VERIFY: PP 49/2023 recomposed 0.14% of JKK into JKP for JKP-registered
//     employers (rates 0.10/0.40/0.75/1.13/1.60); the official BPJS TK site
//     currently lists the full rates again — confirm which applies before
//     production use.
//   - JP wage cap 2026: Rp11,086,300/month, effective Mar 2026
//     (SE BPJS TK No. B/3307/022026; adjusts yearly per PP 45/2015).
//   - PPh 21 TER: monthly effective-average-rate tables, categories A/B/C by
//     PTKP status — Lampiran PP 58/2023 (implemented by PMK 168/2023),
//     effective 2024-01-01. Transcribed from a public copy of the PP 58/2023
//     appendix, cross-checked against online-pajak.com's TER tables and DJP's
//     PMK 168/2023 briefing slides.
//
// All money in whole rupiah (int64).

// IDInput is the input to CalcIDStatutory.
type IDInput struct {
	ContributionWagesMinor int64  // BPJS wage base for the month (rupiah)
	TaxableMonthlyMinor    int64  // PPh21 TER base: monthly gross (rupiah)
	JKKRiskClass           int    // 1..5 (Calculate validates; CalcIDStatutory treats out-of-range as 0 JKK)
	PTKPStatus             string // e.g. "TK/0"; unknown → category A (caller should warn)
}

// IDOutput is the result of CalcIDStatutory. All amounts in rupiah.
type IDOutput struct {
	BPJSKesEmployer int64
	BPJSKesEmployee int64
	JHTEmployer     int64
	JHTEmployee     int64
	JKKEmployer     int64
	JKMEmployer     int64
	JPEmployer      int64
	JPEmployee      int64
	PPh21           int64
}

// CalcIDStatutory computes Indonesian statutory contributions for one employee.
func CalcIDStatutory(in IDInput) IDOutput {
	var out IDOutput

	wages := in.ContributionWagesMinor
	if wages < 0 {
		wages = 0
	}

	// --- BPJS Kesehatan (PPU): 5% = 4% employer + 1% employee, cap Rp12jt ---
	kesBase := wages
	if kesBase > 12000000 {
		kesBase = 12000000
	}
	out.BPJSKesEmployer = mulBps(kesBase, 400)
	out.BPJSKesEmployee = mulBps(kesBase, 100)

	// --- JHT: 5.7% = 3.7% employer + 2.0% employee, no cap ---
	out.JHTEmployer = mulBps(wages, 370)
	out.JHTEmployee = mulBps(wages, 200)

	// --- JKK: employer only, by risk class, no cap ---
	if bps, ok := jkkRateBps(in.JKKRiskClass); ok {
		out.JKKEmployer = mulBps(wages, bps)
	}

	// --- JKM: 0.3% employer only, no cap ---
	out.JKMEmployer = mulBps(wages, 30)

	// --- JP: 3% = 2% employer + 1% employee, capped ---
	jpBase := wages
	if jpBase > jpWageCap2026 {
		jpBase = jpWageCap2026
	}
	out.JPEmployer = mulBps(jpBase, 200)
	out.JPEmployee = mulBps(jpBase, 100)

	// --- PPh 21: monthly TER on gross ---
	taxable := in.TaxableMonthlyMinor
	if taxable < 0 {
		taxable = 0
	}
	out.PPh21 = mulBps(taxable, terRateBps(terCategory(in.PTKPStatus), taxable))

	return out
}

// jpWageCap2026 is the 2026 JP maximum wage base.
// Source: SE BPJS Ketenagakerjaan No. B/3307/022026, effective Mar 2026
// (raised from Rp10,547,400 for 2025; adjusts yearly per PP 45/2015).
const jpWageCap2026 = 11086300

// jkkRateBps returns the JKK employer rate in basis points for risk class 1–5.
// Source: bpjsketenagakerjaan.go.id (per PP 6/2025).
func jkkRateBps(class int) (int64, bool) {
	switch class {
	case 1:
		return 24, true
	case 2:
		return 54, true
	case 3:
		return 89, true
	case 4:
		return 127, true
	case 5:
		return 174, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// PPh 21 monthly TER (Tarif Efektif Rata-rata).
// ---------------------------------------------------------------------------

// terCategory maps a PTKP status to TER category A/B/C.
// A: TK/0, TK/1, K/0 · B: TK/2, TK/3, K/1, K/2 · C: K/3.
// Unknown statuses map to A; the caller (Calculate) warns.
func terCategory(ptkpStatus string) byte {
	switch strings.ToUpper(strings.TrimSpace(ptkpStatus)) {
	case "TK/0", "TK/1", "K/0":
		return 'A'
	case "TK/2", "TK/3", "K/1", "K/2":
		return 'B'
	case "K/3":
		return 'C'
	}
	return 'A'
}

// terKnownStatus reports whether the PTKP status maps to a known category.
func terKnownStatus(ptkpStatus string) bool {
	switch strings.ToUpper(strings.TrimSpace(ptkpStatus)) {
	case "TK/0", "TK/1", "K/0", "TK/2", "TK/3", "K/1", "K/2", "K/3":
		return true
	}
	return false
}

// terBracket is one TER layer: monthly gross up to upper (inclusive) → rate.
type terBracket struct {
	upperRupiah int64
	rateBps     int64
}

const terTop = int64(1) << 62

// Source for all three tables: Lampiran PP 58/2023 (implemented by
// PMK 168/2023), effective 2024-01-01; transcribed from a public copy of the
// appendix and cross-checked against online-pajak.com and DJP briefing slides.
var terTableA = []terBracket{
	{5400000, 0}, {5650000, 25}, {5950000, 50}, {6300000, 75},
	{6750000, 100}, {7500000, 125}, {8550000, 150}, {9650000, 175},
	{10050000, 200}, {10350000, 225}, {10700000, 250}, {11050000, 300},
	{11600000, 350}, {12500000, 400}, {13750000, 500}, {15100000, 600},
	{16950000, 700}, {19750000, 800}, {24150000, 900}, {26450000, 1000},
	{28000000, 1100}, {30050000, 1200}, {32400000, 1300}, {35400000, 1400},
	{39100000, 1500}, {43850000, 1600}, {47800000, 1700}, {51400000, 1800},
	{56300000, 1900}, {62200000, 2000}, {68600000, 2100}, {77500000, 2200},
	{89000000, 2300}, {103000000, 2400}, {125000000, 2500}, {157000000, 2600},
	{206000000, 2700}, {337000000, 2800}, {454000000, 2900}, {550000000, 3000},
	{695000000, 3100}, {910000000, 3200}, {1400000000, 3300}, {terTop, 3400},
}

var terTableB = []terBracket{
	{6200000, 0}, {6500000, 25}, {6850000, 50}, {7300000, 75},
	{9200000, 100}, {10750000, 150}, {11250000, 200}, {11600000, 250},
	{12600000, 300}, {13600000, 400}, {14950000, 500}, {16400000, 600},
	{18450000, 700}, {21850000, 800}, {26000000, 900}, {27700000, 1000},
	{29350000, 1100}, {31450000, 1200}, {33950000, 1300}, {37100000, 1400},
	{41100000, 1500}, {45800000, 1600}, {49500000, 1700}, {53800000, 1800},
	{58500000, 1900}, {64000000, 2000}, {71000000, 2100}, {80000000, 2200},
	{93000000, 2300}, {109000000, 2400}, {129000000, 2500}, {163000000, 2600},
	{211000000, 2700}, {374000000, 2800}, {459000000, 2900}, {555000000, 3000},
	{704000000, 3100}, {957000000, 3200}, {1405000000, 3300}, {terTop, 3400},
}

var terTableC = []terBracket{
	{6600000, 0}, {6950000, 25}, {7350000, 50}, {7800000, 75},
	{8850000, 100}, {9800000, 125}, {10950000, 150}, {11200000, 175},
	{12050000, 200}, {12950000, 300}, {14150000, 400}, {15550000, 500},
	{17050000, 600}, {19500000, 700}, {22700000, 800}, {26600000, 900},
	{28100000, 1000}, {30100000, 1100}, {32600000, 1200}, {35400000, 1300},
	{38900000, 1400}, {43000000, 1500}, {47400000, 1600}, {51200000, 1700},
	{55800000, 1800}, {60400000, 1900}, {66700000, 2000}, {74500000, 2100},
	{83200000, 2200}, {95600000, 2300}, {110000000, 2400}, {134000000, 2500},
	{169000000, 2600}, {221000000, 2700}, {390000000, 2800}, {463000000, 2900},
	{561000000, 3000}, {709000000, 3100}, {965000000, 3200}, {1419000000, 3300},
	{terTop, 3400},
}

// terRateBps returns the TER rate in basis points for the category and
// monthly gross.
func terRateBps(category byte, grossRupiah int64) int64 {
	table := terTableA
	switch category {
	case 'B':
		table = terTableB
	case 'C':
		table = terTableC
	}
	for _, b := range table {
		if grossRupiah <= b.upperRupiah {
			return b.rateBps
		}
	}
	return 3400 // unreachable: tables end with terTop
}

// terCategoryOf is a small exported helper for the store layer / tests.
func terCategoryOf(ptkpStatus string) (byte, error) {
	if !terKnownStatus(ptkpStatus) {
		return 'A', fmt.Errorf("payroll: unknown PTKP status %q", ptkpStatus)
	}
	return terCategory(ptkpStatus), nil
}
