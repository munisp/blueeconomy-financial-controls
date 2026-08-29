package riskscore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRules() Rules {
	low, mid, high := int64(100_000_000), int64(1_000_000_000), int64(10_000_000_000)
	return Rules{
		ModelVersion: "rules-test-1",
		AmountBands: []AmountBand{
			{MaxMinor: &low, Points: 0},
			{MaxMinor: &mid, Points: 10},
			{MaxMinor: &high, Points: 20},
			{MaxMinor: nil, Points: 35},
		},
		HSPrefixRisk: []HSRisk{
			{Prefix: "03", Points: 15, Reason: "fish"},
			{Prefix: "0306", Points: 25, Reason: "crustaceans"},
			{Prefix: "93", Points: 60, Reason: "arms"},
		},
		CountryRisk:         []CountryRisk{{Country: "NG", Points: 10}, {Country: "AF", Points: 30}},
		SanctionedCountries: []string{"KP"},
		KnownTraderIDs:      []string{"trader-known"},
		NewTraderPoints:     25,
		AEODiscountPoints:   15,
	}
}

func validPayload() ScoreRequest {
	return ScoreRequest{
		DeclarationRef:     "DECL-2026-001",
		DeclarationType:    "IMPORT",
		HSCode:             "03061700",
		CountryOfOrigin:    "NG",
		PortOfEntry:        "NGLOS",
		InvoiceAmountMinor: 250_000_000,
		InvoiceCurrency:    "USD",
		TraderID:           "trader-known",
	}
}

func TestScoreDeterministicBands(t *testing.T) {
	rules := testRules()
	for name, testCase := range map[string]struct {
		mutate func(*ScoreRequest)
		want   int
	}{
		// band 10 + hs 0306 (longest prefix) 25 + origin NG 10 + known trader 0 = 45
		"baseline": {func(*ScoreRequest) {}, 45},
		// low amount band 0 -> 35
		"small amount": {func(r *ScoreRequest) { r.InvoiceAmountMinor = 50_000_000 }, 35},
		// high band 35 (uncapped) -> 70
		"huge amount": {func(r *ScoreRequest) { r.InvoiceAmountMinor = 50_000_000_000 }, 70},
		// new trader +25 -> 70
		"new trader": {func(r *ScoreRequest) { r.TraderID = "trader-new" }, 70},
		// AEO -15 -> 30
		"aeo discount": {func(r *ScoreRequest) { r.IsAEO = true }, 30},
		// destination AF +30 -> 75
		"risky destination": {func(r *ScoreRequest) { r.CountryOfDestination = "AF" }, 75},
	} {
		request := validPayload()
		testCase.mutate(&request)
		first := rules.Score(request)
		second := rules.Score(request)
		if first.Score != testCase.want {
			t.Fatalf("%s score = %d, want %d (reasons %v)", name, first.Score, testCase.want, first.Reasons)
		}
		if first.Score != second.Score || first.ModelVersion != second.ModelVersion {
			t.Fatalf("%s scoring not deterministic", name)
		}
		if first.ModelVersion != "rules-test-1" || !first.RuleBased {
			t.Fatalf("%s missing honest scoring metadata: %+v", name, first)
		}
	}
}

func TestScoreSanctionedFailClosed(t *testing.T) {
	rules := testRules()
	request := validPayload()
	request.CountryOfOrigin = "KP"
	response := rules.Score(request)
	if response.Score != 100 || !response.Sanctioned {
		t.Fatalf("sanctioned origin = %+v, want score 100 sanctioned", response)
	}
	request = validPayload()
	request.CountryOfDestination = "KP"
	if response := rules.Score(request); response.Score != 100 || !response.Sanctioned {
		t.Fatalf("sanctioned destination = %+v, want score 100 sanctioned", response)
	}
}

func TestScoreClamped(t *testing.T) {
	rules := testRules()
	request := validPayload()
	request.HSCode = "93019000"
	request.TraderID = "trader-new"
	request.CountryOfDestination = "AF"
	request.InvoiceAmountMinor = 50_000_000_000
	if response := rules.Score(request); response.Score != 100 {
		t.Fatalf("clamped score = %d, want 100", response.Score)
	}
	request = validPayload()
	request.InvoiceAmountMinor = 1
	request.HSCode = "99999999"
	request.CountryOfOrigin = "IS"
	request.IsAEO = true
	if response := rules.Score(request); response.Score != 0 {
		t.Fatalf("floor score = %d, want 0", response.Score)
	}
}

