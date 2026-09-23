package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// decodeConfigYAML unmarshals the delivered plugin config subtree (YAML).
func decodeConfigYAML(raw []byte) (pluginConfig, error) {
	cfg := pluginConfig{}
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		return cfg, errUnmarshal
	}
	return cfg, nil
}

// pluginConfig holds the plugin runtime configuration. It is delivered as the
// plugin's own config subtree (config.yaml `plugins.configs.<id>`) merged with
// host defaults (`enabled`, `priority`) as YAML on plugin.register and
// plugin.reconfigure. Host fields are ignored here.
type pluginConfig struct {
	BankPath        string            `yaml:"bank_path"`
	IntervalMinutes int               `yaml:"interval_minutes"`
	ProbesPerRun    int               `yaml:"probes_per_run"`
	Providers       []string          `yaml:"providers"`
	AuthIDs         []string          `yaml:"auth_ids"`
	Models          map[string]string `yaml:"models"`       // provider -> expected model id
	Environments    []int             `yaml:"environments"` // suite environment numbers (1-12)
	HistoryPath     string            `yaml:"history_path"`
	HistorySize     int               `yaml:"history_size"`
	RoutingSize     int               `yaml:"routing_size"`
	EntryProtocol   string            `yaml:"entry_protocol"`
	ExitProtocol    string            `yaml:"exit_protocol"`
}

const (
	defaultBankPath        = "data/unified_bank.json"
	defaultIntervalMinutes = 60
	defaultProbesPerRun    = 3
	defaultHistorySize     = 200
	defaultRoutingSize     = 1000
	defaultProbeEntryProto = "openai"
	defaultProbeExitProto  = "openai"
	minimumIntervalMinutes = 15
	maximumProbesPerRun    = 3
)

func normalizePluginConfig(cfg pluginConfig) pluginConfig {
	if strings.TrimSpace(cfg.BankPath) == "" {
		cfg.BankPath = defaultBankPath
	}
	if cfg.IntervalMinutes < 0 {
		cfg.IntervalMinutes = 0
	}
	if cfg.IntervalMinutes == 0 {
		// Keep the documented default when the field is simply omitted.
		cfg.IntervalMinutes = defaultIntervalMinutes
	}
	if cfg.IntervalMinutes > 0 && cfg.IntervalMinutes < minimumIntervalMinutes {
		cfg.IntervalMinutes = minimumIntervalMinutes
	}
	if cfg.ProbesPerRun <= 0 {
		cfg.ProbesPerRun = defaultProbesPerRun
	}
	if cfg.ProbesPerRun > maximumProbesPerRun {
		cfg.ProbesPerRun = maximumProbesPerRun
	}
	cfg.Providers = trimList(cfg.Providers)
	cfg.AuthIDs = trimList(cfg.AuthIDs)
	if cfg.Models == nil {
		cfg.Models = map[string]string{}
	}
	if len(cfg.Environments) == 0 {
		// Clean-transport suite environments: 01 (json), 05 (en), 06 (json).
		cfg.Environments = []int{1, 5, 6}
	}
	cfg.Environments = filterEnvironments(cfg.Environments)
	if cfg.HistorySize <= 0 {
		cfg.HistorySize = defaultHistorySize
	}
	if cfg.RoutingSize <= 0 {
		cfg.RoutingSize = defaultRoutingSize
	}
	if strings.TrimSpace(cfg.EntryProtocol) == "" {
		cfg.EntryProtocol = defaultProbeEntryProto
	}
	if strings.TrimSpace(cfg.ExitProtocol) == "" {
		cfg.ExitProtocol = defaultProbeExitProto
	}
	return cfg
}

func trimList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

// splitCommaList parses a "a,b,c" style string into a trimmed list.
func splitCommaList(value string) []string {
	return trimList(strings.Split(value, ","))
}

