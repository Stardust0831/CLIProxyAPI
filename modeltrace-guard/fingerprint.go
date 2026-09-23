package main

// Fingerprint scoring ported from xqy2006/ModelTrace (fingerprint.py).
// Pipeline: parse the longest 1..355 integer run, build a 355-bin digit
// histogram, score it with a nuisance-projected Hellinger similarity plus a
// weighted ordered-block feature, average over valid outputs, then softmax
// with the bank calibration temperature. Closed-set attribution only.

import "math"

const (
	fingerprintValueMin     = 1
	fingerprintValueMax     = 355
	fingerprintDimension    = fingerprintValueMax - fingerprintValueMin + 1
	fingerprintAlpha        = 0.5
	orderedBlockWeight      = 0.25
	orderedBinsPerChunk     = 16
	orderedChunks           = 4
	orderedFeatureDimension = orderedBinsPerChunk*orderedChunks + 10
)

// parseNumbers extracts the longest run of integers in [1, 355] from text.
// Runs break where the separator contains an alphabetic character, mirroring
// ModelTrace's parse_numbers.
func parseNumbers(text string) []int {
	best := []int{}
	current := []int{}
	previousEnd := 0
	index := 0
	length := len(text)
	for index < length {
		if !isASCIIDigit(text[index]) {
			index++
			continue
		}
		start := index
		for index < length && isASCIIDigit(text[index]) {
			index++
		}
		separator := text[previousEnd:start]
		if len(current) > 0 && containsAlpha(separator) {
			if len(current) > len(best) {
				best = current
			}
			current = []int{}
		}
		if value, ok := parseRangeNumber(text[start:index]); ok {
			current = append(current, value)
		}
		previousEnd = index
	}
	if len(current) > len(best) {
		best = current
	}
	return best
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

func containsAlpha(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') {
			return true
		}
	}
	return false
}

// parseRangeNumber parses an integer string, treating overflows and values
// outside [1, 355] as out of range (Python used arbitrary-precision ints).
func parseRangeNumber(digits string) (int, bool) {
	value := 0
	for i := 0; i < len(digits); i++ {
		d := int(digits[i] - '0')
		if value > (math.MaxInt32-d)/10 {
			return 0, false
		}
		value = value*10 + d
	}
	if value < fingerprintValueMin || value > fingerprintValueMax {
		return 0, false
	}
	return value, true
}

// countNumbers builds the 355-bin histogram of the parsed values.
func countNumbers(numbers []int) []int {
	counts := make([]int, fingerprintDimension)
	for _, value := range numbers {
		if value >= fingerprintValueMin && value <= fingerprintValueMax {
			counts[value-fingerprintValueMin]++
		}
	}
	return counts
}

// standardize mirrors ModelTrace's standardize (population variance).
func standardize(values []float64) []float64 {
	count := float64(len(values))
	if count == 0 {
		return []float64{}
	}
	mean := 0.0
	for _, value := range values {
		mean += value
	}
	mean /= count
	variance := 0.0
	for _, value := range values {
		d := value - mean
		variance += d * d
	}
	variance /= count
	scale := math.Sqrt(variance)
	if scale < 1e-12 {
		scale = 1e-12
	}
	out := make([]float64, len(values))
	for i, value := range values {
		out[i] = (value - mean) / scale
	}
	return out
}

// hellingerFeature mirrors hellinger_feature: sqrt((c + alpha) / sum).
func hellingerFeature(counts []int) []float64 {
	total := 0.0
	values := make([]float64, len(counts))
	for i, count := range counts {
		values[i] = float64(count) + fingerprintAlpha
		total += values[i]
	}
	if total <= 0 {
		total = 1
	}
	out := make([]float64, len(values))
	for i, value := range values {
		out[i] = math.Sqrt(value / total)
	}
	return out
}

// dot returns the inner product of two vectors.
func dot(left []float64, right []float64) float64 {
	sum := 0.0
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for i := 0; i < limit; i++ {
		sum += left[i] * right[i]
	}
	return sum
}

// projectOutBasis removes the nuisance span from the vector in place.
func projectOutBasis(vector []float64, basis [][]float64) {
	for _, row := range basis {
		coefficient := dot(vector, row)
		for i := range vector {
			vector[i] -= coefficient * row[i]
		}
	}
}

// l2Normalize scales the vector to unit length with a 1e-12 floor.
func l2Normalize(vector []float64) []float64 {
	norm := math.Sqrt(dot(vector, vector))
	if norm < 1e-12 {
		norm = 1e-12
	}
	out := make([]float64, len(vector))
	for i, value := range vector {
		out[i] = value / norm
	}
	return out
}

