package abs

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Catalog is what the app is shown: only things it can actually buy, and only
// the fields it needs. ABS's own catalog carries much more (points, agent
// prices, countries, states, ...) which is deliberately left out.
type Catalog struct {
	Airtime     []AirtimeNetwork    `json:"airtime"`
	Data        []DataNetwork       `json:"data"`
	Electricity []ElectricityRegion `json:"electricity"`
	// Recipients describes each kind of recipient a plan can be bought for, so
	// the app can label and check the input field. Plans refer to it by name.
	Recipients map[string]RecipientFormat `json:"recipients"`

	// ChargingFees names the electricity services left out because ABS adds a fee
	// on top of the amount (see hasFee). Not sent to the app; for the logs.
	ChargingFees []string `json:"-"`
}

type RecipientFormat struct {
	Label   string `json:"label"`
	Example string `json:"example"`
	Pattern string `json:"pattern"` // regular expression the value must match
}

var recipientFormats = map[string]RecipientFormat{
	"phone": {Label: "Phone number", Example: "08012345678", Pattern: `^0\d{10}$`},
	"fn":    {Label: "FibreX FN", Example: "FN111700", Pattern: `^FN\d{6}$`},
}

var recipientPatterns = func() map[string]*regexp.Regexp {
	patterns := map[string]*regexp.Regexp{}
	for kind, f := range recipientFormats {
		patterns[kind] = regexp.MustCompile(f.Pattern)
	}
	return patterns
}()

// ValidRecipient reports whether value looks right for the recipient kind
// ("phone" or "fn"). Unknown kinds are never valid.
func ValidRecipient(kind, value string) bool {
	pattern, ok := recipientPatterns[kind]
	return ok && pattern.MatchString(value)
}

type AirtimeNetwork struct {
	ID             string   `json:"id"` // sent as "network" when ordering airtime
	Name           string   `json:"name"`
	NumberPrefixes []string `json:"number_prefixes"` // phone prefixes that belong to this network

	// CostPercent is what ABS charges us per 100 of airtime (99 means a 1%
	// discount). Never sent to the app; it is for working out profit.
	CostPercent float64 `json:"-"`
}

type DataNetwork struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	NumberPrefixes []string       `json:"number_prefixes"`
	Categories     []DataCategory `json:"categories"`
}

// DataCategory is a group of plans, e.g. "Daily", "FibreX" or "ODU". Categories
// and the plans inside them are already in display order.
type DataCategory struct {
	Name  string     `json:"name"`
	Plans []DataPlan `json:"plans"`
}

type DataPlan struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Size         string `json:"size,omitempty"`
	Validity     string `json:"validity,omitempty"`
	ValidityDays *int   `json:"validity_days,omitempty"`
	// RecipientType says what the plan is bought for: "phone" or "fn" (a FibreX
	// FN such as FN111700). See Catalog.Recipients.
	RecipientType string `json:"recipient_type"`
	PriceKobo     int64  `json:"price_kobo"`
	Note          string `json:"note,omitempty"`

	// CostKobo is what ABS charges us for the plan. PriceKobo starts out equal
	// to it; the order package marks it up before the app sees the plan. Never
	// sent to the app.
	CostKobo int64 `json:"-"`
}

type ElectricityRegion struct {
	Name     string               `json:"name"`
	Services []ElectricityService `json:"services"`
}

type ElectricityService struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"` // "prepaid" or "postpaid" when the name says so
}

// Entries in ABS's electricity list that are not electricity companies
// (telcos and TV providers). They are left out until it is known what buying
// through them does.
var notElectricity = map[string]bool{
	"MTN": true, "GLO": true, "9MOBILE": true, "AIRTEL": true,
	"DSTV": true, "GOTV": true, "STARTIMES": true,
}

const catalogTTL = 5 * time.Minute

