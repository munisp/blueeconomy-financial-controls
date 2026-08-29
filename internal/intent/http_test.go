package intent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvffapi"
	"github.com/munisp/blueeconomy-financial-controls/internal/pbac"
)

// tokenAuthenticator maps bearer token text to verified principals, standing
// in for Keycloak signature verification in unit tests.
type tokenAuthenticator struct {
	principals map[string]cvffapi.Principal
}

func (authenticator tokenAuthenticator) Authenticate(_ context.Context, authorizationHeader string) (cvffapi.Principal, error) {
	token := strings.TrimSpace(strings.TrimPrefix(authorizationHeader, "Bearer "))
	principal, ok := authenticator.principals[token]
	if !ok || !strings.HasPrefix(authorizationHeader, "Bearer ") {
		return cvffapi.Principal{}, cvffapi.ErrUnauthenticated
	}
	return principal, nil
}

const (
	makerToken   = "maker-token"
	checkerToken = "checker-token"
	officerToken = "officer-token"
)

func testAuthenticator() tokenAuthenticator {
	return tokenAuthenticator{principals: map[string]cvffapi.Principal{
		makerToken:   {Subject: "maker-001", Roles: []string{IntentMakerRole}},
		checkerToken: {Subject: "checker-001", Roles: []string{IntentCheckerRole}},
		officerToken: {Subject: "officer-001", Roles: []string{FinancialControllerRole}},
	}}
}

func testPolicy(t *testing.T) *pbac.Enforcer {
	t.Helper()
	enforcer, err := pbac.LoadEnforcer(filepath.Join("..", "..", "policies"))
	if err != nil {
		t.Fatalf("load policy pack: %v", err)
	}
	return enforcer
}

// fakeAPIStore implements the maker-checker rule against the retained intent,
// like the durable store does.
type fakeAPIStore struct {
	createErr  error
	approveErr error
	voidErr    error
	retained   Intent
}

func (store *fakeAPIStore) Create(_ context.Context, request CreateRequest) (Intent, error) {
	if store.createErr != nil {
		return Intent{}, store.createErr
	}
	store.retained = Intent{CreateRequest: request, State: StateDraft, Version: 1}
	return store.retained, nil
}

func (store *fakeAPIStore) Approve(_ context.Context, intentID string, expectedVersion int64, checker string) (Intent, error) {
	if store.approveErr != nil {
		return Intent{}, store.approveErr
	}
	if store.retained.IntentID == "" {
		return Intent{}, ErrNotFound
	}
	return Approve(store.retained, checker)
}

func (store *fakeAPIStore) VoidDraft(_ context.Context, intentID string, expectedVersion int64, actor string) (Intent, error) {
	if store.voidErr != nil {
		return Intent{}, store.voidErr
	}
	if store.retained.IntentID == "" {
		return Intent{}, ErrNotFound
	}
	return VoidDraft(store.retained, expectedVersion, actor)
}

// fakeResolver applies the officer-resolution rules in memory like the
// orchestration Resolver (without the ledger leg, which the orchestration
// tests cover).
type fakeResolver struct {
	err      error
	officer  string
	resolved Resolution
}

func (resolver *fakeResolver) ResolveAmbiguous(_ context.Context, _ string, expectedVersion int64, officer string, resolution Resolution) (Intent, error) {
	if resolver.err != nil {
		return Intent{}, resolver.err
	}
	current := Intent{CreateRequest: validRequest(), State: StateAmbiguous, Version: expectedVersion}
	updated, err := ResolveAmbiguous(current, expectedVersion, officer, resolution)
	if err != nil {
		return Intent{}, err
	}
	resolver.officer = officer
	resolver.resolved = resolution
	return updated, nil
}

func newTestHandler(t *testing.T, store APIStore) http.Handler {
	t.Helper()
	return newTestHandlerWithResolver(t, store, &fakeResolver{})
}

func newTestHandlerWithResolver(t *testing.T, store APIStore, resolver AmbiguousResolver) http.Handler {
	t.Helper()
	handler, err := NewHandler(store, testAuthenticator(), testPolicy(t), resolver)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler
}

