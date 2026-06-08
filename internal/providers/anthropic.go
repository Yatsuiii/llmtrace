package providers

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type Anthropic struct{ upstreamURL string }

func NewAnthropic(upstreamURL string) *Anthropic {
	if upstreamURL == "" {
		upstreamURL = "https://api.anthropic.com"
	}
	return &Anthropic{upstreamURL: upstreamURL}
}

func (a *Anthropic) Name() string                { return "anthropic" }
func (a *Anthropic) UpstreamURL() string         { return a.upstreamURL }
func (a *Anthropic) RoutePrefixes() []string     { return []string{"/v1/messages"} }
func (a *Anthropic) SetAuth(r *http.Request, key string) { r.Header.Set("x-api-key", key) }

func (a *Anthropic) ParseUsage(_ *http.Request, respBody []byte) (Usage, error) {
	var resp struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return Usage{}, fmt.Errorf("anthropic: parse response: %w", err)
	}
	return Usage{
		Model:        resp.Model,
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
	}, nil
}
