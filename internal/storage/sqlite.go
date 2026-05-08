package storage

import "time"

type DB struct {
	// to be implemented in week 1
}

type CallRow struct {
	ID           int64
	Timestamp    time.Time
	APIKeyID     string
	Provider     string
	Model        string
	Endpoint     string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	LatencyMs    int64
	Status       int
	ErrorClass   string
	PromptHash   string
	SessionID    string
}

type APIKeyRow struct {
	ID           string
	HashedKey    string
	Label        string
	BudgetUSD    float64
	RateLimitRPM int
	Active       bool
	CreatedAt    time.Time
}

type AnomalyRow struct {
	ID            int64
	DetectedAt    time.Time
	APIKeyID      string
	Date          string
	Metric        string
	BaselineValue float64
	ActualValue   float64
	Delta         float64
	Sigma         float64
	SampleSize    int64
}