func authenticatedRequest(method, target, token, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request
}

func TestHandlerRequiresDependencies(t *testing.T) {
	if _, err := NewHandler(nil, testAuthenticator(), testPolicy(t), &fakeResolver{}); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := NewHandler(&fakeAPIStore{}, nil, testPolicy(t), &fakeResolver{}); err == nil {
		t.Fatal("nil authenticator accepted")
	}
	if _, err := NewHandler(&fakeAPIStore{}, testAuthenticator(), nil, &fakeResolver{}); err == nil {
		t.Fatal("nil policy accepted")
	}
	if _, err := NewHandler(&fakeAPIStore{}, testAuthenticator(), testPolicy(t), nil); err == nil {
		t.Fatal("nil resolver accepted")
	}
}

func TestHealthz(t *testing.T) {
	recorder := httptest.NewRecorder()
	newTestHandler(t, &fakeAPIStore{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz = %d", recorder.Code)
	}
}

const validCreateBody = `{"intent_id":"intent-001","external_ref":"ref-001","debit_account_id":"0000000000000000000000000000000b","credit_account_id":"00000000000000000000000000000016","amount":1000,"ledger":1,"code":1,"currency":"NGN"}`

func TestCreateIntentUnauthenticated(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{})
	for name, request := range map[string]*http.Request{
		"no token":      authenticatedRequest(http.MethodPost, "/v1/financial-intents", "", validCreateBody),
		"unknown token": authenticatedRequest(http.MethodPost, "/v1/financial-intents", "forged-token", validCreateBody),
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s = %d, want 401", name, recorder.Code)
		}
	}
}

func TestCreateIntentForbiddenWithoutMakerRole(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{})
	recorder := httptest.NewRecorder()
	// The checker token is verified but holds no maker role / policy grant.
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents", checkerToken, validCreateBody))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("create with checker role = %d, want 403", recorder.Code)
	}
}

func TestCreateIntentDerivesMakerFromToken(t *testing.T) {
	store := &fakeAPIStore{}
	handler := newTestHandler(t, store)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents", makerToken, validCreateBody))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", recorder.Code, recorder.Body)
	}
	if store.retained.Maker != "maker-001" {
		t.Fatalf("maker = %q, want verified token subject maker-001", store.retained.Maker)
	}
}

