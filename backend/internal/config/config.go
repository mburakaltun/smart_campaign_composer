package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Addr           string
	DatabaseURL    string
	ProviderURL    string
	LLMProvider    string // "anthropic" or "mock"
	AnthropicKey   string
	AnthropicModel string
	SeedUsers      int
	RatePerSec     int // per channel
	Workers        int // per channel
	MaxAttempts    int
	MockLLMDelay   time.Duration
}

func Load() Config {
	c := Config{
		Addr:           env("ADDR", ":8080"),
		DatabaseURL:    env("DATABASE_URL", "postgres://composer:composer@localhost:5432/composer?sslmode=disable"),
		ProviderURL:    env("PROVIDER_URL", "http://localhost:9090"),
		LLMProvider:    os.Getenv("LLM_PROVIDER"),
		AnthropicKey:   os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel: env("ANTHROPIC_MODEL", "claude-sonnet-5-5"),
		SeedUsers:      envInt("SEED_USERS", 4000),
		RatePerSec:     envInt("RATE_PER_SEC", 100),
		Workers:        envInt("WORKERS_PER_CHANNEL", 32),
		MaxAttempts:    envInt("MAX_ATTEMPTS", 5),
		MockLLMDelay:   time.Duration(envInt("MOCK_LLM_DELAY_MS", 25)) * time.Millisecond,
	}
	if c.LLMProvider == "" {
		c.LLMProvider = "mock"
		if c.AnthropicKey != "" {
			c.LLMProvider = "anthropic"
		}
	}
	return c
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return d
}
