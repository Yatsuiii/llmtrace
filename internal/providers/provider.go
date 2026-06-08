// Package providers defines the interface for LLM provider backends and
// implements Anthropic and OpenAI. Add a new provider by implementing Provider
// and wiring it into the proxy via its RoutePrefixes.
package providers

import "net/http"

type Provider interface {
	Name() string
	UpstreamURL() string
	RoutePrefixes() []string
	// SetAuth injects the upstream API key using the provider's auth scheme.
	SetAuth(r *http.Request, key string)
	ParseUsage(req *http.Request, respBody []byte) (Usage, error)
}

type Usage struct {
	Model        string
	InputTokens  int
	OutputTokens int
}
