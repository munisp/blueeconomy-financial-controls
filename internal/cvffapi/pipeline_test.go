package cvffapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvff"
	"github.com/munisp/blueeconomy-financial-controls/internal/workflow"
)

const testOfficerSubject = "kc-officer-001"

// chainPrincipals binds the six chain roles to six distinct parties.
func chainPrincipals() map[cvff.Role]string {
	return map[cvff.Role]string{
		cvff.RoleUnderwriterPrimary:   "kc-uw-primary",
		cvff.RoleUnderwriterSecondary: "kc-uw-secondary",
		cvff.RoleUnderwriterTertiary:  "kc-uw-tertiary",
		cvff.RoleNIMASAApprover:       "kc-nimasa",
		cvff.RoleReceivingBank:        "kc-bank",
		cvff.RoleBeneficiary:          testSubject,
	}
}

// pipelineFixture builds a handler whose authenticator dispatches on the
// bearer token to one chain party at a time.
func pipelineFixture(t *testing.T, store *fakeStore, starter WorkflowStarter, signaler *fakeSignaler) http.Handler {
	t.Helper()
	handler, err := NewHandler(store,
		pipelineAuthenticator{},
		&fakeBlobs{puts: map[string][]byte{}}, fakeScanner{}, testLimits(), starter, signaler, testPolicyEnforcer(t))
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler
}

// pipelineAuthenticator issues principals by bearer token: the token text is
// the principal subject, and its realm roles derive from the chain binding.
type pipelineAuthenticator struct{}

func (pipelineAuthenticator) Authenticate(_ context.Context, authorizationHeader string) (Principal, error) {
	token := strings.TrimPrefix(authorizationHeader, "Bearer ")
	if token == authorizationHeader || token == "" {
		return Principal{}, ErrUnauthenticated
	}
	roles := map[string][]string{
		testSubject:        {BeneficiaryRole},
		testOfficerSubject: {OfficerRole},
		"kc-uw-primary":    {"underwriter"},
		"kc-uw-secondary":  {"underwriter"},
		"kc-uw-tertiary":   {"underwriter"},
		"kc-nimasa":        {"nimasa-approver"},
		"kc-bank":          {"receiving-bank"},
		"kc-recon-officer": {ReconciliationOfficerRole},
		"kc-outsider":      {"beneficiary"},
	}
	held, ok := roles[token]
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{Subject: token, Roles: held}, nil
}

func authedRequest(t *testing.T, method, path, subject, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+subject)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func serve(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestFourPartyChainAdvancesEndToEnd proves the production wiring: intake →
// role assignment (workflow start) → every party decision signalled in chain
// order through to beneficiary confirmation, with SoD violations denied.
func TestFourPartyChainAdvancesEndToEnd(t *testing.T) {
	store := &fakeStore{}
	starter := &fakeStarter{}
	signaler := &fakeSignaler{}
	handler := pipelineFixture(t, store, starter, signaler)

	// 1. Beneficiary intake starts the rail.
	intake := authedRequest(t, http.MethodPost, "/v1/cvff/applications", testSubject, `{
		"vessel_name":"MV Adaeze","imo_number":"9074729","official_number":"NIMASA-4421",
		"vessel_class":"CARGO_COASTER","cabotage_route":"LAGOS_ONNE",
		"amount":750000000,"currency":"NGN",
		"business_name":"Adaeze Coastal Logistics Ltd","business_rc_number":"RC123456",
		"business_address":"14 Marina Road, Lagos Island, Lagos"}`)
	intake.Header.Set("Idempotency-Key", "7f3a1c2e-9b0d-4e5f-a1b2-c3d4e5f60718")
	if recorder := serve(handler, intake); recorder.Code != http.StatusCreated {
		t.Fatalf("intake = %d: %s", recorder.Code, recorder.Body)
	}
	applicationID := store.submitted.ApplicationID
	if len(starter.started) != 1 || starter.started[0] != applicationID {
		t.Fatalf("workflow starts = %v", starter.started)
	}

	// 2. A decision before role assignment conflicts honestly.
	early := serve(handler, authedRequest(t, http.MethodPost, "/v1/cvff/applications/"+applicationID+"/decisions", "kc-uw-primary", `{"decision":"APPROVE"}`))
	if early.Code != http.StatusConflict {
		t.Fatalf("decision before assignment = %d: %s", early.Code, early.Body)
	}

	// 3. Officer assigns the four parties; the rail re-starts idempotently.
	assignBody, _ := json.Marshal(map[string]any{"assignments": map[string]string{
		"UNDERWRITER_PRIMARY":   "kc-uw-primary",
		"UNDERWRITER_SECONDARY": "kc-uw-secondary",
		"UNDERWRITER_TERTIARY":  "kc-uw-tertiary",
		"NIMASA_APPROVER":       "kc-nimasa",
		"RECEIVING_BANK":        "kc-bank",
		"BENEFICIARY":           testSubject,
	}})
	if recorder := serve(handler, authedRequest(t, http.MethodPut, "/v1/cvff/admin/applications/"+applicationID+"/roles", testOfficerSubject, string(assignBody))); recorder.Code != http.StatusOK {
		t.Fatalf("assign roles = %d: %s", recorder.Code, recorder.Body)
	}
	if len(starter.started) != 2 {
		t.Fatalf("workflow re-start after assignment = %v", starter.started)
	}

	// 4. The chain advances state by state; each signal carries the
	//    deterministic application workflow address.
	states := []cvff.State{
		cvff.StateUnderwritingPrimary, cvff.StateUnderwritingSecondary, cvff.StateUnderwritingTertiary,
		cvff.StateNIMASAApproval, cvff.StateBankConfirmation, cvff.StateDisbursementPending,
	}
	parties := []string{"kc-uw-primary", "kc-uw-secondary", "kc-uw-tertiary", "kc-nimasa", "kc-bank", testSubject}
	wantSignals := []string{
		workflow.SignalUnderwritingDecision, workflow.SignalUnderwritingDecision, workflow.SignalUnderwritingDecision,
		workflow.SignalNIMASADecision, workflow.SignalBankConfirmation, workflow.SignalBeneficiaryConfirmation,
	}
	for step, state := range states {
		store.setState(applicationID, state)
		// The wrong party for this state is forbidden.
		wrongParty := parties[(step+1)%len(parties)]
		if recorder := serve(handler, authedRequest(t, http.MethodPost, "/v1/cvff/applications/"+applicationID+"/decisions", wrongParty, `{"decision":"APPROVE"}`)); recorder.Code != http.StatusForbidden {
			t.Fatalf("step %d: wrong party decision = %d: %s", step, recorder.Code, recorder.Body)
		}
		// The assigned party advances the chain.
		recorder := serve(handler, authedRequest(t, http.MethodPost, "/v1/cvff/applications/"+applicationID+"/decisions", parties[step], `{"decision":"APPROVE"}`))
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("step %d: decision = %d: %s", step, recorder.Code, recorder.Body)
		}
	}
	if len(signaler.decisions) != len(states) {
		t.Fatalf("signals = %d, want %d", len(signaler.decisions), len(states))
	}
	for step, recorded := range signaler.decisions {
		if recorded.applicationID != applicationID || recorded.signalName != wantSignals[step] ||
			recorded.signal.PrincipalID != parties[step] || recorded.signal.Decision != cvff.DecisionApprove {
			t.Fatalf("signal %d = %+v", step, recorded)
		}
	}
}

