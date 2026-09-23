package main

// Management API: authenticated /v0/management/... routes plus an
// unauthenticated browser-navigable dashboard resource. The resource page
// masks credential identifiers and never exposes secrets.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	dashboardResourcePath = "/dashboard"
	jsonContentType       = "application/json; charset=utf-8"
	resourceContentType   = "text/html; charset=utf-8"
)

type managementRegistrationPayload struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

func managementRegistration() managementRegistrationPayload {
	return managementRegistrationPayload{
		Routes: []pluginapi.ManagementRoute{
			{Method: "GET", Path: pluginID + "/status", Description: "ModelTrace Guard status summary."},
			{Method: "GET", Path: pluginID + "/history", Description: "Recent probe records."},
			{Method: "GET", Path: pluginID + "/routing", Description: "Recent per-request routing records."},
			{Method: "GET", Path: pluginID + "/config", Description: "Current plugin runtime config."},
			{Method: "PUT", Path: pluginID + "/config", Description: "Update plugin runtime config fields."},
			{Method: "POST", Path: pluginID + "/probe", Description: "Trigger a probe run now."},
			{Method: "GET", Path: pluginID + "/bank", Description: "Loaded fingerprint bank summary."},
		},
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        dashboardResourcePath,
				Menu:        "ModelTrace Guard",
				Description: "Fingerprint probe verdicts and routing overview as a browser-navigable resource.",
			},
		},
	}
}

func handleManagement(request []byte) ([]byte, error) {
	req, errDecode := buildManagementRequest(request)
	if errDecode != nil {
		return okEnvelope(managementResponse{
			StatusCode: http.StatusBadRequest,
			Headers:    http.Header{"Content-Type": []string{jsonContentType}},
			Body:       []byte(`{"error":"invalid management request"}`),
		})
	}
	response := dispatchManagement(req)
	raw, errMarshal := json.Marshal(response)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func dispatchManagement(req managementRequest) managementResponse {
	// Lazily initialize the record rings so status/history/routing stay safe
	// before any usage or probe record exists.
	ensureRings()
	path := normalizeManagementPath(req.Path)
	method := strings.ToUpper(req.Method)
	switch path {
	case strings.TrimPrefix(dashboardResourcePath, "/"):
		if method != "GET" {
			return errorResponse(http.StatusMethodNotAllowed, "use GET to open the dashboard")
		}
		return dashboardResponse()
	case "status":
		return jsonResponse(http.StatusOK, statusJSON())
	case "history":
		return jsonResponse(http.StatusOK, historyJSON(queryInt(req.Query, "limit")))
	case "routing":
		return jsonResponse(http.StatusOK, routingJSON(queryInt(req.Query, "limit")))
	case "config":
		if method == "PUT" || method == "POST" {
			return updateConfigResponse(req.Body)
		}
		return jsonResponse(http.StatusOK, configJSON())
	case "probe":
		if method != "POST" {
			return errorResponse(http.StatusMethodNotAllowed, "use POST to trigger a probe run")
		}
		requestManualProbe()
		return jsonResponse(http.StatusAccepted, jsonRaw(`{"accepted":true,"message":"probe run scheduled"}`))
	case "bank":
		return jsonResponse(http.StatusOK, bankJSON())
	default:
		return errorResponse(http.StatusNotFound, "unknown modeltrace-guard route: "+path)
	}
}

func normalizeManagementPath(path string) string {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimPrefix(path, "v0/management/")
	// Resource requests arrive under /v0/resource/plugins/<pluginID>/.
	path = strings.TrimPrefix(path, "v0/resource/plugins/"+pluginID+"/")
	path = strings.TrimPrefix(path, pluginID+"/")
	return strings.Trim(path, "/")
}

func jsonResponse(status int, body []byte) managementResponse {
	return managementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{jsonContentType}},
		Body:       body,
	}
}

func errorResponse(status int, message string) managementResponse {
	raw, _ := json.Marshal(map[string]string{"error": message})
	return jsonResponse(status, raw)
}

func queryInt(query map[string][]string, key string) int {
	if query == nil {
		return 0
	}
	values, ok := query[key]
	if !ok || len(values) == 0 {
		return 0
	}
	value, errConvert := strconv.Atoi(strings.TrimSpace(values[0]))
	if errConvert != nil {
		return 0
	}
	return value
}

