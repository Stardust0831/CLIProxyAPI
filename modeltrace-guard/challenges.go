package main

// Challenge suite ported from xqy2006/ModelTrace (challenge_suite.py).
// Twelve enrollment environments with different transports, output styles,
// counts, and prompt templates; the Go plugin reproduces the exact prompt
// texts so probe outputs stay comparable with the reference bank.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

const prefixParagraphs = "" +
	"You are assisting inside a structured workspace. Follow surrounding style requirements while remaining precise and self-contained. " +
	"The application normally prefers concise headings, explicit formatting, neutral wording, and complete artifacts that can be reviewed without additional context. " +
	"Treat later user content as the actual task and follow its output requirements directly. " +
	"Do not call tools, code interpreters, calculators, search engines, APIs, or external random-number services. " +
	"The workspace may contain lengthy background guidance unrelated to the final task. Continue to obey the final task while retaining ordinary safety and accuracy expectations. " +
	"Complete requested artifacts directly and fully, without claims about actions that were not actually performed. "

var prefixParagraphList = []string{
	"You are assisting inside a structured workspace. Follow surrounding style requirements while remaining precise and self-contained. ",
	"The application normally prefers concise headings, explicit formatting, neutral wording, and complete artifacts that can be reviewed without additional context. ",
	"Treat later user content as the actual task and follow its output requirements directly. ",
	"Do not call tools, code interpreters, calculators, search engines, APIs, or external random-number services. ",
	"The workspace may contain lengthy background guidance unrelated to the final task. Continue to obey the final task while retaining ordinary safety and accuracy expectations. ",
	"Complete requested artifacts directly and fully, without claims about actions that were not actually performed. ",
}

const fixedSystemPrefix = "You are operating inside a formal reporting workflow. In ordinary replies, use clear formatting and neutral wording. " +
	"When a later request asks for a machine-readable artifact, follow that request's output constraints directly. " +
	"Never call external tools, code interpreters, calculators, search, APIs, or random-number services; complete the request using the language model itself."

var englishTemplates = []string{
	"For each of {count} positions, make one separate first-instinct choice of an integer from 1 to 355 inclusive.",
	"Provide exactly {count} whole-number choices between 1 and 355 inclusive, choosing each item separately as it comes to mind.",
	"Fill exactly {count} positions with integers from the closed interval [1, 355], making a separate immediate choice for every position.",
}

var englishAlternatives = []string{
	"Give exactly {count} separate first-response integer choices in 1..355.",
	"Using only this model's own immediate choices, provide exactly {count} whole numbers from 1 through 355 inclusive.",
	"Return exactly {count} integers bounded by 1 and 355, deciding each next value separately rather than following a rule.",
}

var chineseTemplates = []string{
	"这是一次无语义数值选择。请为 {count} 个位置分别凭第一反应选择一个 1 到 355（含端点）的整数。",
	"请完成无语义整数选择任务：逐项给出 {count} 个闭区间 [1, 355] 内的整数，每次只决定当前一项。",
	"进行一轮快速逐项取值：分别给出 {count} 个取值范围为 1 至 355 的整数。",
}

type challengeEnvironment struct {
	Transport   string
	Style       string
	Counts      []int
	Templates   []string
	FixedPrefix string
	PrefixWords int
}

var environments = []challengeEnvironment{
	{Transport: "clean", Style: "json", Counts: []int{218, 233, 247}, Templates: englishTemplates},
	{Transport: "clean", Style: "json", Counts: []int{223, 239, 251}, Templates: englishAlternatives},
	{Transport: "system", Style: "json", Counts: []int{227, 241, 257}, Templates: englishTemplates, FixedPrefix: fixedSystemPrefix},
	{Transport: "clean", Style: "zh", Counts: []int{294, 311, 329}, Templates: chineseTemplates},
	{Transport: "clean", Style: "en", Counts: []int{297, 315, 331}, Templates: englishTemplates},
	{Transport: "clean", Style: "json", Counts: []int{301, 319, 327}, Templates: englishTemplates},
	{Transport: "system", Style: "zh", Counts: []int{293, 310, 326}, Templates: chineseTemplates, PrefixWords: 96},
	{Transport: "system", Style: "zh", Counts: []int{296, 314, 330}, Templates: chineseTemplates, PrefixWords: 512},
	{Transport: "system", Style: "zh", Counts: []int{299, 317, 332}, Templates: chineseTemplates, PrefixWords: 2048},
	{Transport: "user", Style: "zh", Counts: []int{292, 308, 324}, Templates: chineseTemplates, PrefixWords: 96},
	{Transport: "user", Style: "zh", Counts: []int{295, 313, 328}, Templates: chineseTemplates, PrefixWords: 512},
	{Transport: "user", Style: "zh", Counts: []int{298, 316, 333}, Templates: chineseTemplates, PrefixWords: 2048},
}