func TestCreateIntentRejectsBodySuppliedMaker(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{})
	body := `{"intent_id":"intent-001","external_ref":"ref-001","debit_account_id":"0000000000000000000000000000000b","credit_account_id":"00000000000000000000000000000016","amount":1000,"ledger":1,"code":1,"currency":"NGN","maker":"attacker"}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents", makerToken, body))
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("body-supplied maker = %d, want 422", recorder.Code)
	}
}

func TestCreateIntentValidationFailures(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{})
	for name, body := range map[string]string{
		"malformed JSON":      `{broken`,
		"unknown field":       `{"intent_id":"x","surprise":1}`,
		"unapproved currency": `{"intent_id":"intent-001","external_ref":"ref-001","debit_account_id":"0000000000000000000000000000000b","credit_account_id":"00000000000000000000000000000016","amount":1000,"ledger":1,"code":1,"currency":"EUR"}`,
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents", makerToken, body))
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s = %d, want 422", name, recorder.Code)
		}
	}
}

func TestCreateIntentIdempotencyConflict(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{createErr: ErrImmutableConflict})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents", makerToken, validCreateBody))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("conflict = %d, want 409", recorder.Code)
	}
}

func createDraft(t *testing.T, handler http.Handler, store *fakeAPIStore) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents", makerToken, validCreateBody))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("setup create = %d: %s", recorder.Code, recorder.Body)
	}
}

func TestApproveUnauthenticated(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/approve", "", `{"expected_version":1}`))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated approve = %d, want 401", recorder.Code)
	}
}

func TestApproveForbiddenWithoutCheckerRole(t *testing.T) {
	store := &fakeAPIStore{}
	handler := newTestHandler(t, store)
	createDraft(t, handler, store)
	recorder := httptest.NewRecorder()
	// The maker token is verified but holds no checker role / policy grant.
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/approve", makerToken, `{"expected_version":1}`))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("approve with maker role = %d, want 403", recorder.Code)
	}
}

func TestApproveSameSubjectAsMakerConflicts(t *testing.T) {
	store := &fakeAPIStore{approveErr: ErrMakerChecker}
	handler := newTestHandler(t, store)
	createDraft(t, handler, store)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/approve", checkerToken, `{"expected_version":1}`))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("same-subject approve = %d, want 409", recorder.Code)
	}
}

func TestApproveMakerCannotBeCheckerEvenWithBothRoles(t *testing.T) {
	// A principal holding both roles still cannot self-approve: the store
	// enforces maker != checker on the verified subjects.
	bothRoles := tokenAuthenticator{principals: map[string]cvffapi.Principal{
		"dual": {Subject: "maker-001", Roles: []string{IntentMakerRole, IntentCheckerRole}},
	}}
	store := &fakeAPIStore{}
	handler, err := NewHandler(store, bothRoles, testPolicy(t), &fakeResolver{})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents", "dual", validCreateBody))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("setup create = %d: %s", recorder.Code, recorder.Body)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/approve", "dual", `{"expected_version":1}`))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("self-approve = %d, want 409", recorder.Code)
	}
}

func TestApproveDistinctAuthorizedSubject(t *testing.T) {
	store := &fakeAPIStore{}
	handler := newTestHandler(t, store)
	createDraft(t, handler, store)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/approve", checkerToken, `{"expected_version":1}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", recorder.Code, recorder.Body)
	}
	var updated Intent
	if err := decodeJSONBody(recorder, &updated); err != nil {
		t.Fatalf("decode approve response: %v", err)
	}
	if updated.Checker == nil || *updated.Checker != "checker-001" {
		t.Fatalf("checker = %v, want verified token subject checker-001", updated.Checker)
	}
}

func TestApproveRejectsBodySuppliedChecker(t *testing.T) {
	store := &fakeAPIStore{}
	handler := newTestHandler(t, store)
	createDraft(t, handler, store)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/approve", checkerToken, `{"expected_version":1,"checker":"attacker"}`))
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("body-supplied checker = %d, want 422", recorder.Code)
	}
}

func TestApproveIntentConflicts(t *testing.T) {
	for name, storeErr := range map[string]error{
		"version": ErrConflict,
		"state":   ErrInvalidState,
	} {
		handler := newTestHandler(t, &fakeAPIStore{approveErr: storeErr})
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/approve", checkerToken, `{"expected_version":1}`))
		if recorder.Code != http.StatusConflict {
			t.Fatalf("%s = %d, want 409", name, recorder.Code)
		}
	}
	handler := newTestHandler(t, &fakeAPIStore{approveErr: ErrNotFound})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/approve", checkerToken, `{"expected_version":1}`))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing = %d, want 404", recorder.Code)
	}
}

func TestApproveRejectsBadInput(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/approve", checkerToken, `{"expected_version":0}`))
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad approval = %d, want 422", recorder.Code)
	}
}

func TestVoidDraftUnauthenticated(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/void", "", `{"expected_version":1}`))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated void = %d, want 401", recorder.Code)
	}
}

func TestVoidDraftByMaker(t *testing.T) {
	store := &fakeAPIStore{}
	handler := newTestHandler(t, store)
	createDraft(t, handler, store)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/void", makerToken, `{"expected_version":1}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("maker void = %d: %s", recorder.Code, recorder.Body)
	}
	var updated Intent
	if err := decodeJSONBody(recorder, &updated); err != nil {
		t.Fatalf("decode void response: %v", err)
	}
	if updated.State != StateVoided {
		t.Fatalf("state = %s, want VOIDED", updated.State)
	}
}

