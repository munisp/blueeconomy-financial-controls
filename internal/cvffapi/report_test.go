package cvffapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvff"
)

const (
	testAuditorSubject = "kc-auditor-001"
	testReportFrom     = "2026-08-01T00:00:00Z"
	testReportTo       = "2026-09-01T00:00:00Z"
)

func newReportHandler(t *testing.T, store *fakeStore, principal Principal, authErr error) http.Handler {
	t.Helper()
	handler, err := NewHandler(store,
		stubAuthenticator{principal: principal, err: authErr},
		&fakeBlobs{puts: map[string][]byte{}}, fakeScanner{}, testLimits(), &fakeStarter{}, &fakeSignaler{}, testPolicyEnforcer(t))
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler
}

func seededReportRow() cvff.DualLedgerReport {
	disbursed, _ := time.Parse(time.RFC3339, "2026-08-15T10:30:00Z")
	return cvff.DualLedgerReport{
		ApplicationID:     "cvff-app-001",
		BeneficiaryID:     testSubject,
		State:             cvff.StateDisbursed,
		FeeNGNMinor:       1_500_000,
		CostUSDMinor:      500_000,
		CostNGNEquivalent: 750_000_000,
		NGNPerUSDMicro:    1_500_000_000,
		FeeTransferID:     "fee-transfer-001",
		CostTransferID:    "cost-transfer-001",
		DisbursedAt:       disbursed,
	}
}

func reportRequest(t *testing.T, query string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/cvff/reports/dual-ledger"+query, nil)
	request.Header.Set("Authorization", testBearer)
	return request
}

func TestDualLedgerReportAuthzMatrix(t *testing.T) {
	store := &fakeStore{report: []cvff.DualLedgerReport{seededReportRow()}}
	query := "?from=" + testReportFrom + "&to=" + testReportTo
	for name, test := range map[string]struct {
		principal Principal
		authErr   error
		want      int
	}{
		"auditor role":            {principal: Principal{Subject: testAuditorSubject, Roles: []string{AuditorRole}}, want: http.StatusOK},
		"auditor and beneficiary": {principal: Principal{Subject: testAuditorSubject, Roles: []string{AuditorRole, BeneficiaryRole}}, want: http.StatusOK},
		"beneficiary only":        {principal: Principal{Subject: testSubject, Roles: []string{BeneficiaryRole}}, want: http.StatusForbidden},
		"no roles":                {principal: Principal{Subject: testSubject}, want: http.StatusForbidden},
		"unauthenticated":         {authErr: ErrUnauthenticated, want: http.StatusUnauthorized},
		"authenticator forbids":   {authErr: ErrForbidden, want: http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			handler := newReportHandler(t, store, test.principal, test.authErr)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, reportRequest(t, query))
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, test.want, recorder.Body)
			}
			if test.want != http.StatusOK {
				if contentType := recorder.Header().Get("Content-Type"); contentType != "application/problem+json" {
					t.Fatalf("rejection content type = %q", contentType)
				}
			}
		})
	}
}

func TestDualLedgerReportJSONShape(t *testing.T) {
	store := &fakeStore{report: []cvff.DualLedgerReport{seededReportRow()}}
	handler := newReportHandler(t, store, Principal{Subject: testAuditorSubject, Roles: []string{AuditorRole}}, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, reportRequest(t, "?from="+testReportFrom+"&to="+testReportTo))
	if recorder.Code != http.StatusOK {
		t.Fatalf("report = %d: %s", recorder.Code, recorder.Body)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("report content type = %q", contentType)
	}
	var rows []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	for _, key := range []string{
		"application_id", "beneficiary_id", "state",
		"fee_ngn_minor", "cost_usd_minor", "cost_ngn_equivalent", "ngn_per_usd_micro",
		"fee_transfer_id", "cost_transfer_id", "disbursed_at",
	} {
		if _, ok := rows[0][key]; !ok {
			t.Fatalf("report row missing %q: %v", key, rows[0])
		}
	}
	if rows[0]["application_id"] != "cvff-app-001" || rows[0]["state"] != "DISBURSED" ||
		rows[0]["fee_ngn_minor"] != 1_500_000.0 || rows[0]["disbursed_at"] != "2026-08-15T10:30:00Z" {
		t.Fatalf("report row mismatch: %v", rows[0])
	}
	// The window must reach the store unchanged (UTC).
	if store.reportFrom.Format(time.RFC3339) != testReportFrom || store.reportTo.Format(time.RFC3339) != testReportTo {
		t.Fatalf("store window = %s..%s", store.reportFrom, store.reportTo)
	}
}

func TestDualLedgerReportCSVShape(t *testing.T) {
	store := &fakeStore{report: []cvff.DualLedgerReport{seededReportRow()}}
	handler := newReportHandler(t, store, Principal{Subject: testAuditorSubject, Roles: []string{AuditorRole}}, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, reportRequest(t, "?from="+testReportFrom+"&to="+testReportTo+"&format=csv"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("csv report = %d: %s", recorder.Code, recorder.Body)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "text/csv" {
		t.Fatalf("csv content type = %q", contentType)
	}
	lines := strings.Split(strings.TrimSpace(recorder.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("csv lines = %d: %q", len(lines), recorder.Body)
	}
	if lines[0] != "application_id,beneficiary_id,state,fee_ngn_minor,cost_usd_minor,cost_ngn_equivalent,ngn_per_usd_micro,fee_transfer_id,cost_transfer_id,disbursed_at" {
		t.Fatalf("csv header = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "cvff-app-001,kc-beneficiary-001,DISBURSED,1500000,500000,750000000,1500000000,fee-transfer-001,cost-transfer-001,") {
		t.Fatalf("csv row = %q", lines[1])
	}
}

func TestDualLedgerReportEmptyIsJSONArray(t *testing.T) {
	store := &fakeStore{report: []cvff.DualLedgerReport{}}
	handler := newReportHandler(t, store, Principal{Subject: testAuditorSubject, Roles: []string{AuditorRole}}, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, reportRequest(t, "?from="+testReportFrom+"&to="+testReportTo))
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("empty report = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestDualLedgerReportInvalidWindowMatrix(t *testing.T) {
	store := &fakeStore{report: []cvff.DualLedgerReport{}}
	handler := newReportHandler(t, store, Principal{Subject: testAuditorSubject, Roles: []string{AuditorRole}}, nil)
	for name, query := range map[string]string{
		"missing window":   "",
		"missing to":       "?from=" + testReportFrom,
		"from not RFC3339": "?from=2026-08-01&to=" + testReportTo,
		"to not RFC3339":   "?from=" + testReportFrom + "&to=tomorrow",
		"inverted":         "?from=" + testReportTo + "&to=" + testReportFrom,
		"empty range":      "?from=" + testReportFrom + "&to=" + testReportFrom,
		"window too wide":  "?from=2025-01-01T00:00:00Z&to=2026-12-31T00:00:00Z",
		"bad format":       "?from=" + testReportFrom + "&to=" + testReportTo + "&format=xml",
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, reportRequest(t, query))
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", recorder.Code, recorder.Body)
			}
			if contentType := recorder.Header().Get("Content-Type"); contentType != "application/problem+json" {
				t.Fatalf("422 content type = %q", contentType)
			}
		})
	}
}

func TestDualLedgerReportStoreFailure(t *testing.T) {
	store := &fakeStore{reportErr: errors.New("postgres down")}
	handler := newReportHandler(t, store, Principal{Subject: testAuditorSubject, Roles: []string{AuditorRole}}, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, reportRequest(t, "?from="+testReportFrom+"&to="+testReportTo))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
}