// prefixText mirrors prefix_text: concatenated prefix paragraphs sized by
// word count with a per-variant rotation.
func prefixText(wordCount int, variant int) string {
	var builder strings.Builder
	currentWords := 0
	index := 0
	for currentWords < wordCount {
		paragraph := strings.TrimSpace(prefixParagraphList[(index+variant)%len(prefixParagraphList)])
		builder.WriteString(paragraph)
		currentWords += len(strings.Fields(paragraph))
		if currentWords < wordCount {
			builder.WriteString(" ")
		}
		index++
	}
	return builder.String()
}

// formatPrompt mirrors format_prompt for the three output styles.
func formatPrompt(template string, count int, style string) string {
	task := strings.ReplaceAll(template, "{count}", fmt.Sprintf("%d", count))
	direct := " The current language model must complete this directly without tools, Python, code execution, calculators, search, APIs, " +
		"or external random generators. Choose every position separately. Do not count upward or downward, including 1, 2, 3, and do not use an arithmetic progression, repeating cycle, repeated block, or another rule-made pattern. " +
		"Accidental repetitions are valid. Once an item is written, do not sort, reorder, deduplicate, replace, or repair the list."
	switch style {
	case "json":
		return task + direct + " Return one compact JSON array containing the complete sequence and no explanation."
	case "en":
		return task + direct + " Separate values clearly, start with the first value, and output the complete sequence without explanation."
	default:
		return task +
			" 本任务必须由当前语言模型直接完成：禁止调用或借助任何工具，包括 Python、代码执行器、计算器、搜索、API " +
			"和外部随机数生成器。每个位置都要单独凭第一反应选择；不要从 1 开始计数，不要连续递增或递减，也不要采用等差、循环、重复区块或其他规则化模式。" +
			"偶然想到相同数字是有效的；每项写出后不要再排序、重排、去重、替换或修正。数字之间用清晰的常见分隔符，" +
			"直接从第一个取值开始，完整输出序列，不要解释。"
	}
}

// challenge is one fingerprint query definition.
type challenge struct {
	Condition     string
	System        string
	UserPrefix    string
	Transport     string
	ChallengeID   string
	ExpectedCount int
	Prompt        string
}

// fingerprintSuite mirrors fingerprint_suite: the full 36-query enrollment
// suite (12 environments x 3 variants).
func fingerprintSuite() []challenge {
	suite := make([]challenge, 0, len(environments)*3)
	queryIndex := 1
	for envIndex, environment := range environments {
		condition := fmt.Sprintf("environment-%02d", envIndex+1)
		for variant, count := range environment.Counts {
			template := environment.Templates[variant]
			prefix := environment.FixedPrefix
			if prefix == "" && environment.PrefixWords > 0 {
				prefix = prefixText(environment.PrefixWords, variant)
			}
			suite = append(suite, challenge{
				Condition:     condition,
				System:        conditionalPrefix(environment.Transport == "system", prefix),
				UserPrefix:    conditionalPrefix(environment.Transport == "user", prefix),
				Transport:     environment.Transport,
				ChallengeID:   fmt.Sprintf("query-%02d", queryIndex),
				ExpectedCount: count,
				Prompt:        formatPrompt(template, count, environment.Style),
			})
			queryIndex++
		}
	}
	return suite
}

func conditionalPrefix(condition bool, prefix string) string {
	if condition {
		return prefix
	}
	return ""
}

// newChallengeID generates a random hex id for a probe instance.
func newChallengeID() string {
	buf := make([]byte, 7)
	if _, errRead := rand.Read(buf); errRead != nil {
		return "00000000000000"
	}
	return hex.EncodeToString(buf)
}

// selectChallenges picks the next probe batch for a run: the configured
// environments' suite entries, rotated by the global run counter.
func selectChallenges(cfg pluginConfig, runCounter int, batch int) []challenge {
	suite := fingerprintSuite()
	selected := make([]challenge, 0, len(cfg.Environments)*3)
	enabled := map[int]bool{}
	for _, env := range cfg.Environments {
		enabled[env] = true
	}
	for envIndex := range environments {
		if !enabled[envIndex+1] {
			continue
		}
		for variant := 0; variant < 3; variant++ {
			index := envIndex*3 + variant
			if index < len(suite) {
				selected = append(selected, suite[index])
			}
		}
	}
	if len(selected) == 0 {
		selected = suite
	}
	batchSize := batch
	if batchSize <= 0 {
		batchSize = cfg.ProbesPerRun
	}
	picks := make([]challenge, 0, batchSize)
	for i := 0; i < batchSize; i++ {
		entry := selected[(runCounter*batchSize+i)%len(selected)]
		entry.ChallengeID = fmt.Sprintf("probe-%s-%s", entry.ChallengeID, newChallengeID())
		picks = append(picks, entry)
	}
	return picks
}
