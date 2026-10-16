package payroll

import "testing"

func mkResult(id string, net int64, warnings ...string) PayResult {
	return PayResult{EmployeeID: id, NetMinor: net, Warnings: warnings}
}

func anomaliesOf(ep EmployeePreview) map[string]bool {
	m := map[string]bool{}
	for _, a := range ep.Anomalies {
		m[a.Code] = true
	}
	return m
}

func TestDiffRunPreviewNilPrev(t *testing.T) {
	next := []PayResult{mkResult("E1", 100000), mkResult("E2", 200000)}
	out := DiffRunPreview(nil, next)
	if len(out) != 2 {
		t.Fatalf("got %d previews, want 2", len(out))
	}
	for _, ep := range out {
		an := anomaliesOf(ep)
		if !an[AnomalyNoPriorRun] {
			t.Errorf("%s: expected NO_PRIOR_RUN, got %v", ep.EmployeeID, ep.Anomalies)
		}
		if !an[AnomalyNewEmployee] {
			t.Errorf("%s: expected NEW_EMPLOYEE, got %v", ep.EmployeeID, ep.Anomalies)
		}
	}
}

func TestDiffRunPreviewSwing(t *testing.T) {
	prev := []PayResult{mkResult("E1", 100000), mkResult("E2", 100000), mkResult("E3", 100000)}
	next := []PayResult{
		mkResult("E1", 125000), // +25% -> swing
		mkResult("E2", 110000), // +10% -> ok
		mkResult("E3", 75000),  // -25% -> swing
	}
	out := DiffRunPreview(prev, next)
	byID := map[string]EmployeePreview{}
	for _, ep := range out {
		byID[ep.EmployeeID] = ep
	}

	if !anomaliesOf(byID["E1"])[AnomalyNetSwingGT20] {
		t.Error("E1 +25%: expected NET_SWING_GT_20")
	}
	if anomaliesOf(byID["E2"])[AnomalyNetSwingGT20] {
		t.Error("E2 +10%: unexpected NET_SWING_GT_20")
	}
	if !anomaliesOf(byID["E3"])[AnomalyNetSwingGT20] {
		t.Error("E3 -25%: expected NET_SWING_GT_20")
	}
	if byID["E1"].DiffMinor != 25000 || byID["E1"].DiffPct != 25.0 {
		t.Errorf("E1 diff = (%d, %f), want (25000, 25.0)", byID["E1"].DiffMinor, byID["E1"].DiffPct)
	}
}

func TestDiffRunPreviewNewEmployee(t *testing.T) {
	prev := []PayResult{mkResult("E1", 100000)}
	next := []PayResult{mkResult("E1", 100000), mkResult("E9", 50000)}
	out := DiffRunPreview(prev, next)
	byID := map[string]EmployeePreview{}
	for _, ep := range out {
		byID[ep.EmployeeID] = ep
	}
	if !anomaliesOf(byID["E9"])[AnomalyNewEmployee] {
		t.Error("E9: expected NEW_EMPLOYEE")
	}
	if anomaliesOf(byID["E9"])[AnomalyNoPriorRun] {
		t.Error("E9: should not get NO_PRIOR_RUN when a prev run exists")
	}
	if len(byID["E1"].Anomalies) != 0 {
		t.Errorf("E1: expected no anomalies, got %v", byID["E1"].Anomalies)
	}
}

func TestDiffRunPreviewMissingBank(t *testing.T) {
	prev := []PayResult{mkResult("E1", 100000)}
	next := []PayResult{mkResult("E1", 100000, WarnMissingBankDetails)}
	out := DiffRunPreview(prev, next)
	if !anomaliesOf(out[0])[AnomalyMissingBank] {
		t.Errorf("expected MISSING_BANK_DETAILS, got %v", out[0].Anomalies)
	}
}

func TestDiffRunPreviewZeroPrev(t *testing.T) {
	prev := []PayResult{mkResult("E1", 0)}
	next := []PayResult{mkResult("E1", 50000)}
	out := DiffRunPreview(prev, next)
	if !anomaliesOf(out[0])[AnomalyNetSwingGT20] {
		t.Errorf("zero prev -> non-zero: expected NET_SWING_GT_20, got %v", out[0].Anomalies)
	}
}
