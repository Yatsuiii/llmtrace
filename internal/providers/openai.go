package providers

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type OpenAI struct{ upstreamURL string }

func NewOpenAI(upstreamURL string) *OpenAI {
	if upstreamURL == "" {
		upstreamURL = "https://api.openai.com"
	}
	return &OpenAI{upstreamURL: upstreamURL}
}

func (o *OpenAI) Name() string            { return "openai" }
func (o *OpenAI) UpstreamURL() string     { return o.upstreamURL }
func (o *OpenAI) RoutePrefixes() []string { return []string{"/v1/chat/completions"} }
func (o *OpenAI) SetAuth(r *http.Request, key string) {
	r.Header.Set("Authorization", "Bearer "+key)
}

func (o *OpenAI) ParseUsage(_ *http.Request, respBody []byte) (Usage, error) {
	var resp struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return Usage{}, fmt.Errorf("openai: parse response: %w", err)
	}
	return Usage{
		Model:        resp.Model,
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
	}, nil
}