// robustScoreCounts mirrors robust_score_counts: the nuisance-projected
// Hellinger marginal similarity against each bank model center.
func robustScoreCounts(counts []int, b *bank) []float64 {
	feature := hellingerFeature(counts)
	mean := b.Robust.Hellinger.FeatureMean
	scale := b.Robust.Hellinger.FeatureScale
	projected := make([]float64, len(feature))
	for i, value := range feature {
		s := scale[i]
		if s == 0 {
			s = 1e-12
		}
		projected[i] = (value - mean[i]) / s
	}
	projectOutBasis(projected, b.Robust.Hellinger.NuisanceBasis)
	projected = l2Normalize(projected)
	nuisance := make([]float64, len(b.Robust.Hellinger.Centroids))
	for i, centroid := range b.Robust.Hellinger.Centroids {
		nuisance[i] = dot(projected, centroid)
	}
	return standardize(nuisance)
}

// orderedBlockFeature mirrors ordered_block_feature: four 16-bin range
// histograms over split chunks plus a last-digit distribution (74 dims).
func orderedBlockFeature(numbers []float64) []float64 {
	feature := make([]float64, 0, orderedFeatureDimension)
	for chunk := 0; chunk < orderedChunks; chunk++ {
		bins := make([]float64, orderedBinsPerChunk)
		for _, value := range arraySplit(numbers, orderedChunks)[chunk] {
			idx := int(math.Floor((value - float64(fingerprintValueMin)) / chunkBinWidth))
			if idx < 0 {
				idx = 0
			}
			if idx >= orderedBinsPerChunk {
				idx = orderedBinsPerChunk - 1
			}
			bins[idx]++
		}
		feature = append(feature, sqrtHistogram(bins)...)
	}
	last := make([]float64, 10)
	for _, value := range numbers {
		last[int(value)%10]++
	}
	feature = append(feature, sqrtHistogram(last)...)
	return feature
}

const chunkBinWidth = (float64(fingerprintValueMax) + 1.0 - float64(fingerprintValueMin)) / float64(orderedBinsPerChunk)

// sqrtHistogram applies +0.5 smoothing and L1 sqrt normalization.
func sqrtHistogram(bins []float64) []float64 {
	total := 0.0
	values := make([]float64, len(bins))
	for i, count := range bins {
		values[i] = count + 0.5
		total += values[i]
	}
	if total <= 0 {
		total = 1
	}
	out := make([]float64, len(values))
	for i, value := range values {
		out[i] = math.Sqrt(value / total)
	}
	return out
}

// arraySplit mirrors numpy.array_split for a 1-D slice.
func arraySplit(values []float64, sections int) [][]float64 {
	length := len(values)
	if sections <= 0 {
		return [][]float64{values}
	}
	size := length / sections
	remainder := length % sections
	out := make([][]float64, 0, sections)
	offset := 0
	for i := 0; i < sections; i++ {
		chunkSize := size
		if i < remainder {
			chunkSize++
		}
		end := offset + chunkSize
		if end > length {
			end = length
		}
		out = append(out, values[offset:end])
		offset = end
	}
	return out
}

// orderedBlockScores mirrors ordered_block_scores: max environment alignment
// blended with the nuisance-projected ordered similarity.
func orderedBlockScores(numbers []float64, o *bankOrdered, modelCount int) []float64 {
	feature := orderedBlockFeature(numbers)
	standardized := make([]float64, len(feature))
	for i, value := range feature {
		s := o.FeatureScale[i]
		if s == 0 {
			s = 1e-12
		}
		standardized[i] = (value - o.FeatureMean[i]) / s
	}
	normalized := l2Normalize(standardized)

	// Environment alignment: dot with every model template in every
	// environment, then take the element-wise max over environments.
	template := make([]float64, modelCount)
	for e, environment := range o.EnvironmentCentroids {
		if e == 0 {
			for m, centroid := range environment {
				if m < modelCount {
					template[m] = dot(normalized, centroid)
				}
			}
			continue
		}
		for m, centroid := range environment {
			if m >= modelCount {
				continue
			}
			score := dot(normalized, centroid)
			if score > template[m] {
				template[m] = score
			}
		}
	}
	templateStd := standardize(template)

	// Nuisance projection on the standardized (not normalized) feature.
	projected := make([]float64, len(standardized))
	copy(projected, standardized)
	projectOutBasis(projected, o.NuisanceBasis)
	projected = l2Normalize(projected)
	nuisance := make([]float64, modelCount)
	for m, centroid := range o.Centroids {
		if m < modelCount {
			nuisance[m] = dot(projected, centroid)
		}
	}
	nuisanceStd := standardize(nuisance)

	blend := make([]float64, modelCount)
	for m := 0; m < modelCount; m++ {
		blend[m] = 0.5*templateStd[m] + 0.5*nuisanceStd[m]
	}
	return standardize(blend)
}

