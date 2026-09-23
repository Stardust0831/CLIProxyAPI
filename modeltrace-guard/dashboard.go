package main

// Dashboard resource page: server-rendered HTML with probe verdicts and the
// routing overview. Credential identifiers are masked; no secrets are ever
// included because resource requests are not management-authenticated.

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"
)

// timeNowUTC is a small indirection for readable status code generation.
func timeNowUTC() time.Time { return time.Now().UTC() }

// renderDashboard builds the browser-navigable dashboard HTML.
func renderDashboard() []byte {
	ensureRings()
	var builder strings.Builder
	builder.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">`)
	builder.WriteString(`<meta http-equiv="refresh" content="30">`)
	builder.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	builder.WriteString(`<title>ModelTrace Guard</title><style>`)
	builder.WriteString(`body{font-family:ui-sans-serif,system-ui,-apple-system,sans-serif;margin:24px;background:#0f1117;color:#e6e6e6}`)
	builder.WriteString(`h1{font-size:20px;margin:0 0 4px}h2{font-size:15px;margin:24px 0 8px;color:#9fb4c7}`)
	builder.WriteString(`.muted{color:#8b93a1;font-size:13px}`)
	builder.WriteString(`table{border-collapse:collapse;width:100%;font-size:13px;margin-top:6px}`)
	builder.WriteString(`th,td{border:1px solid #262b36;padding:6px 10px;text-align:left;vertical-align:top}`)
	builder.WriteString(`th{background:#171b24;color:#9fb4c7;font-weight:600}`)
	builder.WriteString(`.badge{display:inline-block;padding:1px 8px;border-radius:10px;font-size:12px}`)
	builder.WriteString(`.match{background:#12351f;color:#7ee2a0}.mismatch{background:#3a1414;color:#ff8f8f}`)
	builder.WriteString(`.insufficient{background:#33290f;color:#ffd479}.unlabeled{background:#1c2333;color:#9fb4c7}`)
	builder.WriteString(`.error{background:#3a1414;color:#ff8f8f}`)
	builder.WriteString(`.prob{font-variant-numeric:tabular-nums}`)
	builder.WriteString(`</style></head><body>`)
	builder.WriteString(`<h1>ModelTrace Guard</h1>`)
	builder.WriteString(`<div class="muted">CLIProxyAPI fingerprint guard &middot; generated ` + html.EscapeString(timeNowUTC().Format(time.RFC3339)) + ` &middot; auto-refresh 30s</div>`)

	cfg := activeConfig()
	b, bankHash, errLoad := loadBank(resolveBankPath(cfg.BankPath))
	builder.WriteString(`<h2>Bank</h2>`)
	if errLoad == nil {
		builder.WriteString(fmt.Sprintf(`<div class="muted">%d models &middot; built %s &middot; hash <code>%s</code> &middot; interval %d min &middot; %d probes/run</div>`,
			len(b.Models), html.EscapeString(b.BuiltAt), html.EscapeString(bankHash), cfg.IntervalMinutes, cfg.ProbesPerRun))
	} else {
		builder.WriteString(`<div class="badge error">bank not loaded</div> <span class="muted">` + html.EscapeString(errLoad.Error()) + `</span>`)
	}

	builder.WriteString(`<h2>Recent probe verdicts</h2>`)
	records := probeHistory.snapshot(20)
	if len(records) == 0 {
		builder.WriteString(`<div class="muted">No probe runs yet. Wait for the scheduler or POST /v0/management/` + pluginID + `/probe.</div>`)
	} else {
		builder.WriteString(`<table><tr><th>time</th><th>provider</th><th>account</th><th>expected</th><th>detected</th><th>verdict</th><th>outputs</th><th>top candidates</th></tr>`)
		for i := len(records) - 1; i >= 0; i-- {
			var record probeRecord
			if errUnmarshal := json.Unmarshal(records[i], &record); errUnmarshal != nil {
				continue
			}
			verdictClass := html.EscapeString(record.Verdict)
			detected := record.DetectedName
			if detected == "" {
				detected = record.Detected
			}
			detectedLabel := html.EscapeString(detected)
			if record.Probability > 0 {
				detectedLabel += fmt.Sprintf(` <span class="prob">%.1f%%</span>`, record.Probability*100)
			}
			candidates := ""
			for _, candidate := range record.TopCandidates {
				if candidates != "" {
					candidates += "<br>"
				}
				candidates += fmt.Sprintf(`%s <span class="prob">%.1f%%</span>`, html.EscapeString(candidate.DisplayName), candidate.Probability*100)
			}
			builder.WriteString(`<tr>`)
			builder.WriteString(`<td>` + html.EscapeString(record.Time.Format("01-02 15:04")) + `</td>`)
			builder.WriteString(`<td>` + html.EscapeString(record.Provider) + `</td>`)
			builder.WriteString(`<td>` + html.EscapeString(record.AuthIDMasked) + `</td>`)
			builder.WriteString(`<td>` + html.EscapeString(record.ExpectedModel) + `</td>`)
			builder.WriteString(`<td>` + detectedLabel + `</td>`)
			builder.WriteString(`<td><span class="badge ` + verdictClass + `">` + verdictClass + `</span></td>`)
			builder.WriteString(`<td class="prob">` + fmt.Sprintf("%d/%d", record.UsedOutputs, len(record.Probes)) + `</td>`)
			builder.WriteString(`<td>` + candidates + `</td>`)
			builder.WriteString(`</tr>`)
		}
		builder.WriteString(`</table>`)
	}

	builder.WriteString(`<h2>Routing overview</h2>`)
	builder.WriteString(routingSummaryHTML())

	builder.WriteString(`<h2>Recent requests</h2>`)
	routingRecords := routingHistory.snapshot(15)
	if len(routingRecords) == 0 {
		builder.WriteString(`<div class="muted">No routed requests observed yet. The usage hook records each completed request.</div>`)
	} else {
		builder.WriteString(`<table><tr><th>time</th><th>provider</th><th>model</th><th>response model</th><th>account</th><th>latency</th><th>tokens</th><th>status</th></tr>`)
		for i := len(routingRecords) - 1; i >= 0; i-- {
			var record routingRecord
			if errUnmarshal := json.Unmarshal(routingRecords[i], &record); errUnmarshal != nil {
				continue
			}
			latency := fmt.Sprintf("%d ms", record.LatencyMs)
			if record.TTFTMs > 0 {
				latency += fmt.Sprintf(" (ttft %d ms)", record.TTFTMs)
			}
			status := `<span class="badge match">ok</span>`
			if record.Failed {
				status = fmt.Sprintf(`<span class="badge mismatch">fail %d</span>`, record.FailureCode)
			}
			builder.WriteString(`<tr>`)
			builder.WriteString(`<td>` + html.EscapeString(record.Time.Format("01-02 15:04:05")) + `</td>`)
			builder.WriteString(`<td>` + html.EscapeString(record.Provider) + `</td>`)
			builder.WriteString(`<td>` + html.EscapeString(record.Model) + `</td>`)
			builder.WriteString(`<td>` + html.EscapeString(record.ResponseModel) + `</td>`)
			builder.WriteString(`<td>` + html.EscapeString(record.AuthIDMasked) + `</td>`)
			builder.WriteString(`<td>` + latency + `</td>`)
			builder.WriteString(`<td class="prob">` + fmt.Sprintf("%d/%d", record.InputTokens, record.OutputTokens) + `</td>`)
			builder.WriteString(`<td>` + status + `</td>`)
			builder.WriteString(`</tr>`)
		}
		builder.WriteString(`</table>`)
	}

	builder.WriteString(`<h2>Full data</h2>`)
	builder.WriteString(`<div class="muted">This page is an unauthenticated resource and masks account identifiers. `)
	builder.WriteString(`Use the authenticated management API (<code>GET /v0/management/` + pluginID + `/history</code>, `)
	builder.WriteString(`<code>/routing</code>, <code>/status</code>) for complete records.</div>`)
	builder.WriteString(`</body></html>`)
	return []byte(builder.String())
}