// Catalog returns the cleaned catalog. ABS's catalog is large and ABS limits a
// key to 300 requests an hour, so it is cached for a few minutes; if ABS cannot
// be reached, the last good copy is served.
func (c *Client) Catalog(ctx context.Context) (Catalog, error) {
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	if c.catalog != nil && time.Since(c.catalogAt) < catalogTTL {
		return *c.catalog, nil
	}
	raw, err := c.SystemData(ctx)
	if err == nil {
		var fresh Catalog
		if fresh, err = parseCatalog(raw); err == nil {
			c.catalog, c.catalogAt = &fresh, time.Now()
			if c.Log != nil && len(fresh.ChargingFees) > 0 {
				c.Log.Info("Catalog - ABS", "detail", fmt.Sprintf("left out %d electricity services that charge a fee: %s",
					len(fresh.ChargingFees), strings.Join(fresh.ChargingFees, ", ")))
			}
			return fresh, nil
		}
	}
	if c.catalog != nil {
		if c.Log != nil {
			c.Log.Warn("Catalog - ABS", "detail", "serving the cached catalog because a refresh failed: "+err.Error())
		}
		return *c.catalog, nil
	}
	return Catalog{}, err
}

// parseCatalog turns ABS's system data into the app-facing Catalog.
func parseCatalog(raw json.RawMessage) (Catalog, error) {
	var in struct {
		AirtimeProviders []struct {
			Name           string `json:"name"`
			ProductTrackID string `json:"product_trackid"`
			AllowedNumbers string `json:"allowed_numbers"`
			Price          string `json:"sellatwhatpercentoff"` // naira charged per 100 naira of airtime
		} `json:"airtimeproviders"`
		DataProviders []struct {
			Name           string `json:"name"`
			ProductTrackID string `json:"product_trackid"`
			AllowedNumbers string `json:"allowed_numbers"`
			Bouquets       []struct {
				TrackID       string `json:"trackid"`
				Name          string `json:"name"`
				DataSize      string `json:"data_size"`
				Note          string `json:"note"`
				Price         string `json:"sellatwhatpercentoff"`
				RecipientType string `json:"recipient_type"`
				ValidityType  string `json:"validity_type"`
				Validity      string `json:"validity"`
				ValidityDays  *int   `json:"validity_days"`
			} `json:"bouquetdata"`
		} `json:"data_providers"`
		ElectricityProviders []struct {
			Name     string `json:"name"`
			Services []struct {
				TrackID string `json:"trackid"`
				Name    string `json:"name"`
				// ABS does not document these. On electricity they are 0 for most DISCOs
				// and 50, 100 or 150 for a few, and a purchase through one of those took
				// 1100 from the wallet for a 1000 meter top-up: a fee the app never shows.
				Fee      json.RawMessage `json:"sellatwhatpercentoff"`
				AgentFee json.RawMessage `json:"percentoffforagent"`
			} `json:"servicedata"`
		} `json:"electricity_providers"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Catalog{}, fmt.Errorf("abs: could not read the catalog: %w", err)
	}

	out := Catalog{
		Airtime:     []AirtimeNetwork{},
		Data:        []DataNetwork{},
		Electricity: []ElectricityRegion{},
		Recipients:  map[string]RecipientFormat{"phone": recipientFormats["phone"]}, // airtime is always bought for a phone
	}

	for _, p := range in.AirtimeProviders {
		out.Airtime = append(out.Airtime, AirtimeNetwork{
			ID: p.ProductTrackID, Name: p.Name, NumberPrefixes: splitPrefixes(p.AllowedNumbers), CostPercent: airtimeCostPercent(p.Price),
		})
	}

	for _, p := range in.DataProviders {
		var plans []categorizedPlan
		for _, b := range p.Bouquets {
			price, ok := nairaToKobo(b.Price)
			if !ok {
				return Catalog{}, fmt.Errorf("abs: data plan %s has an unreadable price %q", b.TrackID, b.Price)
			}
			plans = append(plans, categorizedPlan{dataCategory(b.Name, b.ValidityType, b.DataSize, b.RecipientType), DataPlan{
				ID: b.TrackID, Name: b.Name, Size: b.DataSize, Validity: b.Validity, ValidityDays: b.ValidityDays,
				RecipientType: b.RecipientType, PriceKobo: price, CostKobo: price, Note: b.Note,
			}})
			if format, known := recipientFormats[b.RecipientType]; known {
				out.Recipients[b.RecipientType] = format
			}
		}
		if len(plans) > 0 {
			out.Data = append(out.Data, DataNetwork{
				ID: p.ProductTrackID, Name: p.Name, NumberPrefixes: splitPrefixes(p.AllowedNumbers), Categories: groupPlans(plans),
			})
		}
	}

	for _, r := range in.ElectricityProviders {
		if notElectricity[strings.ToUpper(r.Name)] {
			continue
		}
		region := ElectricityRegion{Name: r.Name, Services: []ElectricityService{}}
		for _, s := range r.Services {
			if hasFee(s.Fee) || hasFee(s.AgentFee) {
				out.ChargingFees = append(out.ChargingFees, r.Name+" "+s.Name)
				continue
			}
			region.Services = append(region.Services, ElectricityService{ID: s.TrackID, Name: s.Name, Kind: electricityKind(s.Name)})
		}
		if len(region.Services) > 0 {
			out.Electricity = append(out.Electricity, region)
		}
	}

	return out, nil
}

// categoryOrder is the order categories are shown in: short validity first,
// then router, then the two unlimited-broadband kinds. Categories ABS adds
// later come after these.
var categoryOrder = map[string]int{
	"Deals": 0, "Daily": 1, "Weekly": 2, "Monthly": 3, "Yearly": 4, "Social Media": 5, "Router": 6, "FibreX": 7, "ODU": 8,
}

// dealWord matches the words ABS puts in the name of its promotional plans. ABS
// has no flag for them, so the name is all there is; a renamed deal simply
// shows up under its normal category again.
var dealWord = regexp.MustCompile(`(?i)\b(?:hot deal|cheap offer|best deal)\b`)

// odUWord matches "ODU" as a whole word (case-insensitive), e.g. in
// "50MBPS ODU" or "50MBPS ROUTER UNLIMITED [ODU ONLY]- 30days", without
// matching it as a substring of an unrelated word.
var odUWord = regexp.MustCompile(`(?i)\bODU\b`)

// fibreXWord matches "FibreX" as a whole word (case-insensitive), tolerating
// ABS's own "FirbeX" typo (seen on one real plan) by matching either spelling.
var fibreXWord = regexp.MustCompile(`(?i)\bFi(?:br|rb)eX\b`)

// dataCategory works out which display category a data plan belongs in.
// Several ABS quirks make this more than just reading validity_type:
//
//  1. ABS labels genuinely unlimited plans (MTN's own "ODU" plans, and
//     Airtel's "Unlimited N Mbps" plans) with validity_type "FibreX / ODU",
//     lumping them together with FibreX. recipient_type tells them apart:
//     "fn" (a FibreX FN) is FibreX, "phone" is ODU.
//  2. Some of MTN's own ODU plans do NOT use that validity_type at all — they
//     are labelled plainly "Monthly", and are only identifiable by "ODU"
//     appearing in data_size (e.g. "50MBPS ODU").
//  3. Some of Airtel's ODU plans go a step further: neither validity_type nor
//     data_size says "ODU" — only the plan's name does (e.g. data_size
//     "50MBPS", name "50MBPS ROUTER UNLIMITED [ODU ONLY]- 30days"). So name is
//     checked too, not just data_size.
//
// The name/data_size checks run first, regardless of validity_type, so none of
// these are left buried inside the ordinary Monthly/Weekly categories. Next,
// ABS's promotional plans ("HOT DEAL", "CHEAP OFFER", "BEST DEAL" in the name)
// are gathered into their own "Deals" category.
func dataCategory(name, validityType, dataSize, recipientType string) string {
	if odUWord.MatchString(dataSize) || odUWord.MatchString(name) {
		return "ODU"
	}
	if dealWord.MatchString(name) {
		return "Deals"
	}
	if validityType != "FibreX / ODU" {
		return validityType
	}
	if recipientType == "fn" || fibreXWord.MatchString(dataSize) || fibreXWord.MatchString(name) {
		return "FibreX"
	}
	return "ODU"
}

type categorizedPlan struct {
	category string
	plan     DataPlan
}

// groupPlans groups plans by category, in display order, with each category's
// plans sorted by how long they last and then by price (cheapest first).
func groupPlans(entries []categorizedPlan) []DataCategory {
	byName := map[string][]DataPlan{}
	for _, e := range entries {
		name := e.category
		if name == "" {
			name = "Other"
		}
		byName[name] = append(byName[name], e.plan)
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	rank := func(name string) int {
		if r, ok := categoryOrder[name]; ok {
			return r
		}
		return len(categoryOrder)
	}
	sort.Slice(names, func(i, j int) bool {
		if ri, rj := rank(names[i]), rank(names[j]); ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})

	categories := make([]DataCategory, 0, len(names))
	for _, name := range names {
		plans := byName[name]
		sortPlans(plans)
		categories = append(categories, DataCategory{Name: name, Plans: plans})
	}
	return categories
}

// sortPlans orders plans by duration, then price (cheapest first), then name.
// Plans with no fixed duration come last.
func sortPlans(plans []DataPlan) {
	sort.SliceStable(plans, func(i, j int) bool {
		a, b := plans[i], plans[j]
		if (a.ValidityDays == nil) != (b.ValidityDays == nil) {
			return b.ValidityDays == nil
		}
		if a.ValidityDays != nil && *a.ValidityDays != *b.ValidityDays {
			return *a.ValidityDays < *b.ValidityDays
		}
		if a.PriceKobo != b.PriceKobo {
			return a.PriceKobo < b.PriceKobo
		}
		return a.Name < b.Name
	})
}

func splitPrefixes(csv string) []string {
	prefixes := []string{}
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			prefixes = append(prefixes, p)
		}
	}
	return prefixes
}

// airtimeCostPercent reads what ABS charges per 100 of airtime ("99.00"). An
// unreadable or absurd value counts as 100, no discount, so that profit is never
// overstated.
func airtimeCostPercent(raw string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || v < 50 || v > 120 {
		return 100
	}
	return v
}

func nairaToKobo(naira string) (int64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(naira), 64)
	if err != nil || value < 0 {
		return 0, false
	}
	return int64(math.Round(value * 100)), true
}

// hasFee reports whether an electricity service's fee field says ABS charges
// extra. Missing, empty or zero means no fee; a number above zero is a fee, and
// so is anything unreadable, since a fee we cannot see is the case to avoid.
func hasFee(raw json.RawMessage) bool {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" || text == "null" {
		return false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	return err != nil || value != 0
}

func electricityKind(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "(prepaid)"):
		return "prepaid"
	case strings.Contains(lower, "(postpaid)"):
		return "postpaid"
	}
	return ""
}

// The Find methods look an id up in the catalog. Orders use them so the app can
// only buy things ABS is currently selling, at the price in the catalog.

func (c Catalog) FindAirtimeNetwork(id string) (AirtimeNetwork, bool) {
	for _, n := range c.Airtime {
		if n.ID == id {
			return n, true
		}
	}
	return AirtimeNetwork{}, false
}

func (c Catalog) FindDataPlan(planID string) (DataNetwork, DataPlan, bool) {
	for _, network := range c.Data {
		for _, category := range network.Categories {
			for _, plan := range category.Plans {
				if plan.ID == planID {
					return network, plan, true
				}
			}
		}
	}
	return DataNetwork{}, DataPlan{}, false
}

func (c Catalog) FindElectricityService(serviceID string) (ElectricityService, bool) {
	for _, region := range c.Electricity {
		for _, service := range region.Services {
			if service.ID == serviceID {
				return service, true
			}
		}
	}
	return ElectricityService{}, false
}
