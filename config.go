package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

type Config struct {
	Env          string // "sandbox" or "production"
	HTTPAddr     string
	DatabasePath string

	MonnifyBaseURL      string
	MonnifyAPIKey       string
	MonnifySecretKey    string
	MonnifyContractCode string

	ABSBaseURL       string
	ABSAPIKey        string
	ABSWebhookSecret string
	// ABSSandbox makes ABS validate purchases without charging or vending.
	// It is on by default and must be turned off explicitly to spend real money.
	ABSSandbox bool

	// AdminToken protects the direct /abs and /monnify endpoints. It is required
	// whenever real money can be spent (production, or ABS_SANDBOX=false).
	AdminToken string
}

func (c Config) IsProduction() bool { return c.Env == "production" }

// LoadConfig reads a .env file (if present) and then the environment.
// Variables already set in the environment win over .env.
func LoadConfig() (Config, error) {
	loadDotEnv(".env")
	c := Config{
		Env:                 envOr("APP_ENV", "sandbox"),
		HTTPAddr:            envOr("HTTP_ADDR", ":8080"),
		DatabasePath:        envOr("DATABASE_PATH", "bills.db"),
		MonnifyBaseURL:      envOr("MONNIFY_BASE_URL", "https://sandbox.monnify.com"),
		MonnifyAPIKey:       os.Getenv("MONNIFY_API_KEY"),
		MonnifySecretKey:    os.Getenv("MONNIFY_SECRET_KEY"),
		MonnifyContractCode: os.Getenv("MONNIFY_CONTRACT_CODE"),
		ABSBaseURL:          envOr("ABS_BASE_URL", "https://absvendor.com"),
		ABSAPIKey:           os.Getenv("ABS_API_KEY"),
		ABSWebhookSecret:    os.Getenv("ABS_WEBHOOK_SECRET"),
		ABSSandbox:          envOr("ABS_SANDBOX", "true") != "false",
		AdminToken:          os.Getenv("ADMIN_TOKEN"),
	}
	if c.Env != "sandbox" && c.Env != "production" {
		return c, fmt.Errorf("APP_ENV must be sandbox or production, got %q", c.Env)
	}
	if c.IsProduction() {
		for name, v := range map[string]string{
			"MONNIFY_API_KEY":       c.MonnifyAPIKey,
			"MONNIFY_SECRET_KEY":    c.MonnifySecretKey,
			"MONNIFY_CONTRACT_CODE": c.MonnifyContractCode,
			"ABS_API_KEY":           c.ABSAPIKey,
			"ABS_WEBHOOK_SECRET":    c.ABSWebhookSecret,
		} {
			if v == "" {
				return c, fmt.Errorf("%s is required in production", name)
			}
		}
	}
	if (c.IsProduction() || !c.ABSSandbox) && c.AdminToken == "" {
		return c, fmt.Errorf("ADMIN_TOKEN is required when real money can be spent (production, or ABS_SANDBOX=false)")
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadDotEnv sets KEY=VALUE lines from path. Missing file is fine.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if os.Getenv(key) == "" {
			os.Setenv(key, strings.Trim(strings.TrimSpace(value), `"'`))
		}
	}
}
