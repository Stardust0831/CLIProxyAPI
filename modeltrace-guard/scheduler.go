package main

// Time-window rotation scheduler. Each provider sticks to one credential for a
// fixed window (default 4 minutes) and then rotates to the next candidate in a
// stable, deterministic order. Requests arriving inside the window reuse the
// current credential; when the window expires the next pick advances. This is
// implemented as a host scheduler plugin: the host offers every available
// candidate on each request and the plugin returns exactly one AuthID.

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

// schedulerPickRequest mirrors pluginapi.SchedulerPickRequest (uppercase JSON
// keys, no json tags on the upstream struct).
type schedulerPickRequest struct {
	Provider   string                   `json:"Provider"`
	Providers  []string                 `json:"Providers"`
	Model      string                   `json:"Model"`
	Stream     bool                     `json:"Stream"`
	Candidates []schedulerAuthCandidate `json:"Candidates"`
}

// schedulerAuthCandidate mirrors pluginapi.SchedulerAuthCandidate.
type schedulerAuthCandidate struct {
	ID         string            `json:"ID"`
	Provider   string            `json:"Provider"`
	Priority   int               `json:"Priority"`
	Status     string            `json:"Status"`
	Attributes map[string]string `json:"Attributes"`
}

// schedulerPickResponse mirrors pluginapi.SchedulerPickResponse.
type schedulerPickResponse struct {
	AuthID          string `json:"AuthID,omitempty"`
	DelegateBuiltin string `json:"DelegateBuiltin,omitempty"`
	Handled         bool   `json:"Handled"`
}

// rotateEntry tracks the active credential window for one provider key.
type rotateEntry struct {
	AuthID      string    `json:"auth_id"`
	WindowStart time.Time `json:"window_start"`
	Switches    int64     `json:"switches"`
}

var (
	rotateMu     sync.Mutex
	rotateStates = map[string]*rotateEntry{}
	rotateNow    = func() time.Time { return time.Now().UTC() }
)

// rotationMixedKey anchors the shared window for mixed-provider routing, where
// the host offers one merged candidate list regardless of provider.
const rotationMixedKey = "mixed"

// handleSchedulerPick answers one scheduler.pick RPC.
func handleSchedulerPick(request []byte) ([]byte, error) {
	req := schedulerPickRequest{}
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return errorEnvelope("invalid_request", "invalid scheduler.pick request"), nil
	}
	cfg := activeConfig()
	resp := schedulerPickResponse{}
	if !cfg.RotationEnabled {
		resp.Handled = false
		return okEnvelope(resp)
	}
	providerKey := strings.ToLower(strings.TrimSpace(req.Provider))
	candidates := req.Candidates
	if providerKey == "" || providerKey == "mixed" {
		// Mixed-provider routing: the host offers the merged candidate list
		// under a single window keyed rotationMixedKey. When a provider
		// filter is configured, only the listed providers' candidates
		// participate in the rotation.
		if len(cfg.RotationProviders) > 0 {
			candidates = filterProviders(candidates, cfg.RotationProviders)
			if len(candidates) == 0 {
				resp.Handled = false
				return okEnvelope(resp)
			}
		}
		authID := rotatePick(rotationMixedKey, candidates)
		if authID == "" {
			resp.Handled = false
			return okEnvelope(resp)
		}
		resp.Handled = true
		resp.AuthID = authID
		return okEnvelope(resp)
	}
	if len(cfg.RotationProviders) > 0 && !providerInList(cfg.RotationProviders, providerKey) {
		resp.Handled = false
		return okEnvelope(resp)
	}
	authID := rotatePick(providerKey, candidates)
	if authID == "" {
		resp.Handled = false
		return okEnvelope(resp)
	}
	resp.Handled = true
	resp.AuthID = authID
	return okEnvelope(resp)
}

// filterProviders keeps only candidates whose provider is listed.
func filterProviders(candidates []schedulerAuthCandidate, providers []string) []schedulerAuthCandidate {
	out := make([]schedulerAuthCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if providerInList(providers, strings.ToLower(strings.TrimSpace(candidate.Provider))) {
			out = append(out, candidate)
		}
	}
	return out
}

// rotatePick resolves the credential for one provider key under the time
// window policy. It returns "" when no candidate is available.
func rotatePick(providerKey string, candidates []schedulerAuthCandidate) string {
	ids := sortedCandidateIDs(candidates)
	if len(ids) == 0 {
		return ""
	}
	window := time.Duration(activeConfig().RotationWindowMinutes) * time.Minute
	if window <= 0 {
		window = 4 * time.Minute
	}

	rotateMu.Lock()
	defer rotateMu.Unlock()
	state := rotateStates[providerKey]
	now := rotateNow()
	if state == nil {
		state = &rotateEntry{AuthID: ids[0], WindowStart: now}
		rotateStates[providerKey] = state
		return state.AuthID
	}
	if index := indexOfID(ids, state.AuthID); index >= 0 {
		if now.Sub(state.WindowStart) < window {
			// Inside the window: stick to the current credential.
			return state.AuthID
		}
		// Window expired: advance to the next candidate, wrapping around.
		nextIndex := (index + 1) % len(ids)
		state.AuthID = ids[nextIndex]
		state.WindowStart = now
		state.Switches++
		return state.AuthID
	}
	// The previous credential disappeared from the candidate list (disabled,
	// cooling down, or removed). Start a fresh window on the first candidate.
	state.AuthID = ids[0]
	state.WindowStart = now
	state.Switches++
	return state.AuthID
}

// sortedCandidateIDs returns the deduplicated candidate IDs in a stable order.
func sortedCandidateIDs(candidates []schedulerAuthCandidate) []string {
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		id := strings.TrimSpace(candidate.ID)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func indexOfID(ids []string, id string) int {
	for i, candidate := range ids {
		if candidate == id {
			return i
		}
	}
	return -1
}

func providerInList(list []string, providerKey string) bool {
	for _, value := range list {
		if strings.ToLower(strings.TrimSpace(value)) == providerKey {
			return true
		}
	}
	return false
}

// rotationStatus summarizes the current windows for the management API.
func rotationStatus() map[string]any {
	rotateMu.Lock()
	defer rotateMu.Unlock()
	window := time.Duration(activeConfig().RotationWindowMinutes) * time.Minute
	entries := map[string]any{}
	for providerKey, state := range rotateStates {
		remaining := window - rotateNow().Sub(state.WindowStart)
		if remaining < 0 {
			remaining = 0
		}
		entries[providerKey] = map[string]any{
			"auth_id":           maskID(state.AuthID),
			"window_started_at": state.WindowStart.Format(time.RFC3339),
			"remaining_seconds": int(remaining.Seconds()),
			"switches":          state.Switches,
		}
	}
	return entries
}
