package main

import (
	"encoding/json"
	"testing"
	"time"
)

func setTestConfig(t *testing.T, cfg pluginConfig) {
	t.Helper()
	config.Store(&configStore{curr: normalizePluginConfig(cfg)})
}

func candidates(ids ...string) []schedulerAuthCandidate {
	out := make([]schedulerAuthCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, schedulerAuthCandidate{ID: id, Provider: "codex", Status: "active"})
	}
	return out
}

func TestRotatePickSticksInsideWindow(t *testing.T) {
	rotateStates = map[string]*rotateEntry{}
	defer delete(rotateStates, "codex")
	setTestConfig(t, pluginConfig{RotationEnabled: true, RotationWindowMinutes: 4})
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	rotateNow = func() time.Time { return base }
	if got := rotatePick("codex", candidates("a", "b", "c")); got != "a" {
		t.Fatalf("first pick = %q, want a", got)
	}
	// Advance 3 minutes (inside the 4-minute window): same credential.
	rotateNow = func() time.Time { return base.Add(3 * time.Minute) }
	for i := 0; i < 5; i++ {
		if got := rotatePick("codex", candidates("a", "b", "c")); got != "a" {
			t.Fatalf("pick %d inside window = %q, want a", i, got)
		}
	}
}

func TestRotatePickAdvancesAfterWindow(t *testing.T) {
	rotateStates = map[string]*rotateEntry{}
	defer delete(rotateStates, "codex")
	setTestConfig(t, pluginConfig{RotationEnabled: true, RotationWindowMinutes: 4})
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	now := base
	rotateNow = func() time.Time { return now }
	if got := rotatePick("codex", candidates("a", "b", "c")); got != "a" {
		t.Fatalf("first pick = %q, want a", got)
	}
	// At 4 minutes the window is exhausted: rotate to b.
	now = base.Add(4 * time.Minute)
	if got := rotatePick("codex", candidates("a", "b", "c")); got != "b" {
		t.Fatalf("pick after window = %q, want b", got)
	}
	// Inside b's window: still b.
	now = base.Add(6 * time.Minute)
	if got := rotatePick("codex", candidates("a", "b", "c")); got != "b" {
		t.Fatalf("pick inside b window = %q, want b", got)
	}
	// b's window exhausts at 10 minutes: rotate to c.
	now = base.Add(10 * time.Minute)
	if got := rotatePick("codex", candidates("a", "b", "c")); got != "c" {
		t.Fatalf("pick after b window = %q, want c", got)
	}
	// c's window exhausts at 16 minutes: wrap back to a.
	now = base.Add(16 * time.Minute)
	if got := rotatePick("codex", candidates("a", "b", "c")); got != "a" {
		t.Fatalf("pick after c window = %q, want a (wrap)", got)
	}
}

func TestRotatePickCandidateDisappears(t *testing.T) {
	rotateStates = map[string]*rotateEntry{}
	defer delete(rotateStates, "codex")
	setTestConfig(t, pluginConfig{RotationEnabled: true, RotationWindowMinutes: 4})
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	rotateNow = func() time.Time { return base }
	if got := rotatePick("codex", candidates("a", "b", "c")); got != "a" {
		t.Fatalf("first pick = %q, want a", got)
	}
	// The active credential drops out of the candidates (disabled/cooling):
	// start a fresh window on the first remaining candidate.
	if got := rotatePick("codex", candidates("b", "c")); got != "b" {
		t.Fatalf("pick after dropout = %q, want b", got)
	}
}

func TestRotatePickSingleCandidateRestartsWindow(t *testing.T) {
	rotateStates = map[string]*rotateEntry{}
	defer delete(rotateStates, "codex")
	setTestConfig(t, pluginConfig{RotationEnabled: true, RotationWindowMinutes: 4})
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	now := base
	rotateNow = func() time.Time { return now }
	if got := rotatePick("codex", candidates("only")); got != "only" {
		t.Fatalf("first pick = %q, want only", got)
	}
	now = base.Add(5 * time.Minute)
	if got := rotatePick("codex", candidates("only")); got != "only" {
		t.Fatalf("pick after window = %q, want only (restart)", got)
	}
	rotateMu.Lock()
	state := rotateStates["codex"]
	rotateMu.Unlock()
	if state == nil || !state.WindowStart.Equal(now) {
		t.Fatalf("window was not restarted on rotation: %+v", state)
	}
}

