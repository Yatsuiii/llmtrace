// Package providers defines the interface for LLM provider backends.
// Currently only Anthropic is implemented (in internal/proxy).
// OpenAI is planned for v0.3; Bedrock after that.
// When a new provider is added, it should implement this interface and be
// wired into the proxy handler so the rest of the ledger stays provider-agnostic.
package providers

import "net/http"

type Provider interface {
	Name() string
	UpstreamURL() string
	RoutePrefixes() []string
	ParseUsage(req *http.Request, respBody []byte) (Usage, error)
}

type Usage struct {
	Model        string
	InputTokens  int
	OutputTokens int
}
