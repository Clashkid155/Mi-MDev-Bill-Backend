// abscheck asks ABS (read only, nothing is bought or charged) whether its API
// still looks the way this backend expects, and says what changed if not.
//
//	ABS_API_KEY=... go run ./tools/abscheck            # or run it where the server's settings are loaded
//	go run ./tools/abscheck -meter 12345678901 -disco ELEC1     # also checks a meter lookup
//
// It reads ABS_API_KEY and ABS_BASE_URL (default https://absvendor.com) from the
// environment, or from a .env file in the current folder. It makes 3 to 5 calls;
// ABS allows 300 an hour per key. The exit code is 1 when something we depend on
// is missing or has changed type, 0 otherwise (new fields are only reported).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"Bills-Backend/abs"
)

func main() {
	meter := flag.String("meter", "", "optional meter number to check with -disco (read only)")
	disco := flag.String("disco", "", "optional electricity service trackid for -meter")
	flag.Parse()

	base, key := setting("ABS_BASE_URL", "https://absvendor.com"), setting("ABS_API_KEY", "")
	if key == "" {
		fmt.Fprintln(os.Stderr, "ABS_API_KEY is not set (in the environment or a .env file here)")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	r := &report{}
	api := &caller{base: strings.TrimRight(base, "/"), key: key}

	// 1. The catalog: every field the app is built on.
	r.section("Catalog  GET /api/user/system/get_system_data.php")
	status, body, err := api.get(ctx, "/api/user/system/get_system_data.php")
	if r.reply(status, body, err) {
		checkCatalog(r, body)
		client := abs.New(base, key, true)
		if cat, err := client.Catalog(ctx); err != nil {
			r.brokenLine("our own catalog parser fails on the live reply: %v", err)
		} else {
			plans := 0
			for _, n := range cat.Data {
				for _, c := range n.Categories {
					plans += len(c.Plans)
				}
			}
			services := 0
			for _, reg := range cat.Electricity {
				services += len(reg.Services)
			}
			r.ok("our parser reads it: %d airtime networks, %d data networks with %d plans, %d electricity services (%d more left out for charging a fee)",
				len(cat.Airtime), len(cat.Data), plans, services, len(cat.ChargingFees))
		}
	}

	// 2. The wallet balance our low-balance alert and /admin/balances use.
	r.section("Balance  GET /api/user/profile/get_user_detail.php")
	status, body, err = api.get(ctx, "/api/user/profile/get_user_detail.php")
	if r.reply(status, body, err) {
		client := abs.New(base, key, true)
		if kobo, err := client.Balance(ctx); err != nil {
			r.brokenLine("our balance reader fails on the live reply: %v", err)
		} else {
			r.ok("our balance reader works: wallet holds ₦%.2f", float64(kobo)/100)
		}
		// Only the names of the fields: this reply holds personal details.
		var d struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if json.Unmarshal(body, &d) == nil {
			r.info("fields in data: %s", strings.Join(sortedKeys(d.Data), ", "))
		}
	}

	// 3. The lookup used to settle an order left PROCESSING. Only a future feature
	// uses it, so a refusal is a note, but ABS's own reason is shown.
	r.section("Lookup   GET /api/partner/v1/transaction.php?ident=...")
	status, body, err = api.get(ctx, "/api/partner/v1/transaction.php?ident="+url.QueryEscape("ABSCHECK-NOT-AN-ORDER"))
	partnerReply(r, "the order lookup", status, body, err)

	// 4. The partner account endpoint (daily cap, IP and PIN controls); not used by us yet.
	r.section("Partner account  GET /api/partner/v1/user.php (not used by us yet)")
	status, body, err = api.get(ctx, "/api/partner/v1/user.php")
	partnerReply(r, "the partner account", status, body, err)
	if status == http.StatusOK {
		var d map[string]json.RawMessage
		if json.Unmarshal(body, &d) == nil {
			for _, f := range []string{"api_daily_cap", "api_daily_spent", "currency"} {
				if v, ok := d[f]; ok {
					r.info("%s = %s", f, string(v))
				}
			}
		}
	}

	// 5. Optional: a meter lookup (read only).
	if *meter != "" && *disco != "" {
		r.section("Meter    POST /api/user/transaction/validate_meter.php")
		client := abs.New(base, key, true)
		details, err := client.ValidateMeter(ctx, *disco, *meter)
		switch {
		case err != nil:
			r.info("ABS did not accept it: %v (a wrong meter or service id looks like this too)", err)
		default:
			var d map[string]json.RawMessage
			_ = json.Unmarshal(details, &d)
			r.ok("accepted; fields: %s", strings.Join(sortedKeys(d), ", "))
		}
	}

	r.summary()
	if r.broken > 0 {
		os.Exit(1)
	}
}

// setting reads a variable from the environment, then from ./.env.
func setting(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	f, err := os.Open(".env")
	if err != nil {
		return fallback
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == name {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return fallback
}

type caller struct{ base, key string }

func (c *caller) get(ctx context.Context, path string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, body, err
}

// partnerReply reports an answer from the Partner API, which this backend barely
// uses. A refusal is only a note, but ABS's own words are shown because they say
// why (a key without Partner access, or an address that is not allowed).
func partnerReply(r *report, what string, status int, body []byte, err error) {
	if err != nil {
		r.info("%s: no answer: %v", what, err)
		return
	}
	var d map[string]json.RawMessage
	if json.Unmarshal(body, &d) != nil {
		r.info("%s: HTTP %d, not JSON: %s", what, status, shorten(body))
		return
	}
	if status == http.StatusOK {
		r.ok("%s: HTTP 200, fields: %s", what, strings.Join(sortedKeys(d), ", "))
		return
	}
	r.info("%s: HTTP %d, refused. ABS says: %s", what, status, errorDetail(d))
}

// errorDetail is ABS's own code and message from a refusal.
func errorDetail(d map[string]json.RawMessage) string {
	var parts []string
	for _, f := range []string{"code", "message", "text", "error"} {
		if v, ok := d[f]; ok {
			parts = append(parts, f+"="+strings.TrimSpace(string(v)))
		}
	}
	if len(parts) == 0 {
		return "(no message; fields: " + strings.Join(sortedKeys(d), ", ") + ")"
	}
	return strings.Join(parts, " ")
}

// ---- what the catalog must look like ----

// field is one value the backend reads: its allowed JSON types, and whether every
// item must have it.
type field struct {
	types    []string // "string", "number", "null", "array", "object", "bool"
	required bool
}

var (
	str, num, null = "string", "number", "null"

	airtimeFields = map[string]field{
		"name": {[]string{str}, true}, "product_trackid": {[]string{str}, true},
		"allowed_numbers": {[]string{str}, true}, "sellatwhatpercentoff": {[]string{str}, true},
	}
	dataProviderFields = map[string]field{
		"name": {[]string{str}, true}, "product_trackid": {[]string{str}, true},
		"allowed_numbers": {[]string{str}, true}, "bouquetdata": {[]string{"array"}, true},
	}
	bouquetFields = map[string]field{
		"trackid": {[]string{str}, true}, "name": {[]string{str}, true}, "sellatwhatpercentoff": {[]string{str}, true},
		"recipient_type": {[]string{str}, true}, "validity_type": {[]string{str}, true},
		"data_size": {[]string{str, null}, false}, "note": {[]string{str, null}, false},
		"validity": {[]string{str, null}, false}, "validity_days": {[]string{num, null}, false},
	}
	electricityFields = map[string]field{
		"name": {[]string{str}, true}, "servicedata": {[]string{"array"}, true},
	}
	serviceFields = map[string]field{
		"trackid": {[]string{str}, true}, "name": {[]string{str}, true},
		"sellatwhatpercentoff": {[]string{str, num, null}, false}, "percentoffforagent": {[]string{str, num, null}, false},
	}
)

func checkCatalog(r *report, body []byte) {
	var top struct {
		Status json.RawMessage            `json:"status"`
		Data   map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		r.brokenLine("the reply is not JSON: %s", shorten(body))
		return
	}
	if string(top.Status) != "true" {
		r.brokenLine("status is %s, not true", string(top.Status))
	}
	for _, section := range []string{"airtimeproviders", "data_providers", "electricity_providers"} {
		if _, ok := top.Data[section]; !ok {
			r.brokenLine("data.%s is gone", section)
		}
	}
	known := map[string]bool{"airtimeproviders": true, "data_providers": true, "electricity_providers": true}
	for k := range top.Data {
		if !known[k] {
			r.info("section data.%s is not used by us", k)
		}
	}

	compare(r, "airtimeproviders", items(top.Data["airtimeproviders"]), airtimeFields)
	providers := items(top.Data["data_providers"])
	compare(r, "data_providers", providers, dataProviderFields)
	var bouquets []map[string]json.RawMessage
	for _, p := range providers {
		bouquets = append(bouquets, items(p["bouquetdata"])...)
	}
	compare(r, "data_providers[].bouquetdata (the plans)", bouquets, bouquetFields)
	regions := items(top.Data["electricity_providers"])
	compare(r, "electricity_providers", regions, electricityFields)
	var services []map[string]json.RawMessage
	for _, p := range regions {
		services = append(services, items(p["servicedata"])...)
	}
	compare(r, "electricity_providers[].servicedata", services, serviceFields)
}

func items(raw json.RawMessage) []map[string]json.RawMessage {
	var out []map[string]json.RawMessage
	_ = json.Unmarshal(raw, &out)
	return out
}

// compare checks every item of one list against what we expect of it.
func compare(r *report, name string, list []map[string]json.RawMessage, want map[string]field) {
	if len(list) == 0 {
		r.brokenLine("%s: no items", name)
		return
	}
	missing := map[string]int{}
	wrongType := map[string]map[string]int{} // field -> found type -> count
	seen := map[string]int{}
	for _, item := range list {
		for k, raw := range item {
			seen[k]++
			if f, ok := want[k]; ok && !contains(f.types, jsonType(raw)) {
				if wrongType[k] == nil {
					wrongType[k] = map[string]int{}
				}
				wrongType[k][jsonType(raw)]++
			}
		}
		for k, f := range want {
			if _, ok := item[k]; !ok && f.required {
				missing[k]++
			}
		}
	}
	problems := 0
	for _, k := range sortedKeys(missing) {
		r.brokenLine("%s: required field %q is missing in %d of %d items", name, k, missing[k], len(list))
		problems++
	}
	for _, k := range sortedKeys(wrongType) {
		for typ, n := range wrongType[k] {
			r.brokenLine("%s: field %q is %s in %d of %d items; we read %s", name, k, typ, n, len(list), strings.Join(want[k].types, " or "))
			problems++
		}
	}
	var added []string
	for k := range seen {
		if _, ok := want[k]; !ok {
			added = append(added, k)
		}
	}
	sort.Strings(added)
	if len(added) > 0 {
		r.info("%s: fields we do not read: %s", name, strings.Join(added, ", "))
	}
	if problems == 0 {
		r.ok("%s: %d items, every field we use is there with the right type", name, len(list))
	}
}

func jsonType(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	switch {
	case s == "null":
		return null
	case strings.HasPrefix(s, `"`):
		return str
	case strings.HasPrefix(s, "["):
		return "array"
	case strings.HasPrefix(s, "{"):
		return "object"
	case s == "true" || s == "false":
		return "bool"
	}
	return num
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func shorten(b []byte) string {
	if len(b) > 160 {
		return string(b[:160]) + "..."
	}
	return string(b)
}

// ---- the report ----

type report struct {
	broken int
	notes  int
}

func (r *report) section(title string) { fmt.Printf("\n== %s\n", title) }
func (r *report) ok(format string, a ...any) {
	fmt.Printf("  ok       %s\n", fmt.Sprintf(format, a...))
}
func (r *report) info(format string, a ...any) {
	r.notes++
	fmt.Printf("  note     %s\n", fmt.Sprintf(format, a...))
}

func (r *report) brokenLine(format string, a ...any) {
	r.broken++
	fmt.Printf("  CHANGED  %s\n", fmt.Sprintf(format, a...))
}

// reply reports the HTTP outcome and says whether the body is worth reading.
func (r *report) reply(status int, body []byte, err error) bool {
	switch {
	case err != nil:
		r.brokenLine("no answer: %v", err)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		r.brokenLine("HTTP %d: the key was refused (wrong key, or an IP restriction that does not include this machine): %s", status, shorten(body))
	case status == http.StatusNotFound:
		r.brokenLine("HTTP 404: this endpoint no longer exists")
	case status != http.StatusOK:
		r.brokenLine("HTTP %d: %s", status, shorten(body))
	default:
		r.ok("HTTP 200, %d bytes", len(body))
		return true
	}
	return false
}

func (r *report) summary() {
	fmt.Println()
	switch {
	case r.broken > 0:
		fmt.Printf("RESULT: %d thing(s) the backend depends on have CHANGED or are unreachable. See the CHANGED lines above.\n", r.broken)
	default:
		fmt.Printf("RESULT: the ABS API still matches what the backend expects (%d note(s), nothing breaking).\n", r.notes)
	}
}
