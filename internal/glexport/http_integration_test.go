//go:build integration

package glexport

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandlerEndpoints exercises the full HTTP surface against real
// PostgreSQL: bearer enforcement, the maker-checker close flow, statement
// export with replay conflict, integrity-checked payload download, trial
// balance and the reconciliation report.
func TestHandlerEndpoints(t *testing.T) {
	service, store, pool, _, _ := openService(t)
	seedJournal(t, pool)
	handler, err := NewHandler(stubAuthenticator{}, service, store, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	call := func(method, path, token string, body any) (int, map[string]any, http.Header) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			reader = bytes.NewReader(raw)
		}
		request, err := http.NewRequest(method, server.URL+path, reader)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer response.Body.Close()
		var decoded map[string]any
		_ = json.NewDecoder(response.Body).Decode(&decoded)
		return response.StatusCode, decoded, response.Header
	}

	// Unauthenticated calls fail closed.
	if status, _, _ := call("GET", "/v1/gl/trial-balance?period_start=2026-01-01&period_end=2026-01-31", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated trial balance status = %d, want 401", status)
	}
	if status, _, _ := call("GET", "/healthz", "", nil); status != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", status)
	}

	scope := map[string]string{"period_start": "2026-01-01", "period_end": "2026-01-31"}

	// Maker requests the close.
	status, created, _ := call("POST", "/v1/gl/periods/close", "maker-1", scope)
	if status != http.StatusCreated {
		t.Fatalf("close request status = %d (%v)", status, created)
	}
	closeID, _ := created["period_close_id"].(string)
	if closeID == "" || created["status"] != "PENDING_APPROVAL" {
		t.Fatalf("unexpected close response: %v", created)
	}

	// Maker cannot self-approve.
	if status, _, _ := call("POST", "/v1/gl/periods/"+closeID+"/approve", "maker-1", nil); status != http.StatusConflict {
		t.Fatalf("self-approval status = %d, want 409", status)
	}

	// Checker approves.
	status, approved, _ := call("POST", "/v1/gl/periods/"+closeID+"/approve", "checker-1", nil)
	if status != http.StatusOK || approved["status"] != "CLOSED" {
		t.Fatalf("approval status = %d (%v)", status, approved)
	}

	// Trial balance reports the three moved accounts.
	status, balance, _ := call("GET", "/v1/gl/trial-balance?period_start=2026-01-01&period_end=2026-01-31", "reader-1", nil)
	if status != http.StatusOK {
		t.Fatalf("trial balance status = %d", status)
	}
	if rows, ok := balance["trial_balance"].([]any); !ok || len(rows) != 3 {
		t.Fatalf("trial balance rows = %v", balance["trial_balance"])
	}

	// Statement export + replay conflict + integrity-checked download.
	exportBody := map[string]string{
		"account": "TSA:NGN", "currency": "NGN",
		"period_start": "2026-01-01", "period_end": "2026-01-31",
	}
	status, batch, _ := call("POST", "/v1/gl/statements", "exporter-1", exportBody)
	if status != http.StatusCreated {
		t.Fatalf("statement export status = %d (%v)", status, batch)
	}
	batchID, _ := batch["batch_id"].(string)
	if batchID == "" {
		t.Fatalf("missing batch id: %v", batch)
	}
	if status, _, _ := call("POST", "/v1/gl/statements", "exporter-1", exportBody); status != http.StatusConflict {
		t.Fatalf("replay export status = %d, want 409", status)
	}

	request, err := http.NewRequest("GET", server.URL+"/v1/gl/exports/"+batchID, nil)
	if err != nil {
		t.Fatalf("build download request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer reader-1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("download export: %v", err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d", response.StatusCode)
	}
	if response.Header.Get("X-Payload-SHA256") != sha256Hex(payload) {
		t.Fatal("download payload hash header does not match the payload")
	}

	// Payment initiation export over HTTP.
	status, paymentBatch, _ := call("POST", "/v1/gl/payments", "exporter-1", map[string]string{
		"currency": "NGN", "period_start": "2026-01-01", "period_end": "2026-01-31",
	})
	if status != http.StatusCreated || paymentBatch["export_type"] != ExportTypePain001 {
		t.Fatalf("payment export status = %d (%v)", status, paymentBatch)
	}

	// Reconciliation report: drift until every account is exported, then balanced.
	status, run, _ := call("POST", "/v1/gl/reconciliation/runs", "recon-1", scope)
	if status != http.StatusOK || run["balanced"] != false {
		t.Fatalf("first recon run status = %d (%v)", status, run)
	}
	for _, account := range []string{"REVENUE:NGN", "CVFF:EXPENSE:NGN"} {
		if status, resp, _ := call("POST", "/v1/gl/statements", "exporter-1", map[string]string{
			"account": account, "currency": "NGN",
			"period_start": "2026-01-01", "period_end": "2026-01-31",
		}); status != http.StatusCreated {
			t.Fatalf("export %s status = %d (%v)", account, status, resp)
		}
	}
	status, run, _ = call("POST", "/v1/gl/reconciliation/runs", "recon-1", scope)
	if status != http.StatusCreated || run["balanced"] != true {
		t.Fatalf("balanced recon run status = %d (%v)", status, run)
	}

	// The report endpoint lists the persisted runs.
	status, list, _ := call("GET", "/v1/gl/reconciliation/runs?period_start=2026-01-01&period_end=2026-01-31", "reader-1", nil)
	if status != http.StatusOK {
		t.Fatalf("list runs status = %d", status)
	}
	if runs, ok := list["reconciliation_runs"].([]any); !ok || len(runs) != 2 {
		t.Fatalf("reconciliation runs = %v", list["reconciliation_runs"])
	}
}
