package config

type Config struct {
	Proxy     ProxyConfig
	Providers ProvidersConfig
	GitHub    GitHubConfig
	Detection DetectionConfig
	Output    OutputConfig
}

type ProxyConfig struct {
	Listen  string
	TLSCert string
	TLSKey  string
}

type ProvidersConfig struct {
	Anthropic ProviderConfig
	OpenAI    ProviderConfig
	Bedrock   ProviderConfig
}

type ProviderConfig struct {
	UpstreamURL    string
	UpstreamKeyEnv string
}

type GitHubConfig struct {
	Repo                  string
	TokenEnv              string
	DeployWorkflowPattern string
}

type DetectionConfig struct {
	BaselineDays           int
	SigmaThreshold         float64
	MinDeltaUSD            float64
	CorrelationWindowHours int
}

type OutputConfig struct {
	Format string
}

func Defaults() *Config {
	return &Config{
		Proxy: ProxyConfig{Listen: "0.0.0.0:8080"},
		Providers: ProvidersConfig{
			Anthropic: ProviderConfig{
				UpstreamURL:    "https://api.anthropic.com",
				UpstreamKeyEnv: "ANTHROPIC_API_KEY",
			},
			OpenAI: ProviderConfig{
				UpstreamURL:    "https://api.openai.com",
				UpstreamKeyEnv: "OPENAI_API_KEY",
			},
		},
		GitHub: GitHubConfig{
			TokenEnv:              "GITHUB_TOKEN",
			DeployWorkflowPattern: "deploy.*",
		},
		Detection: DetectionConfig{
			BaselineDays:           7,
			SigmaThreshold:         2.5,
			MinDeltaUSD:            5,
			CorrelationWindowHours: 4,
		},
		Output: OutputConfig{Format: "text"},
	}
}
