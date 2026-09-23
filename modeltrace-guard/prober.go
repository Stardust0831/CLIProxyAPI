package main

// Probe engine: enumerates credentials through host.auth.list, sends the
// ModelTrace long-integer challenges through host.model.execute with AuthID
// locking, scores the digit outputs against the fingerprint bank, and records
// the attribution outcome.

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// chatCompletionRequest is an OpenAI-compatible probe request body.
type chatCompletionRequest struct {
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// hostModelExecutionRequest wraps the SDK host model execution request with
// the host_callback_id recursion guard field.
type hostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type probeTarget struct {
	AuthID    string
	AuthIndex string
	Provider  string
	AuthType  string
	Model     string
}

type probeDetail struct {
	Condition     string `json:"condition"`
	ChallengeID   string `json:"challenge_id"`
	ExpectedCount int    `json:"expected_count"`
	ParsedNumbers int    `json:"parsed_numbers"`
	Minimum       int    `json:"minimum_numbers"`
	Accepted      bool   `json:"accepted"`
	StatusCode    int    `json:"status_code,omitempty"`
	ResponseModel string `json:"response_model,omitempty"`
	LatencyMs     int64  `json:"latency_ms,omitempty"`
	Error         string `json:"error,omitempty"`
}

type candidateJSON struct {
	Model       string  `json:"model"`
	DisplayName string  `json:"display_name"`
	Probability float64 `json:"probability"`
}

type probeRecord struct {
	Time           time.Time       `json:"time"`
	Trigger        string          `json:"trigger"`
	Provider       string          `json:"provider"`
	AuthID         string          `json:"auth_id"`
	AuthIDMasked   string          `json:"auth_id_masked"`
	AuthIndex      string          `json:"auth_index,omitempty"`
	AuthType       string          `json:"auth_type,omitempty"`
	ExpectedModel  string          `json:"expected_model"`
	Detected       string          `json:"detected"`
	DetectedName   string          `json:"detected_name,omitempty"`
	DetectedFamily string          `json:"detected_family,omitempty"`
	Probability    float64         `json:"probability,omitempty"`
	FamilyProb     float64         `json:"family_probability,omitempty"`
	Verdict        string          `json:"verdict"`
	UsedOutputs    int             `json:"used_outputs"`
	TopCandidates  []candidateJSON `json:"top_candidates,omitempty"`
	Probes         []probeDetail   `json:"probes"`
	Detail         json.RawMessage `json:"detail,omitempty"`
	LatencyMs      int64           `json:"latency_ms"`
	BankHash       string          `json:"bank_hash,omitempty"`
}

type proberState struct {
	mu         sync.Mutex
	stopped    bool
	stopOnce   sync.Once
	stopChan   chan struct{}
	wakeChan   chan struct{}
	inProgress bool
	runCounter int
}

var prober atomicProber

type atomicProber struct {
	mu  sync.Mutex
	ref *proberState
}

func (p *atomicProber) get() *proberState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ref
}

func (p *atomicProber) set(state *proberState) {
	p.mu.Lock()
	p.ref = state
	p.mu.Unlock()
}

// ensureProber starts the scheduled probe loop if it is not running.
func ensureProber() {
	if activeConfig().IntervalMinutes <= 0 {
		return
	}
	if prober.get() != nil {
		return
	}
	state := &proberState{
		stopChan: make(chan struct{}),
		wakeChan: make(chan struct{}, 1),
	}
	prober.set(state)
	go proberLoop(state)
}

// stopProber stops the scheduled probe loop.
func stopProber() {
	if state := prober.get(); state != nil {
		state.stopOnce.Do(func() {
			close(state.stopChan)
		})
		prober.set(nil)
	}
}

// requestManualProbe wakes the loop or runs a probe directly when no loop is
// running. Returns the state that accepted the request.
func requestManualProbe() {
	state := prober.get()
	if state == nil {
		go runProbeOnce("manual")
		return
	}
	select {
	case state.wakeChan <- struct{}{}:
	default:
	}
}