// robustScoreNumbers mirrors robust_score_numbers: fused marginal + ordered
// similarity for one parsed output.
func robustScoreNumbers(numbers []int, b *bank) []float64 {
	modelCount := len(b.Robust.ModelOrder)
	marginal := robustScoreCounts(countNumbers(numbers), b)
	artifact := b.Robust.OrderedBlocks
	orderedWeight := 0.0
	if artifact != nil {
		orderedWeight = artifact.Weight
	}
	if artifact == nil || orderedWeight == 0.0 {
		return marginal
	}
	values := make([]float64, len(numbers))
	for i, value := range numbers {
		values[i] = float64(value)
	}
	ordered := orderedBlockScores(values, artifact, modelCount)
	fused := make([]float64, modelCount)
	for m := 0; m < modelCount; m++ {
		fused[m] = (1.0-orderedWeight)*marginal[m] + orderedWeight*ordered[m]
	}
	return fused
}

// softmax mirrors ModelTrace's softmax with max subtraction.
func softmax(values []float64) []float64 {
	if len(values) == 0 {
		return []float64{}
	}
	maximum := values[0]
	for _, value := range values {
		if value > maximum {
			maximum = value
		}
	}
	weights := make([]float64, len(values))
	total := 0.0
	for i, value := range values {
		weights[i] = math.Exp(value - maximum)
		total += weights[i]
	}
	if total <= 0 {
		total = 1
	}
	out := make([]float64, len(values))
	for i, weight := range weights {
		out[i] = weight / total
	}
	return out
}

// jsSimilarity mirrors js_similarity for display purposes.
func jsSimilarity(left []int, right []int) float64 {
	leftTotal := 0.0
	rightTotal := fingerprintAlpha * float64(fingerprintDimension)
	for _, value := range left {
		leftTotal += float64(value)
	}
	for _, value := range right {
		rightTotal += float64(value)
	}
	if leftTotal == 0 || rightTotal == 0 {
		return 0
	}
	log2 := math.Log(2.0)
	divergence := func(p float64, m float64) float64 {
		if p <= 0 {
			return 0
		}
		return p * math.Log(p/m)
	}
	js := 0.0
	for i := 0; i < len(left) && i < len(right); i++ {
		p := float64(left[i]) / leftTotal
		q := (float64(right[i]) + fingerprintAlpha) / rightTotal
		mid := (p + q) / 2.0
		js += (divergence(p, mid) + divergence(q, mid)) / 2.0
	}
	return 1.0 - math.Sqrt(js/log2)
}

// fingerprintCandidate is one scored model in the closed set.
type fingerprintCandidate struct {
	Model       string  `json:"model"`
	DisplayName string  `json:"display_name"`
	Family      string  `json:"family,omitempty"`
	FamilyName  string  `json:"family_name,omitempty"`
	Probability float64 `json:"probability"`
	Score       float64 `json:"score"`
	ProfileSim  float64 `json:"profile_similarity"`
	CondProb    float64 `json:"conditional_probability,omitempty"`
}

// fingerprintResult is the closed-set attribution outcome for one run.
type fingerprintResult struct {
	Prediction     string                 `json:"prediction"`
	PredictionName string                 `json:"prediction_name"`
	Family         string                 `json:"family,omitempty"`
	FamilyName     string                 `json:"family_name,omitempty"`
	Probability    float64                `json:"probability"`
	FamilyProb     float64                `json:"family_probability,omitempty"`
	UsedOutputs    int                    `json:"used_outputs"`
	Calibration    int                    `json:"calibration_queries"`
	Beta           float64                `json:"beta"`
	Results        []fingerprintCandidate `json:"results"`
	Diagnostics    []outputDiagnostic     `json:"diagnostics"`
}

type outputDiagnostic struct {
	Index          int  `json:"index"`
	ParsedNumbers  int  `json:"parsed_numbers"`
	MinimumNumbers int  `json:"minimum_numbers"`
	Accepted       bool `json:"accepted"`
}

