package pricing

import "log"

type ModelRate struct {
	InputPerMillion  float64
	OutputPerMillion float64
}

// Rates as of 2026-05. Check https://www.anthropic.com/pricing and
// https://openai.com/api/pricing when updating.
var rates = map[string]ModelRate{
	// Anthropic
	"claude-opus-4-8":            {InputPerMillion: 15.00, OutputPerMillion: 75.00},
	"claude-opus-4-7":            {InputPerMillion: 15.00, OutputPerMillion: 75.00},
	"claude-sonnet-4-6":          {InputPerMillion: 3.00, OutputPerMillion: 15.00},
	"claude-haiku-4-5-20251001":  {InputPerMillion: 0.80, OutputPerMillion: 4.00},
	"claude-haiku-4-5":           {InputPerMillion: 0.80, OutputPerMillion: 4.00},
	"claude-3-5-sonnet-20241022": {InputPerMillion: 3.00, OutputPerMillion: 15.00},
	"claude-3-5-haiku-20241022":  {InputPerMillion: 0.80, OutputPerMillion: 4.00},
	"claude-3-opus-20240229":     {InputPerMillion: 15.00, OutputPerMillion: 75.00},

	// OpenAI
	"gpt-4o":        {InputPerMillion: 2.50, OutputPerMillion: 10.00},
	"gpt-4o-mini":   {InputPerMillion: 0.15, OutputPerMillion: 0.60},
	"gpt-4-turbo":   {InputPerMillion: 10.00, OutputPerMillion: 30.00},
	"gpt-4":         {InputPerMillion: 30.00, OutputPerMillion: 60.00},
	"gpt-3.5-turbo": {InputPerMillion: 0.50, OutputPerMillion: 1.50},
	"o1":            {InputPerMillion: 15.00, OutputPerMillion: 60.00},
	"o1-mini":       {InputPerMillion: 1.10, OutputPerMillion: 4.40},
	"o3-mini":       {InputPerMillion: 1.10, OutputPerMillion: 4.40},
}

func Lookup(model string) (ModelRate, bool) {
	r, ok := rates[model]
	return r, ok
}

func Cost(model string, inputTokens, outputTokens int) float64 {
	r, ok := Lookup(model)
	if !ok {
		log.Printf("pricing: unknown model %q — cost recorded as $0, update internal/pricing/rates.go", model)
		return 0
	}
	return float64(inputTokens)*r.InputPerMillion/1_000_000 +
		float64(outputTokens)*r.OutputPerMillion/1_000_000
}
