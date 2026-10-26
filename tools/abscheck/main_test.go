package main

import (
	"strings"
	"testing"
)

// The shape ABS answers with today, as far as the backend reads it.
const goodCatalog = `{"status":true,"text":"Data found","data":{
  "airtimeproviders":[{"name":"MTN Airtime","product_trackid":"MTNAIR6TFD","trackid":"x","allowed_numbers":"0803","sellatwhatpercentoff":"99.00"}],
  "data_providers":[{"name":"MTN Data","product_trackid":"MTN56TFD","allowed_numbers":"0803","bouquetdata":[
    {"trackid":"P1","name":"1GB Daily","sellatwhatpercentoff":"290.00","recipient_type":"phone","validity_type":"Daily","validity":"1 day","validity_days":1},
    {"trackid":"P2","name":"2GB","sellatwhatpercentoff":"490.00","recipient_type":"phone","validity_type":"Daily","validity_days":null,"data_size":"2GB"}
  ]}],
  "electricity_providers":[{"name":"Oyo","servicedata":[{"trackid":"E1","name":"IBEDC","sellatwhatpercentoff":"0","percentoffforagent":"0"}]}]
}}`

func run(t *testing.T, body string) *report {
	t.Helper()
	r := &report{}
	checkCatalog(r, []byte(body))
	return r
}

func TestTheShapeWeExpectPasses(t *testing.T) {
	if r := run(t, goodCatalog); r.broken != 0 {
		t.Fatalf("a good catalog was reported as changed (%d problems)", r.broken)
	}
}

func TestNewFieldsAreOnlyNotes(t *testing.T) {
	body := strings.Replace(goodCatalog, `"validity_days":1}`, `"validity_days":1,"brand_new_field":"x"}`, 1)
	r := run(t, body)
	if r.broken != 0 || r.notes == 0 {
		t.Errorf("a new field should be a note, not a break: broken %d, notes %d", r.broken, r.notes)
	}
}

func TestChangesThatWouldBreakTheBackendAreCaught(t *testing.T) {
	cases := map[string]string{
		"price became a number":  strings.Replace(goodCatalog, `"sellatwhatpercentoff":"290.00"`, `"sellatwhatpercentoff":290`, 1),
		"trackid renamed":        strings.Replace(goodCatalog, `"trackid":"P1"`, `"id":"P1"`, 1),
		"section removed":        strings.Replace(goodCatalog, `"electricity_providers"`, `"electric_providers"`, 1),
		"plans moved":            strings.Replace(goodCatalog, `"bouquetdata"`, `"plans"`, 1),
		"status false":           strings.Replace(goodCatalog, `"status":true`, `"status":false`, 1),
		"validity_days a string": strings.Replace(goodCatalog, `"validity_days":1}`, `"validity_days":"1"}`, 1),
	}
	for name, body := range cases {
		if r := run(t, body); r.broken == 0 {
			t.Errorf("%s was not noticed", name)
		}
	}
	if r := run(t, "<html>maintenance</html>"); r.broken == 0 {
		t.Error("a reply that is not JSON was not noticed")
	}
}