// TestAssignRolesSegregationOfDutiesDenied: the PBAC policy and the store
// both refuse SoD-violating bindings.
func TestAssignRolesSegregationOfDutiesDenied(t *testing.T) {
	store := &fakeStore{applications: []cvff.ApplicationDetail{seededApplication()}}
	handler := pipelineFixture(t, store, &fakeStarter{}, &fakeSignaler{})
	applicationID := "cvff-app-001"
	assign := func(body string, subject string) *httptest.ResponseRecorder {
		return serve(handler, authedRequest(t, http.MethodPut, "/v1/cvff/admin/applications/"+applicationID+"/roles", subject, body))
	}
	// One principal holding two chain roles.
	collapsed := `{"assignments":{"UNDERWRITER_PRIMARY":"kc-uw-primary","UNDERWRITER_SECONDARY":"kc-uw-secondary","UNDERWRITER_TERTIARY":"kc-uw-tertiary","NIMASA_APPROVER":"kc-uw-primary","RECEIVING_BANK":"kc-bank","BENEFICIARY":"kc-beneficiary-x"}}`
	if recorder := assign(collapsed, testOfficerSubject); recorder.Code != http.StatusUnprocessableEntity && recorder.Code != http.StatusForbidden {
		t.Fatalf("collapsed SoD = %d: %s", recorder.Code, recorder.Body)
	}
	// The officer binding themselves as a party.
	selfDealing := `{"assignments":{"UNDERWRITER_PRIMARY":"kc-uw-primary","UNDERWRITER_SECONDARY":"kc-uw-secondary","UNDERWRITER_TERTIARY":"kc-uw-tertiary","NIMASA_APPROVER":"kc-nimasa","RECEIVING_BANK":"` + testOfficerSubject + `","BENEFICIARY":"kc-beneficiary-x"}}`
	if recorder := assign(selfDealing, testOfficerSubject); recorder.Code != http.StatusForbidden && recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("self-dealing = %d: %s", recorder.Code, recorder.Body)
	}
	// A beneficiary cannot assign roles at all (realm role gate).
	valid := `{"assignments":{"UNDERWRITER_PRIMARY":"kc-uw-primary","UNDERWRITER_SECONDARY":"kc-uw-secondary","UNDERWRITER_TERTIARY":"kc-uw-tertiary","NIMASA_APPROVER":"kc-nimasa","RECEIVING_BANK":"kc-bank","BENEFICIARY":"kc-beneficiary-x"}}`
	if recorder := assign(valid, testSubject); recorder.Code != http.StatusForbidden {
		t.Fatalf("beneficiary assignment = %d: %s", recorder.Code, recorder.Body)
	}
	// Valid assignment is accepted; identical replay is idempotent success;
	// divergent replay conflicts.
	if recorder := assign(valid, testOfficerSubject); recorder.Code != http.StatusOK {
		t.Fatalf("valid assignment = %d: %s", recorder.Code, recorder.Body)
	}
	if recorder := assign(valid, testOfficerSubject); recorder.Code != http.StatusOK {
		t.Fatalf("idempotent replay = %d: %s", recorder.Code, recorder.Body)
	}
	divergent := strings.Replace(valid, "kc-nimasa", "kc-nimasa-other", 1)
	if recorder := assign(divergent, testOfficerSubject); recorder.Code != http.StatusConflict {
		t.Fatalf("divergent replay = %d: %s", recorder.Code, recorder.Body)
	}
}

