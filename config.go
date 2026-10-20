package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"Bills-Backend/order"
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

	// Pricing. What ABS charges is marked up by a percentage per network before
	// the customer sees it. Each markup has to be at least 2%, or plans would sell
	// at a loss after the payment provider's fee of about 1.6%. Percentages are
	// percent over the ABS price.
	MarkupMTN, MarkupMTNLarge    float64 // MTN plans below and from MarkupLargeFromNaira
	MarkupLargeFromNaira         int64
	MarkupAirtel, MarkupGlo      float64
	Markup9mobile, MarkupDefault float64 // MarkupDefault covers any network not named above
	HiddenPlans                  []string
	ElectricityFeePercent        float64
	ElectricityFeeMinNaira       int64
	PaymentFeeWarnPercent        float64

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
	var err error
	for _, p := range []struct {
		key      string
		fallback float64
		into     *float64
	}{
		{"MARKUP_MTN_PERCENT", 2.5, &c.MarkupMTN},
		{"MARKUP_MTN_LARGE_PERCENT", 2.2, &c.MarkupMTNLarge},
		{"MARKUP_AIRTEL_PERCENT", 4, &c.MarkupAirtel},
		{"MARKUP_GLO_PERCENT", 5, &c.MarkupGlo},
		{"MARKUP_9MOBILE_PERCENT", 5, &c.Markup9mobile},
		{"MARKUP_DEFAULT_PERCENT", 4, &c.MarkupDefault},
		{"ELECTRICITY_FEE_PERCENT", 1.8, &c.ElectricityFeePercent},
		{"PAYMENT_FEE_WARN_PERCENT", 1.7, &c.PaymentFeeWarnPercent},
	} {
		if *p.into, err = envFloat(p.key, p.fallback); err != nil {
			return c, err
		}
	}
	largeFrom, err := envFloat("MARKUP_LARGE_FROM_NAIRA", 20000)
	if err != nil {
		return c, err
	}
	c.MarkupLargeFromNaira = int64(largeFrom)
	minFee, err := envFloat("ELECTRICITY_FEE_MIN_NAIRA", 50)
	if err != nil {
		return c, err
	}
	c.ElectricityFeeMinNaira = int64(minFee)
	hidden, set := os.LookupEnv("HIDDEN_PLANS")
	if !set {
		hidden = defaultHiddenPlans
	}
	c.HiddenPlans = splitList(hidden)
	if err := c.Pricing().Validate(); err != nil {
		return c, err
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

// defaultHiddenPlans are ABS plans that are not offered, each for a reason that
// could hurt a customer or us (see DESIGN.md): MTN 12.5GB 2-week (ABS charges more
// than the official price), the two Airtel "no credit simcard" deals, the two MTN
// "[postpaid]" plans, and the MTN 15GB deal (dearer than the regular plan).
// HIDDEN_PLANS replaces this list; an empty value hides nothing.
const defaultHiddenPlans = "5GGUX,PPVK8,P77ZY,PKTS6,PD5ZP,2PV3Y"

// Pricing is the pricing rules in the order package's terms.
func (c Config) Pricing() order.Pricing {
	hidden := map[string]bool{}
	for _, id := range c.HiddenPlans {
		hidden[id] = true
	}
	return order.Pricing{
		Markups:               map[string]float64{"mtn": c.MarkupMTN, "airtel": c.MarkupAirtel, "glo": c.MarkupGlo, "9mobile": c.Markup9mobile},
		DefaultMarkup:         c.MarkupDefault,
		LargeFromKobo:         c.MarkupLargeFromNaira * 100,
		LargeMarkups:          map[string]float64{"mtn": c.MarkupMTNLarge},
		Hidden:                hidden,
		ElectricityFeePercent: c.ElectricityFeePercent,
		ElectricityFeeMinKobo: c.ElectricityFeeMinNaira * 100,
	}
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// envFloat reads a number from the environment, or the fallback when unset.
func envFloat(key string, fallback float64) (float64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%s must be a number, 0 or more, got %q", key, raw)
	}
	return v, nil
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