func TestRequestValidation(t *testing.T) {
	for name, mutate := range map[string]func(*ScoreRequest){
		"empty ref":       func(r *ScoreRequest) { r.DeclarationRef = "" },
		"bad hs":          func(r *ScoreRequest) { r.HSCode = "ABC" },
		"bad origin":      func(r *ScoreRequest) { r.CountryOfOrigin = "NGA" },
		"bad destination": func(r *ScoreRequest) { r.CountryOfDestination = "ng" },
		"negative amount": func(r *ScoreRequest) { r.InvoiceAmountMinor = -1 },
		"bad currency":    func(r *ScoreRequest) { r.InvoiceCurrency = "usd" },
		"negative weight": func(r *ScoreRequest) { r.GrossWeightKg = -1 },
		"missing trader":  func(r *ScoreRequest) { r.TraderID = "" },
	} {
		request := validPayload()
		mutate(&request)
		if err := request.Validate(); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if err := validPayload().Validate(); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	handler, err := NewHandler(testRules())
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler
}

func TestHandlerKnownPayloads(t *testing.T) {
	handler := newTestHandler(t)
	body, err := json.Marshal(validPayload())
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/risk-scores", strings.NewReader(string(body))))
	if recorder.Code != http.StatusOK {
		t.Fatalf("score = %d: %s", recorder.Code, recorder.Body)
	}
	var response ScoreResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Score != 45 || response.ModelVersion != "rules-test-1" || !response.RuleBased {
		t.Fatalf("unexpected verdict: %+v", response)
	}
}

func TestHandlerMalformedPayloads(t *testing.T) {
	handler := newTestHandler(t)
	valid, err := json.Marshal(validPayload())
	if err != nil {
		t.Fatal(err)
	}
	unknownField := strings.Replace(string(valid), `"declaration_ref"`, `"declaration_ref_x"`, 1)
	for name, body := range map[string]string{
		"not json":      `{broken`,
		"two documents": string(valid) + string(valid),
		"unknown field": unknownField,
		"empty ref":     strings.Replace(string(valid), "DECL-2026-001", "", 1),
		"bad hs":        strings.Replace(string(valid), "03061700", "AB", 1),
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/risk-scores", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", name, recorder.Code)
		}
	}
}

func TestLoadRulesFailClosed(t *testing.T) {
	if _, err := LoadRules(""); err == nil {
		t.Fatal("empty path accepted")
	}
	if _, err := LoadRules(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file accepted")
	}
	for name, content := range map[string]string{
		"not json":       `{broken`,
		"no version":     `{"amount_bands":[{"max_minor":null,"points":1}]}`,
		"no bands":       `{"model_version":"v1"}`,
		"unsorted bands": `{"model_version":"v1","amount_bands":[{"max_minor":5,"points":1},{"max_minor":5,"points":2}]}`,
		"mid uncapped":   `{"model_version":"v1","amount_bands":[{"max_minor":null,"points":1},{"max_minor":5,"points":2}]}`,
		"bad prefix":     `{"model_version":"v1","amount_bands":[{"max_minor":null,"points":1}],"hs_prefix_risk":[{"prefix":"AB","points":1}]}`,
		"bad country":    `{"model_version":"v1","amount_bands":[{"max_minor":null,"points":1}],"country_risk":[{"country":"NGA","points":1}]}`,
	} {
		path := filepath.Join(t.TempDir(), "rules.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRules(path); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestShippedRulesValid(t *testing.T) {
	rules, err := LoadRules(filepath.Join("..", "..", "config", "declaration-scorer-rules.json"))
	if err != nil {
		t.Fatalf("shipped rules invalid: %v", err)
	}
	// Every trader scores the new-trader flag with the empty shipped registry;
	// the score stays within the client contract.
	response := rules.Score(validPayload())
	if response.Score < 0 || response.Score > 100 || response.ModelVersion == "" {
		t.Fatalf("shipped rules verdict outside contract: %+v", response)
	}
}