// TestRecordDecisionFailClosed covers the pipeline's refusal modes.
func TestRecordDecisionFailClosed(t *testing.T) {
	store := &fakeStore{applications: []cvff.ApplicationDetail{seededApplication()}}
	store.assignments = map[string]map[cvff.Role]string{"cvff-app-001": chainPrincipals()}
	signaler := &fakeSignaler{}
	handler := pipelineFixture(t, store, &fakeStarter{}, signaler)
	applicationID := "cvff-app-001"
	decide := func(subject, body string) *httptest.ResponseRecorder {
		return serve(handler, authedRequest(t, http.MethodPost, "/v1/cvff/applications/"+applicationID+"/decisions", subject, body))
	}
	// No role gate passes for a principal without any chain role... the
	// policy denies before the handler runs.
	if recorder := decide("kc-outsider", `{"decision":"APPROVE"}`); recorder.Code != http.StatusForbidden {
		t.Fatalf("non-party decision = %d: %s", recorder.Code, recorder.Body)
	}
	// Unauthenticated is rejected.
	unauth := httptest.NewRequest(http.MethodPost, "/v1/cvff/applications/"+applicationID+"/decisions", strings.NewReader(`{"decision":"APPROVE"}`))
	if recorder := serve(handler, unauth); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated decision = %d", recorder.Code)
	}
	// Invalid decision value.
	if recorder := decide("kc-uw-primary", `{"decision":"MAYBE"}`); recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid decision = %d", recorder.Code)
	}
	// Signaler outage surfaces as retryable 503, never a silent drop.
	signaler.err = errors.New("temporal unavailable")
	if recorder := decide("kc-uw-primary", `{"decision":"APPROVE"}`); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("signaler outage = %d: %s", recorder.Code, recorder.Body)
	}
	signaler.err = nil
	if recorder := decide("kc-uw-primary", `{"decision":"APPROVE"}`); recorder.Code != http.StatusAccepted {
		t.Fatalf("valid decision = %d: %s", recorder.Code, recorder.Body)
	}
}

// TestResolveReconciliation exercises the RECONCILIATION_REQUIRED exit path.
func TestResolveReconciliation(t *testing.T) {
	store := &fakeStore{applications: []cvff.ApplicationDetail{seededApplication()}}
	signaler := &fakeSignaler{}
	handler := pipelineFixture(t, store, &fakeStarter{}, signaler)
	applicationID := "cvff-app-001"
	store.setState(applicationID, cvff.StateReconciliationRequired)
	resolve := func(subject, body string) *httptest.ResponseRecorder {
		return serve(handler, authedRequest(t, http.MethodPost, "/v1/cvff/admin/applications/"+applicationID+"/reconciliation", subject, body))
	}
	// Only reconciliation officers may resolve.
	if recorder := resolve(testOfficerSubject, `{"resolution":"RESUME_DISBURSEMENT"}`); recorder.Code != http.StatusForbidden {
		t.Fatalf("officer resolution = %d: %s", recorder.Code, recorder.Body)
	}
	// Unknown resolution values are rejected.
	if recorder := resolve("kc-recon-officer", `{"resolution":"FORCE_APPROVE"}`); recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bogus resolution = %d", recorder.Code)
	}
	// RESUME signals the parked workflow.
	if recorder := resolve("kc-recon-officer", `{"resolution":"RESUME_DISBURSEMENT"}`); recorder.Code != http.StatusAccepted {
		t.Fatalf("resume resolution = %d: %s", recorder.Code, recorder.Body)
	}
	if len(signaler.resolutions) != 1 || signaler.resolutions[0].Resolution != cvff.ResolutionResumeDisbursement ||
		signaler.resolutions[0].PrincipalID != "kc-recon-officer" {
		t.Fatalf("resolution signals = %+v", signaler.resolutions)
	}
	// An application not parked in reconciliation conflicts.
	store.setState(applicationID, cvff.StateDisbursementPending)
	if recorder := resolve("kc-recon-officer", `{"resolution":"REJECT"}`); recorder.Code != http.StatusConflict {
		t.Fatalf("non-parked resolution = %d: %s", recorder.Code, recorder.Body)
	}
}
