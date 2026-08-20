package web

import "strings"

// modelPrice stores local reference prices in USD per one million tokens.
// These values are estimates for usage reference only and may differ from
// Microsoft, OpenAI, Anthropic, or other upstream billing.
type modelPrice struct {
	InputPerMillion       float64 `json:"input_per_million"`
	CachedInputPerMillion float64 `json:"cached_input_per_million"`
	OutputPerMillion      float64 `json:"output_per_million"`
	Currency              string  `json:"currency"`
	Estimated             bool    `json:"estimated"`
}

var builtInModelPrices = map[string]modelPrice{
	"gpt-5.2":                 {InputPerMillion: 1.75, CachedInputPerMillion: 0.175, OutputPerMillion: 14.00, Currency: "USD", Estimated: true},
	"gpt-5.2-reasoning":       {InputPerMillion: 1.75, CachedInputPerMillion: 0.175, OutputPerMillion: 14.00, Currency: "USD", Estimated: true},
	"gpt-5.3":                 {InputPerMillion: 1.75, CachedInputPerMillion: 0.175, OutputPerMillion: 14.00, Currency: "USD", Estimated: true},
	"gpt-5.4":                 {InputPerMillion: 2.50, CachedInputPerMillion: 0.250, OutputPerMillion: 15.00, Currency: "USD", Estimated: true},
	"gpt-5.4-reasoning":       {InputPerMillion: 2.50, CachedInputPerMillion: 0.250, OutputPerMillion: 15.00, Currency: "USD", Estimated: true},
	"gpt-5.5":                 {InputPerMillion: 2.50, CachedInputPerMillion: 0.250, OutputPerMillion: 15.00, Currency: "USD", Estimated: true},
	"gpt-5.5-reasoning":       {InputPerMillion: 2.50, CachedInputPerMillion: 0.250, OutputPerMillion: 15.00, Currency: "USD", Estimated: true},
	"gpt-5.6-reasoning":       {InputPerMillion: 2.50, CachedInputPerMillion: 0.250, OutputPerMillion: 15.00, Currency: "USD", Estimated: true},
	"claude-sonnet":           {InputPerMillion: 3.00, CachedInputPerMillion: 0.30, OutputPerMillion: 15.00, Currency: "USD", Estimated: true},
	"claude-sonnet-reasoning": {InputPerMillion: 3.00, CachedInputPerMillion: 0.30, OutputPerMillion: 15.00, Currency: "USD", Estimated: true},
	"m365-copilot":            {InputPerMillion: 2.50, CachedInputPerMillion: 0.250, OutputPerMillion: 15.00, Currency: "USD", Estimated: true},
}

var defaultModelPrice = modelPrice{
	InputPerMillion:       2.50,
	CachedInputPerMillion: 0.250,
	OutputPerMillion:      15.00,
	Currency:              "USD",
	Estimated:             true,
}

func priceForModel(model string) modelPrice {
	key := strings.ToLower(strings.TrimSpace(model))
	if p, ok := builtInModelPrices[key]; ok {
		return p
	}
	return defaultModelPrice
}

// estimateUsageCost calculates a local reference cost. InputTokens represents
// newly processed, non-cached input. CacheTokens is charged at the cached-input
// rate, so incremental requests using only a small new suffix still account for
// the reused cached prefix independently.
func estimateUsageCost(model string, inputTokens, outputTokens, cacheTokens int64) float64 {
	p := priceForModel(model)
	return float64(inputTokens)/1_000_000*p.InputPerMillion +
		float64(cacheTokens)/1_000_000*p.CachedInputPerMillion +
		float64(outputTokens)/1_000_000*p.OutputPerMillion
}