func TestVoidDraftForbiddenForNonMaker(t *testing.T) {
	store := &fakeAPIStore{}
	handler := newTestHandler(t, store)
	createDraft(t, handler, store)
	// A different maker-role principal is authenticated and policy-cleared,
	// but is not the recorded maker of this intent.
	otherMaker := tokenAuthenticator{principals: map[string]cvffapi.Principal{
		"other": {Subject: "maker-002", Roles: []string{IntentMakerRole}},
	}}
	otherHandler, err := NewHandler(store, otherMaker, testPolicy(t), &fakeResolver{})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	recorder := httptest.NewRecorder()
	otherHandler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/void", "other", `{"expected_version":1}`))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-maker void = %d, want 403", recorder.Code)
	}
}

func TestVoidDraftConflicts(t *testing.T) {
	for name, storeErr := range map[string]error{
		"not-draft": ErrInvalidState,
		"version":   ErrConflict,
	} {
		handler := newTestHandler(t, &fakeAPIStore{voidErr: storeErr})
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/void", makerToken, `{"expected_version":1}`))
		if recorder.Code != http.StatusConflict {
			t.Fatalf("%s = %d, want 409", name, recorder.Code)
		}
	}
}

func TestResolveUnauthenticated(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/resolve", "", `{"expected_version":3,"resolution":"RECONCILE"}`))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated resolve = %d, want 401", recorder.Code)
	}
}

func TestResolveForbiddenWithoutControllerRole(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{})
	for name, token := range map[string]string{"maker": makerToken, "checker": checkerToken} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/resolve", token, `{"expected_version":3,"resolution":"RECONCILE"}`))
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s resolve = %d, want 403", name, recorder.Code)
		}
	}
}

func TestResolveAmbiguousByOfficer(t *testing.T) {
	for name, testCase := range map[string]struct {
		resolution Resolution
		wantState  State
	}{
		"reconcile": {ResolutionReconcile, StateReconciliationRequired},
		"void":      {ResolutionVoid, StateVoided},
	} {
		resolver := &fakeResolver{}
		handler := newTestHandlerWithResolver(t, &fakeAPIStore{}, resolver)
		recorder := httptest.NewRecorder()
		body := `{"expected_version":3,"resolution":"` + string(testCase.resolution) + `"}`
		handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/resolve", officerToken, body))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s resolve = %d: %s", name, recorder.Code, recorder.Body)
		}
		if resolver.officer != "officer-001" {
			t.Fatalf("%s officer = %q, want verified token subject officer-001", name, resolver.officer)
		}
		var updated Intent
		if err := decodeJSONBody(recorder, &updated); err != nil {
			t.Fatalf("%s decode: %v", name, err)
		}
		if updated.State != testCase.wantState {
			t.Fatalf("%s state = %s, want %s", name, updated.State, testCase.wantState)
		}
	}
}

func TestResolveRejectsInvalidResolution(t *testing.T) {
	handler := newTestHandler(t, &fakeAPIStore{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/resolve", officerToken, `{"expected_version":3,"resolution":"FORCE_POST"}`))
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid resolution = %d, want 422", recorder.Code)
	}
}

func TestResolveConflicts(t *testing.T) {
	for name, resolverErr := range map[string]error{
		"not-ambiguous": ErrInvalidState,
		"version":       ErrConflict,
		"evidence":      ErrResolutionRejected,
	} {
		handler := newTestHandlerWithResolver(t, &fakeAPIStore{}, &fakeResolver{err: resolverErr})
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/resolve", officerToken, `{"expected_version":3,"resolution":"VOID"}`))
		if recorder.Code != http.StatusConflict {
			t.Fatalf("%s = %d, want 409", name, recorder.Code)
		}
	}
	handler := newTestHandlerWithResolver(t, &fakeAPIStore{}, &fakeResolver{err: ErrNotFound})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/financial-intents/intent-001/resolve", officerToken, `{"expected_version":3,"resolution":"VOID"}`))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing = %d, want 404", recorder.Code)
	}
}

func decodeJSONBody(recorder *httptest.ResponseRecorder, target any) error {
	return json.Unmarshal(recorder.Body.Bytes(), target)
}