func onConfigChanged() {
	ensureRings()
	cfg := activeConfig()
	probeHistory.resize(cfg.HistorySize)
	routingHistory.resize(cfg.RoutingSize)
	if cfg.IntervalMinutes > 0 {
		ensureProber()
		return
	}
	stopProber()
}

func proberLoop(state *proberState) {
	for {
		interval := activeConfig().IntervalMinutes
		if interval <= 0 {
			return
		}
		timer := time.NewTimer(time.Duration(interval) * time.Minute)
		select {
		case <-state.stopChan:
			timer.Stop()
			return
		case <-state.wakeChan:
			timer.Stop()
			runProbeOnce("manual")
		case <-timer.C:
			runProbeOnce("schedule")
		}
	}
}

// runProbeOnce performs one full probe pass over the filtered credentials.
func runProbeOnce(trigger string) {
	state := prober.get()
	if state != nil {
		state.mu.Lock()
		if state.inProgress {
			state.mu.Unlock()
			return
		}
		state.inProgress = true
		state.mu.Unlock()
		defer func() {
			state.mu.Lock()
			state.inProgress = false
			state.mu.Unlock()
		}()
	}
	cfg := activeConfig()
	b, bankHash, errLoad := loadBank(resolveBankPath(cfg.BankPath))
	if errLoad != nil {
		appendProbeRecord(probeRecord{
			Time:    time.Now().UTC(),
			Trigger: trigger,
			Verdict: "error",
			Probes:  []probeDetail{},
			Detail:  jsonRaw(fmt.Sprintf(`{"error":%q}`, errLoad.Error())),
		})
		return
	}
	started := time.Now()
	targets, errList := enumerateTargets(cfg)
	if errList != nil {
		appendProbeRecord(probeRecord{
			Time:    time.Now().UTC(),
			Trigger: trigger,
			Verdict: "error",
			Probes:  []probeDetail{},
			Detail:  jsonRaw(fmt.Sprintf(`{"error":%q}`, errList.Error())),
		})
		return
	}
	runCounter := 0
	if state != nil {
		state.mu.Lock()
		runCounter = state.runCounter
		state.runCounter++
		state.mu.Unlock()
	}
	for _, target := range targets {
		probes := probeOneTarget(cfg, b, target, runCounter)
		verdict := classifyVerdict(target.Model, probes.result.Prediction, probes.result.UsedOutputs)
		record := probeRecord{
			Time:           time.Now().UTC(),
			Trigger:        trigger,
			Provider:       target.Provider,
			AuthID:         target.AuthID,
			AuthIDMasked:   maskID(target.AuthID),
			AuthIndex:      target.AuthIndex,
			AuthType:       target.AuthType,
			ExpectedModel:  target.Model,
			Detected:       probes.result.Prediction,
			DetectedName:   probes.result.PredictionName,
			DetectedFamily: probes.result.FamilyName,
			Probability:    probes.result.Probability,
			FamilyProb:     probes.result.FamilyProb,
			Verdict:        verdict,
			UsedOutputs:    probes.result.UsedOutputs,
			Probes:         probes.details,
			LatencyMs:      time.Since(started).Milliseconds(),
			BankHash:       bankHash,
		}
		if len(probes.result.Results) > 0 {
			top := 3
			if len(probes.result.Results) < top {
				top = len(probes.result.Results)
			}
			for i := 0; i < top; i++ {
				record.TopCandidates = append(record.TopCandidates, candidateJSON{
					Model:       probes.result.Results[i].Model,
					DisplayName: probes.result.Results[i].DisplayName,
					Probability: probes.result.Results[i].Probability,
				})
			}
		}
		raw, errMarshal := json.Marshal(probes.result)
		if errMarshal == nil {
			record.Detail = json.RawMessage(raw)
		}
		appendProbeRecord(record)
	}
}

type targetProbeResult struct {
	result  fingerprintResult
	details []probeDetail
}