// routingSummaryHTML aggregates recent routing records per model and account.
func routingSummaryHTML() string {
	ensureRings()
	records := routingHistory.snapshot(0)
	if len(records) == 0 {
		return `<div class="muted">No routing data yet.</div>`
	}
	type tally struct {
		count     int64
		failed    int64
		tokens    int64
		latencyMs int64
	}
	perModel := map[string]*tally{}
	perAccount := map[string]*tally{}
	for _, raw := range records {
		var record routingRecord
		if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
			continue
		}
		key := record.Provider + " / " + record.Model
		modelTally, okModel := perModel[key]
		if !okModel {
			modelTally = &tally{}
			perModel[key] = modelTally
		}
		modelTally.count++
		modelTally.latencyMs += record.LatencyMs
		modelTally.tokens += record.TotalTokens
		if record.Failed {
			modelTally.failed++
		}
		accountTally, okAccount := perAccount[record.Provider+" / "+record.AuthIDMasked]
		if !okAccount {
			accountTally = &tally{}
			perAccount[record.Provider+" / "+record.AuthIDMasked] = accountTally
		}
		accountTally.count++
		if record.Failed {
			accountTally.failed++
		}
	}
	var builder strings.Builder
	builder.WriteString(`<table><tr><th>provider / model</th><th>requests</th><th>failed</th><th>avg latency</th><th>total tokens</th></tr>`)
	for key, t := range perModel {
		avg := int64(0)
		if t.count > 0 {
			avg = t.latencyMs / t.count
		}
		builder.WriteString(fmt.Sprintf(`<tr><td>%s</td><td class="prob">%d</td><td class="prob">%d</td><td class="prob">%d ms</td><td class="prob">%d</td></tr>`,
			html.EscapeString(key), t.count, t.failed, avg, t.tokens))
	}
	builder.WriteString(`</table>`)
	builder.WriteString(`<table style="margin-top:10px"><tr><th>provider / account</th><th>requests</th><th>failed</th></tr>`)
	for key, t := range perAccount {
		builder.WriteString(fmt.Sprintf(`<tr><td>%s</td><td class="prob">%d</td><td class="prob">%d</td></tr>`,
			html.EscapeString(key), t.count, t.failed))
	}
	builder.WriteString(`</table>`)
	return builder.String()
}

// dashboardResponse renders the resource route payload.
func dashboardResponse() managementResponse {
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{resourceContentType}},
		Body:       renderDashboard(),
	}
}
