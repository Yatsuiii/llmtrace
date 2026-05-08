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