func filterEnvironments(values []int) []int {
	out := make([]int, 0, len(values))
	seen := map[int]bool{}
	for _, value := range values {
		if value < 1 || value > 12 || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	if len(out) == 0 {
		return []int{1, 5, 6}
	}
	return out
}

// configStore guards the active plugin configuration.
type configStore struct {
	mu   sync.RWMutex
	curr pluginConfig
}

func (s *configStore) set(cfg pluginConfig) {
	s.mu.Lock()
	s.curr = cfg
	s.mu.Unlock()
}

func (s *configStore) get() pluginConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.curr
}

var config atomic.Pointer[configStore]

func activeConfig() pluginConfig {
	store := config.Load()
	if store == nil {
		return normalizePluginConfig(pluginConfig{})
	}
	return store.get()
}

// applyConfigYAML unmarshals the delivered plugin config subtree and stores it.
// The host wraps the YAML in a lifecycle JSON payload with a base64-encoded
// config_yaml field.
func applyConfigYAML(raw []byte) {
	if len(raw) > 0 {
		var lifecycle struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if errUnmarshal := json.Unmarshal(raw, &lifecycle); errUnmarshal == nil && len(lifecycle.ConfigYAML) > 0 {
			raw = lifecycle.ConfigYAML
		}
	}
	cfg := pluginConfig{}
	if len(raw) > 0 {
		decoded, errDecode := decodeConfigYAML(raw)
		if errDecode == nil {
			cfg = decoded
		}
	}
	config.Store(&configStore{curr: normalizePluginConfig(cfg)})
}

// applyConfigValues updates individual fields from a JSON management payload.
func applyConfigValues(values map[string]any) {
	current := activeConfig()
	updated := current
	if v, ok := values["bank_path"].(string); ok {
		updated.BankPath = v
	}
	if v, ok := values["interval_minutes"]; ok {
		if n, errConvert := numberToInt(v); errConvert == nil {
			updated.IntervalMinutes = n
		}
	}
	if v, ok := values["probes_per_run"]; ok {
		if n, errConvert := numberToInt(v); errConvert == nil {
			updated.ProbesPerRun = n
		}
	}
	if v, ok := values["providers"]; ok {
		updated.Providers = toList(v)
	}
	if v, ok := values["auth_ids"]; ok {
		updated.AuthIDs = toList(v)
	}
	if v, ok := values["models"].(map[string]any); ok {
		models := map[string]string{}
		for key, model := range v {
			if s, okModel := model.(string); okModel {
				models[key] = s
			}
		}
		updated.Models = models
	}
	if v, ok := values["environments"]; ok {
		if list, okList := v.([]any); okList {
			envs := make([]int, 0, len(list))
			for _, item := range list {
				if n, errConvert := numberToInt(item); errConvert == nil {
					envs = append(envs, n)
				}
			}
			updated.Environments = envs
		}
	}
	if v, ok := values["history_path"].(string); ok {
		updated.HistoryPath = v
	}
	if v, ok := values["history_size"]; ok {
		if n, errConvert := numberToInt(v); errConvert == nil {
			updated.HistorySize = n
		}
	}
	if v, ok := values["routing_size"]; ok {
		if n, errConvert := numberToInt(v); errConvert == nil {
			updated.RoutingSize = n
		}
	}
	if v, ok := values["entry_protocol"].(string); ok {
		updated.EntryProtocol = v
	}
	if v, ok := values["exit_protocol"].(string); ok {
		updated.ExitProtocol = v
	}
	updated = normalizePluginConfig(updated)
	config.Store(&configStore{curr: updated})
	onConfigChanged()
}

func numberToInt(value any) (int, error) {
	switch v := value.(type) {
	case float64:
		return int(v), nil
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case string:
		return strconv.Atoi(strings.TrimSpace(v))
	default:
		return 0, strconv.ErrSyntax
	}
}

func toList(value any) []string {
	switch v := value.(type) {
	case string:
		return splitCommaList(v)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, okItem := item.(string); okItem {
				out = append(out, s)
			}
		}
		return trimList(out)
	case nil:
		return nil
	default:
		return nil
	}
}
