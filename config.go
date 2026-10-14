package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Env          string // "sandbox" or "production"
	HTTPAddr     string
	DatabasePath string

	// PaymentProvider is "monnify" (the default) or "kora": who takes the
	// customer's bank transfer. Only that provider's keys are needed.
	PaymentProvider string

	KoraBaseURL   string
	KoraSecretKey string // test or live; Kora tells them apart by the key

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

	MailBaseURL       string
	MailToken         string // ZeptoMail send mail token
	MailFromEmail     string // must belong to a domain verified in ZeptoMail
	MailFromName      string
	MailBounceAddress string // optional; only if ZeptoMail asks for one
	// MailSandbox makes the mailer log instead of sending (ZeptoMail has no
	// sandbox of its own). On by default, like ABSSandbox, so no email goes out
	// until it is turned off.
	MailSandbox bool

	// MinPaymentKobo is the smallest payment the payment provider accepts. It is
	// ₦100 with Kora and 0 (none) with Monnify; MIN_PAYMENT_NAIRA can raise it.
	MinPaymentKobo int64

	// AdminToken protects /admin and, when they are mounted, the direct /abs,
	// /monnify and /email endpoints. It is required whenever real money can be
	// spent (production, or ABS_SANDBOX=false), when only /admin is served.
	AdminToken string
}

// MailConfigured reports whether enough is set to actually send email;
// without it, Service.Mailer is left nil and the receipt email is skipped.
func (c Config) MailConfigured() bool {
	return c.MailToken != "" && c.MailFromEmail != ""
}

// koraMinPaymentKobo is the smallest payment Kora accepts on bank transfer (₦100).
const koraMinPaymentKobo = 10000

func (c Config) IsProduction() bool { return c.Env == "production" }

// RealMoney reports whether the server can spend or take real money: production,
// or ABS purchases that are not sandboxed.
func (c Config) RealMoney() bool { return c.IsProduction() || !c.ABSSandbox }

// LoadConfig reads a .env file (if present) and then the environment.
// Variables already set in the environment win over .env.
func LoadConfig() (Config, error) {
	loadDotEnv(".env")
	c := Config{
		Env:                 envOr("APP_ENV", "sandbox"),
		HTTPAddr:            envOr("HTTP_ADDR", ":8080"),
		DatabasePath:        envOr("DATABASE_PATH", "bills.db"),
		PaymentProvider:     envOr("PAYMENT_PROVIDER", "monnify"),
		KoraBaseURL:         envOr("KORA_BASE_URL", "https://api.korapay.com/merchant"),
		KoraSecretKey:       os.Getenv("KORA_SECRET_KEY"),
		MonnifyBaseURL:      envOr("MONNIFY_BASE_URL", "https://sandbox.monnify.com"),
		MonnifyAPIKey:       os.Getenv("MONNIFY_API_KEY"),
		MonnifySecretKey:    os.Getenv("MONNIFY_SECRET_KEY"),
		MonnifyContractCode: os.Getenv("MONNIFY_CONTRACT_CODE"),
		ABSBaseURL:          envOr("ABS_BASE_URL", "https://absvendor.com"),
		ABSAPIKey:           os.Getenv("ABS_API_KEY"),
		ABSWebhookSecret:    os.Getenv("ABS_WEBHOOK_SECRET"),
		ABSSandbox:          envOr("ABS_SANDBOX", "true") != "false",
		MailBaseURL:         envOr("MAIL_BASE_URL", "https://cpaas.zoho.com"),
		MailToken:           os.Getenv("ZEPTOMAIL_TOKEN"),
		MailFromEmail:       os.Getenv("MAIL_FROM_EMAIL"),
		MailFromName:        envOr("MAIL_FROM_NAME", "Mi MDev"),
		MailBounceAddress:   os.Getenv("MAIL_BOUNCE_ADDRESS"),
		MailSandbox:         envOr("MAIL_SANDBOX", "true") != "false",
		AdminToken:          os.Getenv("ADMIN_TOKEN"),
	}
	if c.PaymentProvider != "monnify" && c.PaymentProvider != "kora" {
		return c, fmt.Errorf("PAYMENT_PROVIDER must be monnify or kora, got %q", c.PaymentProvider)
	}
	if c.PaymentProvider == "kora" {
		c.MinPaymentKobo = koraMinPaymentKobo // Kora refuses less; MIN_PAYMENT_NAIRA may raise it
	}
	if v := os.Getenv("MIN_PAYMENT_NAIRA"); v != "" {
		naira, err := strconv.ParseInt(v, 10, 64)
		if err != nil || naira < 0 {
			return c, fmt.Errorf("MIN_PAYMENT_NAIRA must be a whole number of naira, 0 or more, got %q", v)
		}
		c.MinPaymentKobo = max(c.MinPaymentKobo, naira*100)
	}
	if c.Env != "sandbox" && c.Env != "production" {
		return c, fmt.Errorf("APP_ENV must be sandbox or production, got %q", c.Env)
	}
	if c.PaymentProvider == "kora" && c.KoraSecretKey == "" {
		return c, fmt.Errorf("KORA_SECRET_KEY is required when PAYMENT_PROVIDER=kora")
	}
	if c.IsProduction() {
		required := map[string]string{
			"ABS_API_KEY":        c.ABSAPIKey,
			"ABS_WEBHOOK_SECRET": c.ABSWebhookSecret,
		}
		if c.PaymentProvider == "monnify" {
			required["MONNIFY_API_KEY"] = c.MonnifyAPIKey
			required["MONNIFY_SECRET_KEY"] = c.MonnifySecretKey
			required["MONNIFY_CONTRACT_CODE"] = c.MonnifyContractCode
		}
		for name, v := range required {
			if v == "" {
				return c, fmt.Errorf("%s is required in production", name)
			}
		}
	}
	if c.RealMoney() && c.AdminToken == "" {
		return c, fmt.Errorf("ADMIN_TOKEN is required when real money can be spent (production, or ABS_SANDBOX=false)")
	}
	// Email is optional (Service.Mailer is left nil without it), but a partial
	// setup is almost certainly a mistake rather than an intentional half-config.
	if n := boolCount(c.MailToken != "", c.MailFromEmail != ""); n == 1 {
		return c, fmt.Errorf("ZEPTOMAIL_TOKEN and MAIL_FROM_EMAIL must be set together")
	}
	return c, nil
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
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
