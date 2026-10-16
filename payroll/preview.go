package payroll

import "math"

// Preview is the first phase of the two-phase preview → submit gate
// (REA §5.6): a dry-run diff of the next payroll run against the previous
// one, with anomaly flags. The store layer must require a preview before
// submit_payroll.

// Anomaly codes for EmployeePreview.
const (
	AnomalyNetSwingGT20 = "NET_SWING_GT_20"
	AnomalyNewEmployee  = "NEW_EMPLOYEE"
	AnomalyMissingBank  = "MISSING_BANK_DETAILS"
	AnomalyNoPriorRun   = "NO_PRIOR_RUN"
)

// netSwingPct is the |diff| / prev threshold that flags NET_SWING_GT_20.
const netSwingPct = 20.0

// Anomaly is one flagged issue on an employee's preview line.
type Anomaly struct {
	EmployeeID string `json:"employee_id"`
	Code       string `json:"code"` // e.g. NET_SWING_GT_20
	Message    string `json:"message"`
}

// EmployeePreview is the per-employee diff between two runs.
type EmployeePreview struct {
	EmployeeID   string    `json:"employee_id"`
	PrevNetMinor int64     `json:"prev_net_minor"`
	NewNetMinor  int64     `json:"new_net_minor"`
	DiffMinor    int64     `json:"diff_minor"`
	DiffPct      float64   `json:"diff_pct"`
	Anomalies    []Anomaly `json:"anomalies"`
}

// DiffRunPreview diffs the next (draft) run against the previous run.
//
//   - prev may be nil (no prior run): every employee gets NO_PRIOR_RUN.
//   - An employee in next but not in prev gets NEW_EMPLOYEE.
//   - |diff| / prevNet > 20% (or prevNet == 0 with a non-zero new net) gets
//     NET_SWING_GT_20.
//   - A next-run result carrying the missing-bank warning gets
//     MISSING_BANK_DETAILS.
//
// Employees present in prev but absent from next (departed) are not reported;
// the caller can diff the ID sets if it needs that.
func DiffRunPreview(prev, next []PayResult) []EmployeePreview {
	prevByID := make(map[string]PayResult, len(prev))
	for _, r := range prev {
		prevByID[r.EmployeeID] = r
	}

	out := make([]EmployeePreview, 0, len(next))
	for _, n := range next {
		ep := EmployeePreview{EmployeeID: n.EmployeeID, NewNetMinor: n.NetMinor}
		p, ok := prevByID[n.EmployeeID]
		if !ok {
			ep.Anomalies = append(ep.Anomalies, Anomaly{
				EmployeeID: n.EmployeeID,
				Code:       AnomalyNewEmployee,
				Message:    "employee was not in the previous run",
			})
			if len(prev) == 0 {
				ep.Anomalies = append(ep.Anomalies, Anomaly{
					EmployeeID: n.EmployeeID,
					Code:       AnomalyNoPriorRun,
					Message:    "no previous run to compare against",
				})
			}
		} else {
			ep.PrevNetMinor = p.NetMinor
			ep.DiffMinor = n.NetMinor - p.NetMinor
			if p.NetMinor == 0 {
				if n.NetMinor != 0 {
					ep.Anomalies = append(ep.Anomalies, Anomaly{
						EmployeeID: n.EmployeeID,
						Code:       AnomalyNetSwingGT20,
						Message:    "previous net was zero and new net is non-zero",
					})
				}
			} else {
				ep.DiffPct = float64(ep.DiffMinor) / float64(p.NetMinor) * 100
				if math.Abs(ep.DiffPct) > netSwingPct {
					ep.Anomalies = append(ep.Anomalies, Anomaly{
						EmployeeID: n.EmployeeID,
						Code:       AnomalyNetSwingGT20,
						Message:    "net pay swung more than 20% vs previous run",
					})
				}
			}
		}
		for _, w := range n.Warnings {
			if w == WarnMissingBankDetails {
				ep.Anomalies = append(ep.Anomalies, Anomaly{
					EmployeeID: n.EmployeeID,
					Code:       AnomalyMissingBank,
					Message:    "bank account details are missing; bank file will skip this employee",
				})
				break
			}
		}
		out = append(out, ep)
	}
	return out
}
