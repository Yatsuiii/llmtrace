package pricing

type ModelRate struct {
	InputPerMillion  float64
	OutputPerMillion float64
}

var rates = map[string]ModelRate{
	// Anthropic — placeholder, refresh against official docs before shipping
	"claude-opus-4-7":          {InputPerMillion: 15.00, OutputPerMillion: 75.00},
	"claude-sonnet-4-6":        {InputPerMillion: 3.00, OutputPerMillion: 15.00},
	"claude-haiku-4-5-20251001": {InputPerMillion: 0.80, OutputPerMillion: 4.00},
}

func Lookup(model string) (ModelRate, bool) {
	r, ok := rates[model]
	return r, ok
}

func Cost(model string, inputTokens, outputTokens int) float64 {
	r, ok := Lookup(model)
	if !ok {
		return 0
	}
	return float64(inputTokens)*r.InputPerMillion/1_000_000 +
		float64(outputTokens)*r.OutputPerMillion/1_000_000
}