func TestRotatePickDisabledAndMixedAndFilter(t *testing.T) {
	rotateStates = map[string]*rotateEntry{}
	setTestConfig(t, pluginConfig{RotationEnabled: false})
	req := schedulerPickRequest{Provider: "codex", Candidates: candidates("a", "b")}
	raw, errHandle := handleSchedulerPick(mustJSON(t, req))
	if errHandle != nil {
		t.Fatalf("handleSchedulerPick: %v", errHandle)
	}
	resp := decodeResponse(t, raw)
	if resp.Handled || resp.AuthID != "" {
		t.Fatalf("disabled rotation returned handled response: %+v", resp)
	}
	// Mixed-provider routing: handled under the shared window key.
	setTestConfig(t, pluginConfig{RotationEnabled: true})
	req.Provider = "mixed"
	resp = decodeResponse(t, mustHandle(t, req))
	if !resp.Handled || resp.AuthID != "a" {
		t.Fatalf("mixed request should pick the first candidate: %+v", resp)
	}
	// Provider filter: unlisted providers fall back to the built-in scheduler.
	setTestConfig(t, pluginConfig{RotationEnabled: true, RotationProviders: []string{"claude"}})
	req.Provider = "codex"
	resp = decodeResponse(t, mustHandle(t, req))
	if resp.Handled {
		t.Fatalf("filtered-out provider should not be handled: %+v", resp)
	}
	// Mixed routing with a filter: only listed providers' candidates rotate.
	req.Provider = "mixed"
	req.Candidates = []schedulerAuthCandidate{{ID: "c1", Provider: "codex"}, {ID: "c2", Provider: "claude"}}
	resp = decodeResponse(t, mustHandle(t, req))
	if !resp.Handled || resp.AuthID != "c2" {
		t.Fatalf("mixed filtered candidates should pick claude only: %+v", resp)
	}
	// Empty candidate list: nothing to pick.
	setTestConfig(t, pluginConfig{RotationEnabled: true})
	req.Provider = "codex"
	req.Candidates = nil
	resp = decodeResponse(t, mustHandle(t, req))
	if resp.Handled {
		t.Fatalf("empty candidates should not be handled: %+v", resp)
	}
}

func TestSchedulerPickRequestDecoding(t *testing.T) {
	rotateStates = map[string]*rotateEntry{}
	defer delete(rotateStates, "codex")
	setTestConfig(t, pluginConfig{RotationEnabled: true, RotationWindowMinutes: 4})
	// Simulate the host RPC payload (uppercase keys, no tags upstream).
	payload := []byte(`{"Provider":"codex","Model":"gpt-5.6","Stream":true,"Candidates":[{"ID":"auth-1","Provider":"codex","Priority":0,"Status":"active"},{"ID":"auth-2","Provider":"codex","Priority":0,"Status":"active"}]}`)
	resp := decodeResponse(t, mustHandleRaw(t, payload))
	if !resp.Handled || resp.AuthID != "auth-1" {
		t.Fatalf("first scheduler pick = %+v, want auth-1", resp)
	}
}

func TestConfigDefaultsRotation(t *testing.T) {
	cfg := normalizePluginConfig(pluginConfig{})
	if cfg.RotationWindowMinutes != defaultRotationWindowMin {
		t.Fatalf("default rotation window = %d, want %d", cfg.RotationWindowMinutes, defaultRotationWindowMin)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	return raw
}

func mustHandle(t *testing.T, req schedulerPickRequest) []byte {
	t.Helper()
	return mustHandleRaw(t, mustJSON(t, req))
}

func mustHandleRaw(t *testing.T, payload []byte) []byte {
	t.Helper()
	raw, errHandle := handleSchedulerPick(payload)
	if errHandle != nil {
		t.Fatalf("handleSchedulerPick: %v", errHandle)
	}
	return raw
}

func decodeResponse(t *testing.T, raw []byte) schedulerPickResponse {
	t.Helper()
	env := envelope{}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("unmarshal envelope %s: %v", raw, errUnmarshal)
	}
	resp := schedulerPickResponse{}
	if len(env.Result) > 0 {
		if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
			t.Fatalf("unmarshal result %s: %v", env.Result, errUnmarshal)
		}
	}
	return resp
}
