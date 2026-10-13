package order

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var receiptTime = time.Date(2026, 9, 28, 11, 6, 19, 0, time.UTC) // 12:06 PM in Lagos

func airtimeOrder() Order {
	return Order{
		ID: "MDEV_abc123", Kind: KindAirtime, NetworkName: "MTN Airtime Nigeria", Recipient: "08012345678",
		AmountKobo: 100_00, CustomerName: "Ada Obi", CustomerEmail: "ada@x.co",
	}
}

func TestFormatNaira(t *testing.T) {
	for kobo, want := range map[int64]string{
		0: "₦0.00", 5: "₦0.05", 100_00: "₦100.00", 123456: "₦1,234.56",
		1000_00: "₦1,000.00", 29000_00: "₦29,000.00", 123456789_00 + 50: "₦123,456,789.50",
	} {
		if got := formatNaira(kobo); got != want {
			t.Errorf("%d kobo: got %s, want %s", kobo, got, want)
		}
	}
}

func TestAirtimeReceipt(t *testing.T) {
	r := buildReceipt(airtimeOrder(), nil, receiptTime)
	page, err := r.html()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Hi Ada Obi", "₦100.00", "MTN Airtime Nigeria", "Phone number", "08012345678", "MDEV_abc123",
		"28 Sep 2026, 12:06 PM WAT", "https://mi-mdev.dizztech.com/support", "@dizzinc", "https://x.com/dizzinc",
		"₦100.00 airtime for 08012345678", // the hidden preview line
	} {
		if !strings.Contains(page, want) {
			t.Errorf("html is missing %q", want)
		}
	}
	if strings.Contains(page, "electricity token") || strings.Contains(page, "Plan") {
		t.Error("an airtime receipt must have no token or plan section")
	}
	text := r.text()
	for _, want := range []string{"Hi Ada Obi,", "₦100.00", "Network: MTN Airtime Nigeria", "Phone number: 08012345678", "Reference: MDEV_abc123", "support"} {
		if !strings.Contains(text, want) {
			t.Errorf("text is missing %q: %s", want, text)
		}
	}
}

func TestDataReceiptShowsThePlanAndFibreXNumbers(t *testing.T) {
	o := airtimeOrder()
	o.Kind, o.NetworkName, o.PlanName, o.AmountKobo = KindData, "MTN Data Nigeria", "500MB HOT DEAL", 300_00
	page, _ := buildReceipt(o, nil, receiptTime).html()
	for _, want := range []string{"500MB HOT DEAL", "MTN Data Nigeria", "Phone number", "₦300.00"} {
		if !strings.Contains(page, want) {
			t.Errorf("html is missing %q", want)
		}
	}

	o.PlanName, o.Recipient = "FibreX 50Mbps Monthly", "FN111700"
	if r := buildReceipt(o, nil, receiptTime); r.RecipientLabel != "FibreX number" {
		t.Errorf("label %q", r.RecipientLabel)
	}
}

// Built from a real ABS electricity reply, including the float noise on "unit".
func TestElectricityReceiptAgainstARealABSReply(t *testing.T) {
	o := airtimeOrder()
	o.Kind, o.NetworkName, o.Recipient, o.AmountKobo = KindElectricity, "AEDC- Abuja Electricity Distribution Company(prepaid)", "45705291404", 1000_00
	delivery := json.RawMessage(`{"token": "5296-7725-2448-4408-1616", "unit": 4.4000000000000003552713678800500929355621337890625}`)

	r := buildReceipt(o, delivery, receiptTime)
	if r.Token != "5296-7725-2448-4408-1616" || r.Units != "4.4" || r.RawDelivery != "" {
		t.Fatalf("token %q units %q raw %q", r.Token, r.Units, r.RawDelivery)
	}
	page, _ := r.html()
	for _, want := range []string{"5296-7725-2448-4408-1616", "Units: 4.4", "Meter number", "45705291404"} {
		if !strings.Contains(page, want) {
			t.Errorf("html is missing %q", want)
		}
	}
	if strings.Contains(page, "4.40000000") {
		t.Error("float noise leaked into the email")
	}
	if text := r.text(); !strings.Contains(text, "5296-7725-2448-4408-1616") || !strings.Contains(text, "Units: 4.4") {
		t.Errorf("text: %s", text)
	}
}

