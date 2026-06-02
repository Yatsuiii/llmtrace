// Package ratelimit will provide per-API-key token-bucket rate limiting.
// Each key stores a rate_limit_rpm field in the database; this package will
// enforce it in the proxy handler before forwarding upstream.
// Not yet wired in — the field is stored but not enforced.
package ratelimit
