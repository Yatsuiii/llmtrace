package providers

import (
	"testing"
)

func TestOpenAIParseUsage(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","usage":{"prompt_tokens":100,"completion_tokens":50}}`)
	u, err := NewOpenAI("").ParseUsage(nil, body)
	if err != nil {
		t.Fatal(err)
	}
	if u.Model != "gpt-4o" || u.InputTokens != 100 || u.OutputTokens != 50 {
		t.Fatalf("got %+v", u)
	}
}

func TestAnthropicParseUsage(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","usage":{"input_tokens":80,"output_tokens":20}}`)
	u, err := NewAnthropic("").ParseUsage(nil, body)
	if err != nil {
		t.Fatal(err)
	}
	if u.Model != "claude-sonnet-4-6" || u.InputTokens != 80 || u.OutputTokens != 20 {
		t.Fatalf("got %+v", u)
	}
}
