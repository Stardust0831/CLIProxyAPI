package main

// Routing recorder: the usage plugin hook delivers a marshaled
// pluginapi.UsageRecord (Go-default JSON keys) per completed request. This
// file converts it into a compact routing record for the dashboard and
// management API. Secrets (API keys) are dropped immediately.

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// usageRecord mirrors the delivered pluginapi.UsageRecord JSON. Field names
// are Go-default (capitalized) because the SDK type has no json tags.
type usageRecord struct {
	Provider            string       `json:"Provider"`
	BaseURL             string       `json:"BaseURL"`
	ExecutorType        string       `json:"ExecutorType"`
	Model               string       `json:"Model"`
	Alias               string       `json:"Alias"`
	APIKey              string       `json:"APIKey"`
	SessionID           string       `json:"SessionID"`
	ParentSessionID     string       `json:"ParentSessionID"`
	AuthID              string       `json:"AuthID"`
	AuthIndex           string       `json:"AuthIndex"`
	AuthType            string       `json:"AuthType"`
	Source              string       `json:"Source"`
	ReasoningEffort     string       `json:"ReasoningEffort"`
	ServiceTier         string       `json:"ServiceTier"`
	ResponseServiceTier string       `json:"ResponseServiceTier"`
	ResponseModel       string       `json:"ResponseModel"`
	Generate            bool         `json:"Generate"`
	Stream              bool         `json:"Stream"`
	RequestedAt         time.Time    `json:"RequestedAt"`
	Latency             int64        `json:"Latency"`
	TTFT                int64        `json:"TTFT"`
	Failed              bool         `json:"Failed"`
	Failure             usageFailure `json:"Failure"`
	Detail              usageDetail  `json:"Detail"`
}

type usageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body"`
}

type usageDetail struct {
	InputTokens         int64 `json:"InputTokens"`
	OutputTokens        int64 `json:"OutputTokens"`
	ReasoningTokens     int64 `json:"ReasoningTokens"`
	CachedTokens        int64 `json:"CachedTokens"`
	CacheReadTokens     int64 `json:"CacheReadTokens"`
	CacheCreationTokens int64 `json:"CacheCreationTokens"`
	TotalTokens         int64 `json:"TotalTokens"`
}

// routingRecord is the sanitized per-request routing entry.
type routingRecord struct {
	Time          time.Time `json:"time"`
	Provider      string    `json:"provider"`
	Model         string    `json:"model"`
	Alias         string    `json:"alias,omitempty"`
	ResponseModel string    `json:"response_model,omitempty"`
	AuthID        string    `json:"auth_id,omitempty"`
	AuthIDMasked  string    `json:"auth_id_masked"`
	AuthType      string    `json:"auth_type,omitempty"`
	Source        string    `json:"source,omitempty"`
	Stream        bool      `json:"stream"`
	LatencyMs     int64     `json:"latency_ms"`
	TTFTMs        int64     `json:"ttft_ms,omitempty"`
	Failed        bool      `json:"failed"`
	FailureCode   int       `json:"failure_code,omitempty"`
	InputTokens   int64     `json:"input_tokens,omitempty"`
	OutputTokens  int64     `json:"output_tokens,omitempty"`
	TotalTokens   int64     `json:"total_tokens,omitempty"`
}

// recordRing is a fixed-size in-memory ring buffer.
type recordRing struct {
	mu    sync.RWMutex
	items []json.RawMessage
	next  int
	count int
	limit int
}

func newRecordRing(limit int) *recordRing {
	return &recordRing{items: make([]json.RawMessage, limit), limit: limit}
}

func (r *recordRing) push(value json.RawMessage) {
	if r.limit <= 0 {
		return
	}
	r.mu.Lock()
	r.items[r.next] = value
	r.next = (r.next + 1) % r.limit
	if r.count < r.limit {
		r.count++
	}
	r.mu.Unlock()
}

func (r *recordRing) snapshot(limit int) []json.RawMessage {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]json.RawMessage, 0, r.count)
	// Oldest first.
	for i := 0; i < r.count; i++ {
		index := (r.next - r.count + i + r.limit) % r.limit
		out = append(out, r.items[index])
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func (r *recordRing) size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.count
}

func (r *recordRing) resize(limit int) {
	if limit <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit == r.limit {
		return
	}
	old := r.snapshot(0)
	r.limit = limit
	r.items = make([]json.RawMessage, limit)
	r.next = 0
	r.count = 0
	start := 0
	if len(old) > limit {
		start = len(old) - limit
	}
	for _, item := range old[start:] {
		r.items[r.count] = item
		r.count++
	}
	r.next = r.count % limit
}

var (
	probeHistory   *recordRing
	routingHistory *recordRing
	ringsOnce      sync.Once
)

func initRings() {
	cfg := activeConfig()
	probeHistory = newRecordRing(cfg.HistorySize)
	routingHistory = newRecordRing(cfg.RoutingSize)
}

func ensureRings() {
	ringsOnce.Do(initRings)
}

func appendProbeRecord(record probeRecord) {
	ensureRings()
	raw, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		return
	}
	probeHistory.push(raw)
	if path := activeConfig().HistoryPath; path != "" {
		appendJSONL(path, raw)
	}
}

func appendRoutingRecord(record routingRecord) {
	ensureRings()
	raw, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		return
	}
	routingHistory.push(raw)
}

// handleUsage converts a delivered UsageRecord into a routing record.
func handleUsage(request []byte) ([]byte, error) {
	record := usageRecord{}
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &record); errUnmarshal != nil {
			return okEnvelope(map[string]any{})
		}
	}
	appendRoutingRecord(routingRecord{
		Time:          orNow(record.RequestedAt),
		Provider:      record.Provider,
		Model:         record.Model,
		Alias:         record.Alias,
		ResponseModel: record.ResponseModel,
		AuthID:        record.AuthID,
		AuthIDMasked:  maskID(record.AuthID),
		AuthType:      record.AuthType,
		Source:        record.Source,
		Stream:        record.Stream,
		LatencyMs:     record.Latency / int64(time.Millisecond),
		TTFTMs:        record.TTFT / int64(time.Millisecond),
		Failed:        record.Failed,
		FailureCode:   record.Failure.StatusCode,
		InputTokens:   record.Detail.InputTokens,
		OutputTokens:  record.Detail.OutputTokens,
		TotalTokens:   record.Detail.TotalTokens,
	})
	return okEnvelope(map[string]any{})
}

func orNow(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value
}

// appendJSONL appends a record to a JSONL history file (best effort).
func appendJSONL(path string, value json.RawMessage) {
	file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		return
	}
	defer func() {
		_ = file.Close()
	}()
	_, _ = file.Write(append(value, '\n'))
}