// probeOneTarget sends the challenge batch to one credential and scores the
// outputs together.
func probeOneTarget(cfg pluginConfig, b *bank, target probeTarget, runCounter int) targetProbeResult {
	challenges := selectChallenges(cfg, runCounter, cfg.ProbesPerRun)
	outputs := make([][]int, 0, len(challenges))
	expectedCounts := make([]int, 0, len(challenges))
	details := make([]probeDetail, 0, len(challenges))
	for _, item := range challenges {
		detail := probeDetail{
			Condition:     item.Condition,
			ChallengeID:   item.ChallengeID,
			ExpectedCount: item.ExpectedCount,
		}
		text, statusCode, responseModel, latencyMs, errExec := executeProbe(cfg, target, item)
		detail.StatusCode = statusCode
		detail.ResponseModel = responseModel
		detail.LatencyMs = latencyMs
		if errExec != nil {
			detail.Error = errExec.Error()
			details = append(details, detail)
			continue
		}
		numbers := parseNumbers(text)
		minimum := int(math.Ceil(float64(item.ExpectedCount) * 0.55))
		if minimum < 80 {
			minimum = 80
		}
		detail.ParsedNumbers = len(numbers)
		detail.Minimum = minimum
		detail.Accepted = len(numbers) >= minimum
		if detail.Accepted {
			outputs = append(outputs, numbers)
			expectedCounts = append(expectedCounts, item.ExpectedCount)
		}
		details = append(details, detail)
	}
	result, errScore := scoreOutputs(outputs, expectedCounts, b)
	if errScore != nil {
		return targetProbeResult{result: fingerprintResult{Diagnostics: diagnosticsFrom(details)}, details: details}
	}
	result.Diagnostics = diagnosticsFrom(details)
	return targetProbeResult{result: result, details: details}
}

func diagnosticsFrom(details []probeDetail) []outputDiagnostic {
	out := make([]outputDiagnostic, 0, len(details))
	for i, detail := range details {
		out = append(out, outputDiagnostic{
			Index:          i,
			ParsedNumbers:  detail.ParsedNumbers,
			MinimumNumbers: detail.Minimum,
			Accepted:       detail.Accepted,
		})
	}
	return out
}

// enumerateTargets builds the probe targets from the credential list.
func enumerateTargets(cfg pluginConfig) ([]probeTarget, error) {
	var result struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if errCall := callHost(pluginabi.MethodHostAuthList, map[string]any{}, &result); errCall != nil {
		return nil, errCall
	}
	providerFilter := map[string]bool{}
	for _, provider := range cfg.Providers {
		providerFilter[provider] = true
	}
	authFilter := map[string]bool{}
	for _, auth := range cfg.AuthIDs {
		authFilter[auth] = true
	}
	targets := make([]probeTarget, 0, len(result.Files))
	for _, entry := range result.Files {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		if entry.Status != "" && entry.Status != "active" {
			continue
		}
		if len(providerFilter) > 0 && !providerFilter[entry.Provider] && !providerFilter[entry.Type] {
			continue
		}
		if len(authFilter) > 0 && !authFilter[entry.ID] && !authFilter[entry.AuthIndex] {
			continue
		}
		model := cfg.Models[entry.Provider]
		if model == "" {
			model = cfg.Models[entry.Type]
		}
		targets = append(targets, probeTarget{
			AuthID:    entry.ID,
			AuthIndex: entry.AuthIndex,
			Provider:  entry.Provider,
			AuthType:  entry.Type,
			Model:     model,
		})
	}
	return targets, nil
}