func statusJSON() json.RawMessage {
	cfg := activeConfig()
	b, _, errLoad := loadBank(resolveBankPath(cfg.BankPath))
	bankSummary := map[string]any{
		"path":   cfg.BankPath,
		"loaded": errLoad == nil,
		"models": 0,
	}
	probeState := prober.get()
	inProgress := false
	nextRun := ""
	if probeState != nil {
		inProgress = probeState.inProgress
		nextRun = timeNowUTC().Add(time.Duration(cfg.IntervalMinutes) * time.Minute).Format(time.RFC3339)
	}
	if errLoad == nil {
		bankSummary["models"] = len(b.Models)
		bankSummary["built_at"] = b.BuiltAt
		bankSummary["method"] = b.methodName()
	}
	payload := map[string]any{
		"plugin": map[string]any{
			"id":      pluginID,
			"version": pluginVersion,
		},
		"config": map[string]any{
			"bank_path":        cfg.BankPath,
			"interval_minutes": cfg.IntervalMinutes,
			"probes_per_run":   cfg.ProbesPerRun,
			"providers":        cfg.Providers,
			"auth_ids":         cfg.AuthIDs,
			"environments":     cfg.Environments,
			"entry_protocol":   cfg.EntryProtocol,
			"exit_protocol":    cfg.ExitProtocol,
			"history_path":     cfg.HistoryPath,
		},
		"bank": bankSummary,
		"probe": map[string]any{
			"in_progress": inProgress,
			"next_run":    nextRun,
		},
		"usage": map[string]any{
			"probe_records":   probeHistory.size(),
			"routing_records": routingHistory.size(),
		},
		"rotation": map[string]any{
			"enabled":        cfg.RotationEnabled,
			"window_minutes": cfg.RotationWindowMinutes,
			"providers":      cfg.RotationProviders,
			"active_windows": rotationStatus(),
		},
	}
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return jsonRaw(`{"error":"status marshal failed"}`)
	}
	return raw
}

func historyJSON(limit int) json.RawMessage {
	ensureRings()
	raw, errMarshal := json.Marshal(map[string]any{"records": probeHistory.snapshot(limit)})
	if errMarshal != nil {
		return jsonRaw(`{"error":"history marshal failed"}`)
	}
	return raw
}

func routingJSON(limit int) json.RawMessage {
	ensureRings()
	raw, errMarshal := json.Marshal(map[string]any{"records": routingHistory.snapshot(limit)})
	if errMarshal != nil {
		return jsonRaw(`{"error":"routing marshal failed"}`)
	}
	return raw
}

func configJSON() json.RawMessage {
	cfg := activeConfig()
	raw, errMarshal := json.Marshal(map[string]any{
		"bank_path":               cfg.BankPath,
		"interval_minutes":        cfg.IntervalMinutes,
		"probes_per_run":          cfg.ProbesPerRun,
		"providers":               cfg.Providers,
		"auth_ids":                cfg.AuthIDs,
		"models":                  cfg.Models,
		"environments":            cfg.Environments,
		"history_path":            cfg.HistoryPath,
		"history_size":            cfg.HistorySize,
		"routing_size":            cfg.RoutingSize,
		"entry_protocol":          cfg.EntryProtocol,
		"exit_protocol":           cfg.ExitProtocol,
		"rotation_enabled":        cfg.RotationEnabled,
		"rotation_window_minutes": cfg.RotationWindowMinutes,
		"rotation_providers":      cfg.RotationProviders,
	})
	if errMarshal != nil {
		return jsonRaw(`{"error":"config marshal failed"}`)
	}
	return raw
}

func updateConfigResponse(body []byte) managementResponse {
	values := map[string]any{}
	if len(body) == 0 || errUnmarshalConfig(body, &values) != nil {
		return errorResponse(http.StatusBadRequest, "config update expects a JSON object")
	}
	applyConfigValues(values)
	return jsonResponse(http.StatusOK, configJSON())
}

func errUnmarshalConfig(body []byte, target *map[string]any) error {
	return json.Unmarshal(body, target)
}

func bankJSON() json.RawMessage {
	cfg := activeConfig()
	b, hash, errLoad := loadBank(resolveBankPath(cfg.BankPath))
	if errLoad != nil {
		raw, _ := json.Marshal(map[string]any{"loaded": false, "error": errLoad.Error()})
		return raw
	}
	models := make([]map[string]any, 0, len(b.Models))
	for _, model := range b.Models {
		models = append(models, map[string]any{
			"id":           model.ID,
			"display_name": model.DisplayName,
			"family":       model.Family,
		})
	}
	raw, errMarshal := json.Marshal(map[string]any{
		"loaded":                true,
		"hash":                  hash,
		"built_at":              b.BuiltAt,
		"method":                b.methodName(),
		"recommended_queries":   b.RecommendedQueries,
		"minimum_valid_numbers": b.MinimumValidNumbers,
		"models":                models,
		"model_order":           b.Robust.ModelOrder,
		"calibration":           b.Calibration,
	})
	if errMarshal != nil {
		return jsonRaw(`{"error":"bank marshal failed"}`)
	}
	return raw
}
