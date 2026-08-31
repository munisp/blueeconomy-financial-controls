// Package riskscore implements the declaration risk-scoring service behind
// POST /v1/risk-scores (the DECLARATIONS_SCORER_URL provider consumed by
// blueeconomy-port-interoperability). Scoring is deterministic and
// rules-based only: amount bands, an HS-prefix risk table, consignee/origin
// country risk and new-trader flags, all shipped as versioned config data.
// There is no ML model and no fabricated ML claim: every response carries
// model_version and rule_based:true. Configuration is fail-closed — an
// unreadable or invalid rules file is a startup error, never a default.
package riskscore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Rules is the versioned, deterministic scoring configuration.
type Rules struct {
	ModelVersion string `json:"model_version"`
	// AmountBands award the points of the first band whose ceiling covers
	// the invoice amount; the final band may have a null ceiling (uncapped).
	AmountBands []AmountBand `json:"amount_bands"`
	// HSPrefixRisk awards the points of the longest matching HS-code prefix.
	HSPrefixRisk []HSRisk `json:"hs_prefix_risk"`
	// CountryRisk awards points per origin/destination country.
	CountryRisk []CountryRisk `json:"country_risk"`
	// SanctionedCountries force score 100 with sanctioned:true (fail-closed
	// lane handling is the caller's; the score is the maximum signal).
	SanctionedCountries []string `json:"sanctioned_countries"`
	// KnownTraderIDs is the approved trader registry; any other trader_id
	// scores the new-trader flag.
	KnownTraderIDs    []string `json:"known_trader_ids"`
	NewTraderPoints   int      `json:"new_trader_points"`
	AEODiscountPoints int      `json:"aeo_discount_points"`
}

// AmountBand is one invoice-amount risk band (minor units).
type AmountBand struct {
	MaxMinor *int64 `json:"max_minor"`
	Points   int    `json:"points"`
}

// HSRisk maps one HS-code prefix to risk points.
type HSRisk struct {
	Prefix string `json:"prefix"`
	Points int    `json:"points"`
	Reason string `json:"reason"`
}

// CountryRisk maps one ISO 3166-1 alpha-2 country to risk points.
type CountryRisk struct {
	Country string `json:"country"`
	Points  int    `json:"points"`
}

var (
	countryPattern  = regexp.MustCompile(`^[A-Z]{2}$`)
	hsPrefixPattern = regexp.MustCompile(`^[0-9]{2,6}$`)
	versionPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
)

// LoadRules reads and validates the rules file, failing closed on any gap.
func LoadRules(path string) (Rules, error) {
	if strings.TrimSpace(path) != path || path == "" {
		return Rules{}, errors.New("rules path is required and must be canonical")
	}
	content, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Rules{}, fmt.Errorf("read rules: %w", err)
	}
	var rules Rules
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rules); err != nil {
		return Rules{}, fmt.Errorf("decode rules: %w", err)
	}
	if err := rules.Validate(); err != nil {
		return Rules{}, err
	}
	return rules, nil
}

// Validate enforces the fail-closed config contract.
func (rules Rules) Validate() error {
	if !versionPattern.MatchString(rules.ModelVersion) {
		return errors.New("model_version is required and must be canonical")
	}
	if len(rules.AmountBands) == 0 {
		return errors.New("at least one amount band is required")
	}
	previousCeiling := int64(-1)
	for index, band := range rules.AmountBands {
		if band.Points < 0 || band.Points > 100 {
			return fmt.Errorf("amount band %d points outside [0,100]", index)
		}
		if band.MaxMinor == nil {
			if index != len(rules.AmountBands)-1 {
				return errors.New("only the final amount band may be uncapped")
			}
			continue
		}
		if *band.MaxMinor <= previousCeiling {
			return errors.New("amount bands must be strictly ascending")
		}
		previousCeiling = *band.MaxMinor
	}
	seenPrefixes := map[string]bool{}
	for _, risk := range rules.HSPrefixRisk {
		if !hsPrefixPattern.MatchString(risk.Prefix) {
			return fmt.Errorf("hs prefix %q is not 2-6 digits", risk.Prefix)
		}
		if seenPrefixes[risk.Prefix] {
			return fmt.Errorf("duplicate hs prefix %q", risk.Prefix)
		}
		seenPrefixes[risk.Prefix] = true
		if risk.Points < 0 || risk.Points > 100 {
			return fmt.Errorf("hs prefix %q points outside [0,100]", risk.Prefix)
		}
	}
	seenCountries := map[string]bool{}
	for _, risk := range rules.CountryRisk {
		if !countryPattern.MatchString(risk.Country) {
			return fmt.Errorf("country %q is not ISO 3166-1 alpha-2", risk.Country)
		}
		if seenCountries[risk.Country] {
			return fmt.Errorf("duplicate country %q", risk.Country)
		}
		seenCountries[risk.Country] = true
		if risk.Points < 0 || risk.Points > 100 {
			return fmt.Errorf("country %q points outside [0,100]", risk.Country)
		}
	}
	for _, country := range rules.SanctionedCountries {
		if !countryPattern.MatchString(country) {
			return fmt.Errorf("sanctioned country %q is not ISO 3166-1 alpha-2", country)
		}
	}
	if rules.NewTraderPoints < 0 || rules.NewTraderPoints > 100 {
		return errors.New("new_trader_points outside [0,100]")
	}
	if rules.AEODiscountPoints < 0 || rules.AEODiscountPoints > 100 {
		return errors.New("aeo_discount_points outside [0,100]")
	}
	return nil
}

