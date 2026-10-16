package ess

import (
	"html/template"
	"net/http"
)

// templates holds the parsed page templates. Everything is Go
// html/template with inline CSS — no JS, no frameworks.
type templates struct {
	t *template.Template
}

func newTemplates() *templates {
	t := template.Must(template.New("ess").Parse(pageTemplates))
	return &templates{t: t}
}

func (t *templates) render(w http.ResponseWriter, status int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = t.t.ExecuteTemplate(w, "layout", data)
}

// pageData is the common envelope every page renders.
type pageData struct {
	Title  string
	Authed bool
	Page   string
	Data   map[string]any
}

const pageTemplates = `{{define "layout"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} — ESS</title>
<style>
  :root { --ink:#1a1a1a; --muted:#666; --line:#e2e2e2; --accent:#1d4ed8; --bg:#fafafa; }
  * { box-sizing: border-box; }
  body { font-family: -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; color: var(--ink); background: var(--bg); margin: 0; }
  .wrap { max-width: 860px; margin: 0 auto; padding: 24px 16px 64px; }
  header.top { display: flex; justify-content: space-between; align-items: center; padding: 12px 0; border-bottom: 1px solid var(--line); margin-bottom: 20px; }
  header.top .brand { font-weight: 700; }
  nav a { margin-right: 14px; color: var(--accent); text-decoration: none; font-size: 14px; }
  nav a:hover { text-decoration: underline; }
  h1 { font-size: 22px; margin: 0 0 12px; }
  h2 { font-size: 16px; margin: 24px 0 8px; }
  .card { background: #fff; border: 1px solid var(--line); border-radius: 8px; padding: 16px; margin: 12px 0; }
  .muted { color: var(--muted); font-size: 13px; }
  table { width: 100%; border-collapse: collapse; font-size: 14px; }
  th, td { text-align: left; padding: 8px 6px; border-bottom: 1px solid var(--line); }
  th { font-size: 11px; text-transform: uppercase; letter-spacing: .05em; color: var(--muted); }
  td.num, th.num { text-align: right; font-variant-numeric: tabular-nums; }
  .pill { display: inline-block; padding: 2px 10px; border-radius: 999px; font-size: 12px; background: #eef2ff; color: #3730a3; }
  .pill.ok { background: #ecfdf5; color: #065f46; }
  .pill.warn { background: #fffbeb; color: #92400e; }
  label { display: block; font-size: 13px; margin: 10px 0 4px; }
  input[type=text], input[type=email], input[type=date], select { width: 100%; max-width: 340px; padding: 8px 10px; font-size: 14px; border: 1px solid var(--line); border-radius: 6px; }
  button, .btn { display: inline-block; margin-top: 12px; padding: 9px 18px; font-size: 14px; background: var(--accent); color: #fff; border: 0; border-radius: 6px; cursor: pointer; text-decoration: none; }
  button:hover, .btn:hover { background: #1e40af; }
  .linkbox { word-break: break-all; background: #f4f4f5; border: 1px dashed var(--line); border-radius: 6px; padding: 12px; font-size: 13px; }
  .err { background: #fef2f2; border: 1px solid #fecaca; color: #991b1b; border-radius: 8px; padding: 14px 16px; }
  .row { display: flex; gap: 12px; flex-wrap: wrap; }
  .row > div { flex: 1 1 200px; }
  @media print { header.top nav, .noprint { display: none; } body { background: #fff; } }
</style>
</head>
<body>
<div class="wrap">
<header class="top">
  <div class="brand">ESS Portal</div>
  <nav>
    {{if .Authed}}
    <a href="/ess/home">Home</a><a href="/ess/timesheets">Timesheets</a><a href="/ess/leave">Leave</a><a href="/ess/payslips">Payslips</a>
    {{end}}
  </nav>
</header>
{{template "body" .}}
</div>
</body>
</html>{{end}}

{{define "body"}}{{if eq .Page "login"}}
<h1>Sign in</h1>
<p class="muted">Enter your work email and we'll send you a one-time sign-in link (valid 15 minutes).</p>
<form method="post" action="/ess/login">
  <label for="email">Work email</label>
  <input type="email" id="email" name="email" required autocomplete="email" placeholder="you@company.com">
  <br><button type="submit">Send sign-in link</button>
</form>
{{else if eq .Page "login_sent"}}
<h1>Check your email</h1>
<p>A sign-in link is on its way to <strong>{{.Data.Email}}</strong>. It expires in 15 minutes and can only be used once.</p>
{{if .Data.DevLink}}<h2>Dev mode — no mailer configured</h2>
<p class="muted">In production this link is emailed. Here it is directly:</p>
<p class="linkbox"><a href="{{.Data.DevLink}}">{{.Data.DevLink}}</a></p>{{end}}
<p class="noprint"><a href="/ess/">← Back to sign in</a></p>
{{else if eq .Page "home"}}
<h1>Hi, {{.Data.EmployeeName}}</h1>
<p class="muted">{{.Data.OrgName}} · {{.Data.EmployeeNumber}}</p>
<div class="card">
  <h2 style="margin-top:0">Today</h2>
  {{if .Data.ClockedIn}}<span class="pill ok">Clocked in</span> <span class="muted">since {{.Data.ClockSince}}</span>
  {{if .Data.OnBreak}} <span class="pill warn">On break</span>{{end}}
  {{else}}<span class="pill">Not clocked in</span>{{end}}
</div>
<h2>Leave balances</h2>
{{if .Data.Balances}}<table><tr><th>Policy</th><th>Period</th><th class="num">Balance (days)</th></tr>
{{range .Data.Balances}}<tr><td>{{.PolicyName}}</td><td>{{.Period}}</td><td class="num">{{.Balance}}</td></tr>{{end}}</table>
{{else}}<p class="muted">No leave balances yet.</p>{{end}}
<form method="post" action="/ess/logout" class="noprint"><button type="submit">Sign out</button></form>
{{else if eq .Page "timesheets"}}
<h1>Timesheets</h1>
<p class="muted">Your recent punches (last 30 days).</p>
{{if .Data.Entries}}<table><tr><th>When</th><th>Event</th></tr>
{{range .Data.Entries}}<tr><td>{{.When}}</td><td>{{.Kind}}</td></tr>{{end}}</table>
{{else}}<p class="muted">No entries in the last 30 days.</p>{{end}}
{{else if eq .Page "leave"}}
<h1>Leave</h1>
<h2>Balances</h2>
{{if .Data.Balances}}<table><tr><th>Policy</th><th>Period</th><th class="num">Accrued</th><th class="num">Taken</th><th class="num">Balance</th></tr>
{{range .Data.Balances}}<tr><td>{{.PolicyName}}</td><td>{{.Period}}</td><td class="num">{{.Accrued}}</td><td class="num">{{.Taken}}</td><td class="num"><strong>{{.Balance}}</strong></td></tr>{{end}}</table>
{{else}}<p class="muted">No leave balances yet.</p>{{end}}
<h2>Request leave</h2>
{{if .Data.Policies}}<form method="post" action="/ess/leave/requests">
  <label for="policy">Policy</label>
  <select id="policy" name="policy_id">{{range .Data.Policies}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select>
  <div class="row"><div><label for="start">Start date</label><input type="date" id="start" name="start_date" required></div>
  <div><label for="end">End date</label><input type="date" id="end" name="end_date" required></div></div>
  <button type="submit">Submit request</button>
</form>{{else}}<p class="muted">No leave policies configured.</p>{{end}}
<h2>My requests</h2>
{{if .Data.Requests}}<table><tr><th>Period</th><th>Days</th><th>Status</th></tr>
{{range .Data.Requests}}<tr><td>{{.Start}} → {{.End}}</td><td class="num">{{.Days}}</td><td><span class="pill">{{.Status}}</span></td></tr>{{end}}</table>
{{else}}<p class="muted">No leave requests yet.</p>{{end}}
{{else if eq .Page "payslips"}}
<h1>Payslips</h1>
{{if .Data.Runs}}<table><tr><th>Period</th><th>Status</th><th class="num">Net pay</th><th></th></tr>
{{range .Data.Runs}}<tr><td>{{.Period}}</td><td><span class="pill">{{.Status}}</span></td><td class="num">{{.Net}}</td><td><a href="/ess/payslips/{{.ID}}">View</a></td></tr>{{end}}</table>
{{else}}<p class="muted">No payslips available yet.</p>{{end}}
{{else if eq .Page "error"}}
<h1>{{.Data.Title}}</h1>
<div class="err">{{.Data.Message}}</div>
<p class="noprint"><a href="{{.Data.Back}}">← Back</a></p>
{{end}}{{end}}`
