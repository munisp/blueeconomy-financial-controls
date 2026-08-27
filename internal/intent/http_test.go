package intent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

type fakeAPIStore struct {
	createErr  error
	approveErr error
	created    Intent
	approved   Intent
}

func (store *fakeAPIStore) Create(_ context.Context, request CreateRequest) (Intent, error) {
	if store.createErr != nil {
		return Intent{}, store.createErr
	}
	return Intent{CreateRequest: request, State: StateDraft, Version: 1}, nil
}

func (store *fakeAPIStore) Approve(_ context.Context, intentID string, expectedVersion int64, checker string) (Intent, error) {
	if store.approveErr != nil {
		return Intent{}, store.approveErr
	}
	return Intent{CreateRequest: validRequest(), State: StateApproved, Checker: &checker, Version: expectedVersion + 1}, nil
}

func newTestHandler(t *testing.T, store APIStore) http.Handler {
	t.Helper()
	pipeline, err := telemetry.Setup(context.Background(), telemetry.Config{ServiceName: "intent-api-test"})
	if err != nil {
		t.Fatalf("disabled telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = pipeline.Shutdown(context.Background()) })
	handler, err := NewHandler(store, pipeline)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler
}

func TestHandlerRequiresStore(t *testing.T) {
	if _, err := NewHandler(nil, nil); err == nil {
		t.Fatal("nil store accepted")
	}
}

func TestHandlerFailsClosedWithoutTelemetry(t *testing.T) {
	if _, err := NewHandler(&fakeAPIStore{}, nil); err == nil {
		t.Fatal("nil telemetry pipeline accepted")
	}
}

func TestReadyzFailsClosedWithoutPingCapableStore(t *testing.T) {
	recorder := httptest.NewRecorder()
	newTestHandler(t, &fakeAPIStore{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz must fail closed with 503 for a store without Ping, got %d", recorder.Code)
	}
}

func TestMetricsEndpointServed(t *testing.T) {
	recorder := httptest.NewRecorder()
	newTestHandler(t, &fakeAPIStore{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /metrics must serve 200, got %d", recorder.Code)
	}
}

func TestHealthz(t *testing.T) {
	recorder := httptest.NewRecorder()
	newTestHandler(t, &fakeAPIStore{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz = %d", recorder.Code)
	}
}

func TestCreateIntentContract(t *testing.T) {
	body := `{"intent_id":"intent-001","external_ref":"ref-001","debit_account_id":"0000000000000000000000000000000b","credit_account_id":"00000000000000000000000000000016","amount":1000,"ledger":1,"code":1,"currency":"NGN","maker":"maker-001"}`
	recorder := httptest.NewRecorder()
	newTestHandler(t, &fakeAPIStore{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/financial-intents", strings.NewReader(body)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", recorder.Code, recorder.Body)
	}
}

func TestCreateIntentValidationFailures(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{})
	for name, body := range map[string]string{
		"malformed JSON":      `{broken`,
		"unknown field":       `{"intent_id":"x","surprise":1}`,
		"unapproved currency": `{"intent_id":"intent-001","external_ref":"ref-001","debit_account_id":"0000000000000000000000000000000b","credit_account_id":"00000000000000000000000000000016","amount":1000,"ledger":1,"code":1,"currency":"EUR","maker":"maker-001"}`,
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/financial-intents", strings.NewReader(body)))
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s = %d, want 422", name, recorder.Code)
		}
	}
}

func TestCreateIntentIdempotencyConflict(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{createErr: ErrImmutableConflict})
	body := `{"intent_id":"intent-001","external_ref":"ref-001","debit_account_id":"0000000000000000000000000000000b","credit_account_id":"00000000000000000000000000000016","amount":1000,"ledger":1,"code":1,"currency":"NGN","maker":"maker-001"}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/financial-intents", strings.NewReader(body)))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("conflict = %d, want 409", recorder.Code)
	}
}

func TestApproveIntentContract(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/financial-intents/intent-001/approve", strings.NewReader(`{"expected_version":1,"checker":"checker-001"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", recorder.Code, recorder.Body)
	}
}

func TestApproveIntentConflicts(t *testing.T) {
	for name, storeErr := range map[string]error{
		"maker-checker": ErrMakerChecker,
		"version":       ErrConflict,
		"state":         ErrInvalidState,
	} {
		handler := newTestHandler(t, &fakeAPIStore{approveErr: storeErr})
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/financial-intents/intent-001/approve", strings.NewReader(`{"expected_version":1,"checker":"checker-001"}`)))
		if recorder.Code != http.StatusConflict {
			t.Fatalf("%s = %d, want 409", name, recorder.Code)
		}
	}
	handler := newTestHandler(t, &fakeAPIStore{approveErr: ErrNotFound})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/financial-intents/intent-001/approve", strings.NewReader(`{"expected_version":1,"checker":"checker-001"}`)))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing = %d, want 404", recorder.Code)
	}
}

func TestApproveRejectsBadInput(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/financial-intents/intent-001/approve", strings.NewReader(`{"expected_version":0,"checker":""}`)))
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad approval = %d, want 422", recorder.Code)
	}
}