// ScoreRequest mirrors the blueeconomy-port-interoperability scorer client
// contract exactly.
type ScoreRequest struct {
	DeclarationRef       string `json:"declaration_ref"`
	DeclarationType      string `json:"declaration_type"`
	HSCode               string `json:"hs_code"`
	GoodsDescription     string `json:"goods_description"`
	CountryOfOrigin      string `json:"country_of_origin"`
	CountryOfDestination string `json:"country_of_destination,omitempty"`
	PortOfEntry          string `json:"port_of_entry"`
	GrossWeightKg        int64  `json:"gross_weight_kg"`
	NumberOfPackages     int    `json:"number_of_packages"`
	InvoiceAmountMinor   int64  `json:"invoice_amount_minor"`
	InvoiceCurrency      string `json:"invoice_currency"`
	ConsigneeID          string `json:"consignee_id"`
	OperatorID           string `json:"operator_id"`
	TraderID             string `json:"trader_id"`
	IsAEO                bool   `json:"is_aeo"`
}

var (
	hsCodePattern    = regexp.MustCompile(`^[0-9]{2,10}$`)
	currencyPattern  = regexp.MustCompile(`^[A-Z]{3}$`)
	referencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
)

// Validate enforces the request contract; violations are HTTP 400.
func (request ScoreRequest) Validate() error {
	if !referencePattern.MatchString(request.DeclarationRef) {
		return errors.New("declaration_ref is required and must be canonical")
	}
	if !hsCodePattern.MatchString(request.HSCode) {
		return errors.New("hs_code must be 2-10 digits")
	}
	if !countryPattern.MatchString(request.CountryOfOrigin) {
		return errors.New("country_of_origin must be ISO 3166-1 alpha-2")
	}
	if request.CountryOfDestination != "" && !countryPattern.MatchString(request.CountryOfDestination) {
		return errors.New("country_of_destination must be ISO 3166-1 alpha-2")
	}
	if request.InvoiceAmountMinor < 0 {
		return errors.New("invoice_amount_minor must not be negative")
	}
	if !currencyPattern.MatchString(request.InvoiceCurrency) {
		return errors.New("invoice_currency must be an ISO 4217 upper-case code")
	}
	if request.GrossWeightKg < 0 || request.NumberOfPackages < 0 {
		return errors.New("gross_weight_kg and number_of_packages must not be negative")
	}
	if strings.TrimSpace(request.TraderID) == "" {
		return errors.New("trader_id is required")
	}
	return nil
}

// ScoreResponse is the verdict contract. The client validates score in
// [0,100] and a non-empty model_version; rule_based and reasons are honest
// scoring metadata (no ML is claimed or used).
type ScoreResponse struct {
	Score        int      `json:"score"`
	ModelVersion string   `json:"model_version"`
	RuleBased    bool     `json:"rule_based"`
	Sanctioned   bool     `json:"sanctioned,omitempty"`
	Reasons      []string `json:"reasons"`
}

// Score evaluates the request deterministically against the rules.
func (rules Rules) Score(request ScoreRequest) ScoreResponse {
	response := ScoreResponse{ModelVersion: rules.ModelVersion, RuleBased: true, Reasons: []string{}}
	sanctioned := map[string]bool{}
	for _, country := range rules.SanctionedCountries {
		sanctioned[country] = true
	}
	if sanctioned[request.CountryOfOrigin] || (request.CountryOfDestination != "" && sanctioned[request.CountryOfDestination]) {
		response.Score = 100
		response.Sanctioned = true
		response.Reasons = append(response.Reasons, "sanctioned country involved; maximum score (fail-closed)")
		return response
	}
	score := 0
	for _, band := range rules.AmountBands {
		if band.MaxMinor == nil || request.InvoiceAmountMinor <= *band.MaxMinor {
			if band.Points > 0 {
				response.Reasons = append(response.Reasons, fmt.Sprintf("invoice amount band +%d", band.Points))
			}
			score += band.Points
			break
		}
	}
	bestPrefix := ""
	bestPoints := 0
	bestReason := ""
	for _, risk := range rules.HSPrefixRisk {
		if strings.HasPrefix(request.HSCode, risk.Prefix) && len(risk.Prefix) > len(bestPrefix) {
			bestPrefix, bestPoints, bestReason = risk.Prefix, risk.Points, risk.Reason
		}
	}
	if bestPrefix != "" && bestPoints > 0 {
		score += bestPoints
		response.Reasons = append(response.Reasons, fmt.Sprintf("hs prefix %s (%s) +%d", bestPrefix, bestReason, bestPoints))
	}
	countryPoints := map[string]int{}
	for _, risk := range rules.CountryRisk {
		countryPoints[risk.Country] = risk.Points
	}
	if points := countryPoints[request.CountryOfOrigin]; points > 0 {
		score += points
		response.Reasons = append(response.Reasons, fmt.Sprintf("country of origin %s +%d", request.CountryOfOrigin, points))
	}
	if request.CountryOfDestination != "" {
		if points := countryPoints[request.CountryOfDestination]; points > 0 {
			score += points
			response.Reasons = append(response.Reasons, fmt.Sprintf("country of destination %s +%d", request.CountryOfDestination, points))
		}
	}
	known := map[string]bool{}
	for _, traderID := range rules.KnownTraderIDs {
		known[traderID] = true
	}
	if !known[request.TraderID] {
		score += rules.NewTraderPoints
		response.Reasons = append(response.Reasons, fmt.Sprintf("new trader +%d", rules.NewTraderPoints))
	}
	if request.IsAEO {
		score -= rules.AEODiscountPoints
		response.Reasons = append(response.Reasons, fmt.Sprintf("authorised economic operator -%d", rules.AEODiscountPoints))
	}
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	response.Score = score
	return response
}