// scoreOutputs mirrors analyze_outputs + analyze_global_outputs: average the
// fused scores over accepted outputs, softmax with the calibration beta, and
// derive the family prediction.
func scoreOutputs(outputs [][]int, expectedCounts []int, b *bank) (fingerprintResult, error) {
	modelCount := len(b.Robust.ModelOrder)
	modelOrder := b.Robust.ModelOrder
	valid := make([][]int, 0, len(outputs))
	diagnostics := make([]outputDiagnostic, 0, len(outputs))
	for index, numbers := range outputs {
		expected := 0
		if index < len(expectedCounts) {
			expected = expectedCounts[index]
		}
		minimum := 80
		if expected > 0 {
			minimum = int(math.Ceil(float64(expected) * 0.55))
			if minimum < 80 {
				minimum = 80
			}
		}
		accepted := len(numbers) >= minimum
		diagnostics = append(diagnostics, outputDiagnostic{
			Index:          index,
			ParsedNumbers:  len(numbers),
			MinimumNumbers: minimum,
			Accepted:       accepted,
		})
		if accepted {
			valid = append(valid, numbers)
		}
	}
	if len(valid) == 0 {
		return fingerprintResult{}, errNoValidOutputs{}
	}
	combined := make([]float64, modelCount)
	pooled := make([]int, fingerprintDimension)
	for _, numbers := range valid {
		scores := robustScoreNumbers(numbers, b)
		for m := 0; m < modelCount && m < len(scores); m++ {
			combined[m] += scores[m]
		}
		counts := countNumbers(numbers)
		for i := 0; i < fingerprintDimension && i < len(counts); i++ {
			pooled[i] += counts[i]
		}
	}
	for m := range combined {
		combined[m] /= float64(len(valid))
	}
	beta := b.beta(len(valid))
	probabilities := softmax(scaleSlice(combined, beta))
	results := make([]fingerprintCandidate, 0, modelCount)
	for m := 0; m < modelCount; m++ {
		model, okModel := b.modelByID(modelOrder[m])
		display := modelOrder[m]
		family := ""
		familyName := ""
		if okModel {
			if model.DisplayName != "" {
				display = model.DisplayName
			}
			family = model.Family
			familyName = model.FamilyName
		}
		similarity := 0.0
		if okModel && len(model.Counts) > 0 {
			similarity = jsSimilarity(pooled, model.Counts)
		}
		results = append(results, fingerprintCandidate{
			Model:       modelOrder[m],
			DisplayName: display,
			Family:      family,
			FamilyName:  familyName,
			Probability: probabilities[m],
			Score:       combined[m],
			ProfileSim:  similarity,
		})
	}
	sortCandidates(results)

	prediction := results[0]
	// Family rollup: sum probabilities per family, conditional within family.
	familyProbs := map[string]float64{}
	familyOrderList := []string{}
	familyDisplayName := map[string]string{}
	for _, candidate := range results {
		key := candidate.Family
		if key == "" {
			key = "models"
		}
		if _, seen := familyProbs[key]; !seen {
			familyOrderList = append(familyOrderList, key)
			familyDisplayName[key] = candidate.FamilyName
			if familyDisplayName[key] == "" {
				familyDisplayName[key] = key
			}
		}
		familyProbs[key] += candidate.Probability
	}
	for i := range results {
		key := results[i].Family
		if key == "" {
			key = "models"
		}
		if familyProbs[key] > 0 {
			results[i].CondProb = results[i].Probability / familyProbs[key]
		}
	}
	winningFamily := familyOrderList[0]
	for _, family := range familyOrderList {
		if familyProbs[family] > familyProbs[winningFamily] {
			winningFamily = family
		}
	}
	return fingerprintResult{
		Prediction:     prediction.Model,
		PredictionName: prediction.DisplayName,
		Family:         winningFamily,
		FamilyName:     familyDisplayName[winningFamily],
		Probability:    prediction.Probability,
		FamilyProb:     familyProbs[winningFamily],
		UsedOutputs:    len(valid),
		Calibration:    minInt(len(valid), 3),
		Beta:           beta,
		Results:        results,
		Diagnostics:    diagnostics,
	}, nil
}

type errNoValidOutputs struct{}

func (e errNoValidOutputs) Error() string {
	return "no usable probe outputs: refusals and heavily truncated answers are not counted"
}

func scaleSlice(values []float64, factor float64) []float64 {
	out := make([]float64, len(values))
	for i, value := range values {
		out[i] = value * factor
	}
	return out
}

func sortCandidates(candidates []fingerprintCandidate) {
	// Insertion sort by probability descending; the set is small (13).
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].Probability > candidates[j-1].Probability; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}
}

func minInt(left int, right int) int {
	if left < right {
		return left
	}
	return right
}