// An unrecognised reply is shown raw rather than dropped.
func TestElectricityReceiptFallsBackToTheRawReply(t *testing.T) {
	o := airtimeOrder()
	o.Kind = KindElectricity
	r := buildReceipt(o, json.RawMessage(`{"pin":"9999"}`), receiptTime)
	if r.Token != "" || r.RawDelivery != `{"pin":"9999"}` {
		t.Fatalf("%+v", r)
	}
	if page, _ := r.html(); !strings.Contains(page, "Your token details") || !strings.Contains(page, "9999") {
		t.Error("raw reply missing from html")
	}
	if !strings.Contains(r.text(), `{"pin":"9999"}`) {
		t.Error("raw reply missing from text")
	}
}

// The wording the user asked for, in both versions of the email.
func TestReceiptWording(t *testing.T) {
	o := airtimeOrder()
	o.Kind = KindElectricity
	r := buildReceipt(o, json.RawMessage(`{"token":"1111-2222","unit":1}`), receiptTime)
	page, _ := r.html()
	for name, body := range map[string]string{"html": page, "text": r.text()} {
		if !strings.Contains(body, "This email can't receive replies. For help, visit our support page.") {
			t.Errorf("%s: no-reply notice is missing", name)
		}
		for _, gone := range []string{"went through", "Anyone who has it", "Keep this token private"} {
			if strings.Contains(body, gone) {
				t.Errorf("%s: should no longer say %q", name, gone)
			}
		}
	}
	if !strings.Contains(page, "your electricity purchase was successful.") {
		t.Error("html: the sentence should read \"... purchase was successful.\"")
	}
}

// The Mi MDev identity: yellow mark on a charcoal header, Sora and Manrope type.
func TestReceiptUsesTheMiMDevIdentity(t *testing.T) {
	page, _ := buildReceipt(airtimeOrder(), nil, receiptTime).html()
	for _, want := range []string{"#1C1C1A", "#E6B24E", "'Sora'", "'Manrope'", "#F2F2F0", "Mi MDev"} {
		if !strings.Contains(page, want) {
			t.Errorf("html is missing brand token %q", want)
		}
	}
	if got := strings.Count(page, `bgcolor="#E6B24E"`); got != 5 {
		t.Errorf("the mark should have 5 bars, found %d", got)
	}
}

// customer_name is typed by the user, so it must never be able to inject markup.
func TestReceiptEscapesWhatTheCustomerTyped(t *testing.T) {
	o := airtimeOrder()
	o.CustomerName = `<script>alert(1)</script>`
	o.NetworkName = `<b>Net</b> & Co`
	page, err := buildReceipt(o, nil, receiptTime).html()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "<script>") || strings.Contains(page, "<b>Net</b>") {
		t.Fatalf("unescaped markup in the email:\n%s", page)
	}
	if !strings.Contains(page, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("escaped name not found")
	}
}

// Orders created before names were stored have none; the row is left out rather than shown empty.
func TestOldOrdersWithoutNamesStillGetAGoodReceipt(t *testing.T) {
	o := airtimeOrder()
	o.NetworkName = ""
	r := buildReceipt(o, nil, receiptTime)
	page, err := r.html()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, ">Network<") || strings.Contains(r.text(), "Network:") {
		t.Error("an empty network row was rendered")
	}
	if !strings.Contains(page, "08012345678") || !strings.Contains(page, "MDEV_abc123") {
		t.Error("the rest of the receipt must still be there")
	}
}
