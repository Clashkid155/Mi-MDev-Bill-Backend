package main

import (
	"os"
	"strings"
	"testing"
)

// load runs LoadConfig in an empty folder (so no .env is read) with only the
// given settings.
func load(t *testing.T, env map[string]string) (Config, error) {
	t.Helper()
	t.Chdir(t.TempDir())
	for _, key := range []string{
		"APP_ENV", "ABS_SANDBOX", "ADMIN_TOKEN", "PAYMENT_PROVIDER", "MIN_PAYMENT_NAIRA",
		"MARKUP_MTN_PERCENT", "MARKUP_MTN_LARGE_PERCENT", "MARKUP_LARGE_FROM_NAIRA", "MARKUP_AIRTEL_PERCENT", "MARKUP_GLO_PERCENT",
		"MARKUP_9MOBILE_PERCENT", "MARKUP_DEFAULT_PERCENT", "HIDDEN_PLANS", "ELECTRICITY_FEE_PERCENT", "ELECTRICITY_FEE_MIN_NAIRA",
		"PAYMENT_FEE_WARN_PERCENT", "ZEPTOMAIL_TOKEN", "MAIL_FROM_EMAIL", "ADMIN_ALERT_EMAIL", "ABS_LOW_BALANCE_NAIRA", "ABS_BALANCE_CHECK_MINUTES",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
	return LoadConfig()
}

func TestPricingDefaultsAreTheAgreedNumbers(t *testing.T) {
	c, err := load(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Pricing()
	if p.Markups["mtn"] != 2.5 || p.LargeMarkups["mtn"] != 2.2 || p.LargeFromKobo != 20_000_00 ||
		p.Markups["airtel"] != 4 || p.Markups["glo"] != 5 || p.Markups["9mobile"] != 5 || p.DefaultMarkup != 4 {
		t.Errorf("markups: %+v", p)
	}
	if p.ElectricityFeePercent != 1.8 || p.ElectricityFeeMinKobo != 50_00 || c.PaymentFeeWarnPercent != 1.7 {
		t.Errorf("electricity %v min %d, warn %v", p.ElectricityFeePercent, p.ElectricityFeeMinKobo, c.PaymentFeeWarnPercent)
	}
	for _, id := range []string{"5GGUX", "PPVK8", "P77ZY", "PKTS6", "PD5ZP", "2PV3Y"} {
		if !p.Hidden[id] {
			t.Errorf("%s should be hidden by default", id)
		}
	}
	if len(p.Hidden) != 6 {
		t.Errorf("hidden: %v", p.Hidden)
	}
}

func TestPricingCanBeChangedFromTheEnvironment(t *testing.T) {
	c, err := load(t, map[string]string{"MARKUP_MTN_PERCENT": "3", "ELECTRICITY_FEE_MIN_NAIRA": "100", "HIDDEN_PLANS": "A1, B2"})
	if err != nil {
		t.Fatal(err)
	}
	p := c.Pricing()
	if p.Markups["mtn"] != 3 || p.ElectricityFeeMinKobo != 100_00 || !p.Hidden["A1"] || !p.Hidden["B2"] || len(p.Hidden) != 2 {
		t.Errorf("%+v", p)
	}
	c, err = load(t, map[string]string{"HIDDEN_PLANS": ""}) // set but empty: hide nothing
	if err != nil || len(c.Pricing().Hidden) != 0 {
		t.Errorf("an empty HIDDEN_PLANS hides nothing, got %v %v", c.Pricing().Hidden, err)
	}
}

func TestTheServerRefusesToStartWithAMarkupThatLosesMoney(t *testing.T) {
	cases := map[string]map[string]string{
		"Glo at 1.5%":             {"MARKUP_GLO_PERCENT": "1.5"},
		"MTN large plans at 1%":   {"MARKUP_MTN_LARGE_PERCENT": "1"},
		"the default at 0":        {"MARKUP_DEFAULT_PERCENT": "0"},
		"an electricity fee 1.2%": {"ELECTRICITY_FEE_PERCENT": "1.2"},
	}
	for name, env := range cases {
		_, err := load(t, env)
		if err == nil || !strings.Contains(err.Error(), "%") {
			t.Errorf("%s should be refused, got %v", name, err)
		}
	}
	if _, err := load(t, map[string]string{"MARKUP_GLO_PERCENT": "abc"}); err == nil {
		t.Error("a markup that is not a number must be refused")
	}
	if _, err := load(t, map[string]string{"MARKUP_GLO_PERCENT": "-3"}); err == nil {
		t.Error("a negative markup must be refused")
	}
}

func TestAlertSettings(t *testing.T) {
	c, err := load(t, nil)
	if err != nil || c.AdminAlertEmail != "" || c.ABSLowBalanceNaira != 0 || c.ABSBalanceCheckMinutes != 15 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	mail := map[string]string{"ZEPTOMAIL_TOKEN": "t", "MAIL_FROM_EMAIL": "orders@mi.app"}
	c, err = load(t, map[string]string{"ADMIN_ALERT_EMAIL": " me@example.com ", "ABS_LOW_BALANCE_NAIRA": "5000", "ABS_BALANCE_CHECK_MINUTES": "30",
		"ZEPTOMAIL_TOKEN": mail["ZEPTOMAIL_TOKEN"], "MAIL_FROM_EMAIL": mail["MAIL_FROM_EMAIL"]})
	if err != nil || c.AdminAlertEmail != "me@example.com" || c.ABSLowBalanceNaira != 5000 || c.ABSBalanceCheckMinutes != 30 {
		t.Fatalf("%+v %v", c, err)
	}
	// An alert address with no way to send mail is a mistake, not a silent no-op.
	if _, err := load(t, map[string]string{"ADMIN_ALERT_EMAIL": "me@example.com"}); err == nil {
		t.Error("ADMIN_ALERT_EMAIL without the email settings must be refused")
	}
	for name, env := range map[string]map[string]string{
		"a check every 0 minutes": {"ABS_BALANCE_CHECK_MINUTES": "0"},
		"a negative threshold":    {"ABS_LOW_BALANCE_NAIRA": "-1"},
		"words as the threshold":  {"ABS_LOW_BALANCE_NAIRA": "lots"},
	} {
		if _, err := load(t, env); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
}