// executeProbe sends one challenge through the host model execution path.
func executeProbe(cfg pluginConfig, target probeTarget, item challenge) (text string, statusCode int, responseModel string, latencyMs int64, err error) {
	messages := make([]chatMessage, 0, 2)
	if item.System != "" {
		messages = append(messages, chatMessage{Role: "system", Content: item.System})
	}
	content := item.Prompt
	if item.UserPrefix != "" {
		content = item.UserPrefix + "\n\n" + item.Prompt
	}
	messages = append(messages, chatMessage{Role: "user", Content: content})
	body := chatCompletionRequest{
		Model:    target.Model,
		Stream:   false,
		Messages: messages,
	}
	bodyRaw, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return "", 0, "", 0, errMarshal
	}
	request := hostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol:  cfg.EntryProtocol,
			ExitProtocol:   cfg.ExitProtocol,
			Model:          target.Model,
			Stream:         false,
			Body:           bodyRaw,
			AuthID:         target.AuthID,
			ForcedProvider: target.Provider,
		},
	}
	started := time.Now()
	var response struct {
		pluginapi.HostModelExecutionResponse
	}
	if errCall := callHost(pluginabi.MethodHostModelExecute, request, &response); errCall != nil {
		return "", 0, "", time.Since(started).Milliseconds(), errCall
	}
	latency := time.Since(started).Milliseconds()
	statusCode = response.StatusCode
	contentText, errExtract := extractResponseContent(response.Body)
	if errExtract != nil {
		return "", statusCode, "", latency, errExtract
	}
	responseModel = extractResponseModel(response.Body)
	return contentText, statusCode, responseModel, latency, nil
}

// extractResponseContent pulls the assistant text out of an OpenAI or
// Anthropic style completion payload.
func extractResponseContent(body []byte) (string, error) {
	if len(body) == 0 {
		return "", fmt.Errorf("empty probe response body")
	}
	var openAI struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
			Text any `json:"text"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(body, &openAI); errUnmarshal == nil {
		if openAI.Error != nil && openAI.Error.Message != "" {
			return "", fmt.Errorf("probe request error: %s", openAI.Error.Message)
		}
		for _, choice := range openAI.Choices {
			if text := contentToString(choice.Message.Content); text != "" {
				return text, nil
			}
			if text := contentToString(choice.Text); text != "" {
				return text, nil
			}
		}
	}
	var anthropic struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(body, &anthropic); errUnmarshal == nil {
		if anthropic.Error != nil && anthropic.Error.Message != "" {
			return "", fmt.Errorf("probe request error: %s", anthropic.Error.Message)
		}
		var builder strings.Builder
		for _, block := range anthropic.Content {
			if block.Type == "text" || block.Type == "" {
				builder.WriteString(block.Text)
			}
		}
		if builder.Len() > 0 {
			return builder.String(), nil
		}
	}
	return "", fmt.Errorf("probe response has no assistant content")
}

// extractResponseModel reads the upstream-reported model name.
func extractResponseModel(body []byte) string {
	var named struct {
		Model string `json:"model"`
	}
	if errUnmarshal := json.Unmarshal(body, &named); errUnmarshal == nil {
		return named.Model
	}
	return ""
}

// contentToString flattens OpenAI string-or-parts content values.
func contentToString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var builder strings.Builder
		for _, part := range v {
			if obj, okObj := part.(map[string]any); okObj {
				if text, okText := obj["text"].(string); okText {
					builder.WriteString(text)
				}
			}
		}
		return builder.String()
	default:
		return ""
	}
}

// classifyVerdict compares the detected model with the expected label.
func classifyVerdict(expected string, detected string, usedOutputs int) string {
	if usedOutputs == 0 {
		return "insufficient"
	}
	if expected == "" {
		return "unlabeled"
	}
	if sameModelLabel(expected, detected) {
		return "match"
	}
	return "mismatch"
}

// sameModelLabel compares model ids tolerating version suffix drift.
func sameModelLabel(expected string, detected string) bool {
	if expected == detected {
		return true
	}
	return strings.TrimSuffix(expected, "-latest") == strings.TrimSuffix(detected, "-latest")
}

// maskID hides most of a credential identifier for unauthenticated pages.
func maskID(id string) string {
	if id == "" {
		return ""
	}
	runes := []rune(id)
	if len(runes) <= 4 {
		return strings.Repeat("*", len(runes))
	}
	keep := 2
	if len(runes) < 8 {
		keep = 1
	}
	return string(runes[:keep]) + "***" + string(runes[len(runes)-2:])
}

// jsonRaw builds a JSON RawMessage from a literal string.
func jsonRaw(raw string) json.RawMessage { return json.RawMessage(raw) }
