package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// bankHellinger mirrors the robust.hellinger artifact in unified_bank.json.
type bankHellinger struct {
	FeatureMean          []float64   `json:"feature_mean"`
	FeatureScale         []float64   `json:"feature_scale"`
	NuisanceRank         int         `json:"nuisance_rank,omitempty"`
	NuisanceEnvironments []string    `json:"nuisance_environments,omitempty"`
	NuisanceBasis        [][]float64 `json:"nuisance_basis"`
	Centroids            [][]float64 `json:"centroids"`
}

// bankOrdered mirrors the robust.ordered_blocks artifact.
type bankOrdered struct {
	Weight               float64       `json:"weight"`
	FeatureMean          []float64     `json:"feature_mean"`
	FeatureScale         []float64     `json:"feature_scale"`
	EnvironmentCentroids [][][]float64 `json:"environment_centroids"`
	NuisanceBasis        [][]float64   `json:"nuisance_basis"`
	Centroids            [][]float64   `json:"centroids"`
}

// bankModel mirrors one enrolled model entry.
type bankModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Family      string `json:"family,omitempty"`
	FamilyName  string `json:"family_name,omitempty"`
	Counts      []int  `json:"counts"`
}

// bankCalibrationEntry mirrors one calibration row.
type bankCalibrationEntry struct {
	Beta       float64 `json:"beta"`
	CVAccuracy float64 `json:"cv_accuracy,omitempty"`
	Fallback   bool    `json:"fallback,omitempty"`
}

// bank mirrors the ModelTrace unified fingerprint bank.
type bank struct {
	Schema              string          `json:"schema,omitempty"`
	BuiltAt             string          `json:"built_at,omitempty"`
	Method              json.RawMessage `json:"method,omitempty"`
	RecommendedQueries  int             `json:"recommended_queries,omitempty"`
	MinimumValidNumbers int             `json:"minimum_valid_numbers,omitempty"`
	SourceScope         string          `json:"source_scope,omitempty"`
	Models              []bankModel     `json:"models"`
	Robust              struct {
		ModelOrder           []string      `json:"model_order"`
		RobustReady          bool          `json:"robust_ready"`
		TrainingRows         int           `json:"training_rows,omitempty"`
		CompleteEnvironments []string      `json:"complete_environments,omitempty"`
		Hellinger            bankHellinger `json:"hellinger"`
		OrderedBlocks        *bankOrdered  `json:"ordered_blocks,omitempty"`
	} `json:"robust"`
	Calibration map[string]bankCalibrationEntry `json:"calibration"`
}

// method extracts the bank method display name.
func (b *bank) methodName() string {
	if len(b.Method) == 0 {
		return "Ordered-block + nuisance-Hellinger"
	}
	var named struct {
		Name string `json:"name"`
	}
	if errUnmarshal := json.Unmarshal(b.Method, &named); errUnmarshal == nil && named.Name != "" {
		return named.Name
	}
	return "Ordered-block + nuisance-Hellinger"
}

// modelByID returns the enrolled model entry with the given id.
func (b *bank) modelByID(id string) (bankModel, bool) {
	for _, model := range b.Models {
		if model.ID == id {
			return model, true
		}
	}
	return bankModel{}, false
}

// displayName returns a human-readable name for the model id.
func (b *bank) displayName(id string) string {
	if model, ok := b.modelByID(id); ok && model.DisplayName != "" {
		return model.DisplayName
	}
	return id
}

// validate checks the loaded bank has the shapes the scorer requires.
func (b *bank) validate() error {
	if len(b.Models) == 0 || len(b.Robust.ModelOrder) == 0 {
		return errBankInvalid{"bank has no enrolled models"}
	}
	h := b.Robust.Hellinger
	if len(h.FeatureMean) != fingerprintDimension || len(h.FeatureScale) != fingerprintDimension {
		return errBankInvalid{"hellinger feature mean/scale dimension mismatch"}
	}
	if len(h.Centroids) != len(b.Robust.ModelOrder) {
		return errBankInvalid{"hellinger centroid count does not match model order"}
	}
	if b.Robust.OrderedBlocks != nil {
		o := b.Robust.OrderedBlocks
		if len(o.FeatureMean) != orderedFeatureDimension || len(o.FeatureScale) != orderedFeatureDimension {
			return errBankInvalid{"ordered block feature dimension mismatch"}
		}
		if len(o.Centroids) != len(b.Robust.ModelOrder) {
			return errBankInvalid{"ordered centroid count does not match model order"}
		}
	}
	if len(b.Calibration) == 0 {
		return errBankInvalid{"bank has no calibration table"}
	}
	return nil
}

// beta returns the calibration temperature for the given number of outputs.
func (b *bank) beta(validOutputs int) float64 {
	if b.Calibration == nil {
		return 1.0
	}
	key := validOutputs
	if key > 3 {
		key = 3
	}
	entry, ok := b.Calibration[strconv.Itoa(key)]
	if !ok {
		if fallback, okThree := b.Calibration["3"]; okThree {
			return fallback.Beta
		}
		return 1.0
	}
	if entry.Fallback {
		if three, okThree := b.Calibration["3"]; okThree && !three.Fallback {
			return three.Beta
		}
	}
	return entry.Beta
}

type errBankInvalid struct{ message string }

func (e errBankInvalid) Error() string { return "fingerprint bank invalid: " + e.message }

// bankCache caches the loaded fingerprint bank keyed by path+mtime+size.
type bankCache struct {
	mu      sync.Mutex
	path    string
	modTime int64
	size    int64
	loaded  *bank
	hash    string
	err     error
}

var cachedBank bankCache

// loadBank returns the cached bank, reloading when the file changed.
func loadBank(path string) (*bank, string, error) {
	cachedBank.mu.Lock()
	defer cachedBank.mu.Unlock()
	info, errStat := os.Stat(path)
	if errStat != nil {
		return nil, cachedBank.hash, errStat
	}
	modTime := info.ModTime().UnixNano()
	if cachedBank.loaded != nil && cachedBank.path == path && cachedBank.modTime == modTime && cachedBank.size == info.Size() {
		return cachedBank.loaded, cachedBank.hash, cachedBank.err
	}
	content, errRead := os.ReadFile(path)
	if errRead != nil {
		cachedBank.reset(path, modTime, info.Size())
		cachedBank.err = errRead
		return nil, cachedBank.hash, errRead
	}
	loaded := bank{}
	if errUnmarshal := json.Unmarshal(content, &loaded); errUnmarshal != nil {
		cachedBank.reset(path, modTime, info.Size())
		cachedBank.err = errUnmarshal
		return nil, cachedBank.hash, errUnmarshal
	}
	if errValidate := loaded.validate(); errValidate != nil {
		cachedBank.reset(path, modTime, info.Size())
		cachedBank.err = errValidate
		return nil, cachedBank.hash, errValidate
	}
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:8])
	cachedBank.path = path
	cachedBank.modTime = modTime
	cachedBank.size = info.Size()
	cachedBank.loaded = &loaded
	cachedBank.hash = hash
	cachedBank.err = nil
	return cachedBank.loaded, cachedBank.hash, nil
}

func (c *bankCache) reset(path string, modTime int64, size int64) {
	c.path = path
	c.modTime = modTime
	c.size = size
	c.loaded = nil
	c.hash = ""
}

// resolveBankPath resolves a configured bank path against the plugin data
// directory when the raw path does not exist.
func resolveBankPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if _, errStat := os.Stat(path); errStat == nil {
		return path
	}
	return path
}
