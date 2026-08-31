package cvffapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvff"
	"github.com/munisp/blueeconomy-financial-controls/internal/pbac"
	"github.com/munisp/blueeconomy-financial-controls/internal/workflow"
)

const (
	testBearer   = "Bearer test-token"
	testNow      = "2026-08-28T12:00:00Z"
	testDocBytes = 10 * 1024 * 1024
)

type fakeStore struct {
	applications []cvff.ApplicationDetail
	approvals    []cvff.Approval
	documents    []cvff.Document
	submitted    cvff.Intake
	submitErr    error
	documentErr  error
	createdDocs  []cvff.Document
	report       []cvff.DualLedgerReport
	reportErr    error
	reportFrom   time.Time
	reportTo     time.Time
	assignments  map[string]map[cvff.Role]string
	getErr       error
	assignErr    error
	resolveErr   error
}

func (store *fakeStore) Get(_ context.Context, applicationID string) (cvff.Application, error) {
	if store.getErr != nil {
		return cvff.Application{}, store.getErr
	}
	for _, application := range store.applications {
		if application.ApplicationID == applicationID {
			return application.Application, nil
		}
	}
	return cvff.Application{}, cvff.ErrNotFound
}

func (store *fakeStore) RoleAssignments(_ context.Context, applicationID string) (map[cvff.Role]string, error) {
	if assignments, ok := store.assignments[applicationID]; ok {
		return assignments, nil
	}
	return nil, cvff.ErrNotFound
}

func (store *fakeStore) AssignRoles(_ context.Context, applicationID string, _ string, assignments map[cvff.Role]string) (map[cvff.Role]string, error) {
	if store.assignErr != nil {
		return nil, store.assignErr
	}
	if _, err := store.Get(context.Background(), applicationID); err != nil {
		return nil, err
	}
	if store.assignments == nil {
		store.assignments = map[string]map[cvff.Role]string{}
	}
	if existing, ok := store.assignments[applicationID]; ok {
		for role, party := range assignments {
			if existing[role] != party {
				return nil, cvff.ErrConflict
			}
		}
		return existing, nil
	}
	retained := map[cvff.Role]string{}
	for role, party := range assignments {
		retained[role] = party
	}
	store.assignments[applicationID] = retained
	return retained, nil
}

func (store *fakeStore) ResolveReconciliation(_ context.Context, applicationID string, _ int64, _ string, resolution cvff.ReconciliationResolution) (cvff.Application, error) {
	if store.resolveErr != nil {
		return cvff.Application{}, store.resolveErr
	}
	for index, application := range store.applications {
		if application.ApplicationID != applicationID {
			continue
		}
		updated, err := cvff.ResolveReconciliation(application.Application, resolution)
		if err != nil {
			return cvff.Application{}, err
		}
		store.applications[index].Application = updated
		return updated, nil
	}
	return cvff.Application{}, cvff.ErrNotFound
}

type fakeSignaler struct {
	decisions   []recordedSignal
	resolutions []workflow.ResolutionSignal
	err         error
	resolveErr  error
}

type recordedSignal struct {
	applicationID string
	signalName    string
	signal        workflow.DecisionSignal
}

func (signaler *fakeSignaler) SignalDecision(_ context.Context, applicationID, signalName string, signal workflow.DecisionSignal) error {
	if signaler.err != nil {
		return signaler.err
	}
	signaler.decisions = append(signaler.decisions, recordedSignal{applicationID: applicationID, signalName: signalName, signal: signal})
	return nil
}

func (signaler *fakeSignaler) SignalResolution(_ context.Context, applicationID string, signal workflow.ResolutionSignal) error {
	if signaler.resolveErr != nil {
		return signaler.resolveErr
	}
	signaler.resolutions = append(signaler.resolutions, signal)
	return nil
}

// testPolicyEnforcer compiles the shipped policy pack so handler tests run
// the production allow/deny rules.
func testPolicyEnforcer(t *testing.T) *pbac.Enforcer {
	t.Helper()
	enforcer, err := pbac.LoadEnforcer(filepath.Join("..", "..", "policies"))
	if err != nil {
		t.Fatalf("load policy pack: %v", err)
	}
	return enforcer
}

func (store *fakeStore) DualLedgerReport(_ context.Context, from, to time.Time) ([]cvff.DualLedgerReport, error) {
	store.reportFrom, store.reportTo = from, to
	if store.reportErr != nil {
		return nil, store.reportErr
	}
	return store.report, nil
}

type fakeStarter struct {
	started []string
	err     error
}

func (starter *fakeStarter) StartDisbursement(_ context.Context, applicationID string) error {
	if starter.err != nil {
		return starter.err
	}
	starter.started = append(starter.started, applicationID)
	return nil
}

func (store *fakeStore) SubmitIntake(_ context.Context, intake cvff.Intake) (cvff.ApplicationDetail, error) {
	store.submitted = intake
	if store.submitErr != nil {
		return cvff.ApplicationDetail{}, store.submitErr
	}
	for _, application := range store.applications {
		if application.ExternalRef == intake.IdempotencyKey {
			if application.BeneficiaryID != intake.BeneficiaryID || application.IMONumber != intake.IMONumber {
				return cvff.ApplicationDetail{}, cvff.ErrConflict
			}
			return application, nil
		}
	}
	retained := cvff.ApplicationDetail{
		Application: cvff.Application{
			ApplicationID: intake.ApplicationID,
			ExternalRef:   intake.IdempotencyKey,
			BeneficiaryID: intake.BeneficiaryID,
			Amount:        intake.Amount,
			Currency:      intake.Currency,
			State:         cvff.StateSubmitted,
			CreatedAt:     time.Now().UTC(),
			UpdatedAt:     time.Now().UTC(),
			Version:       1,
		},
		VesselName:       intake.VesselName,
		IMONumber:        intake.IMONumber,
		OfficialNumber:   intake.OfficialNumber,
		VesselClass:      intake.VesselClass,
		CabotageRoute:    intake.CabotageRoute,
		BusinessName:     intake.BusinessName,
		BusinessRCNumber: intake.BusinessRCNumber,
		BusinessAddress:  intake.BusinessAddress,
		StateEnteredAt:   time.Now().UTC(),
	}
	store.applications = append(store.applications, retained)
	return retained, nil
}

func (store *fakeStore) GetForBeneficiary(_ context.Context, applicationID string, beneficiaryID string) (cvff.ApplicationDetail, error) {
	for _, application := range store.applications {
		if application.ApplicationID == applicationID && application.BeneficiaryID == beneficiaryID {
			return application, nil
		}
	}
	return cvff.ApplicationDetail{}, cvff.ErrNotFound
}

func (store *fakeStore) ListForBeneficiary(_ context.Context, beneficiaryID string) ([]cvff.ApplicationDetail, error) {
	result := make([]cvff.ApplicationDetail, 0)
	for _, application := range store.applications {
		if application.BeneficiaryID == beneficiaryID {
			result = append(result, application)
		}
	}
	return result, nil
}

func (store *fakeStore) ListApprovals(_ context.Context, applicationID string) ([]cvff.Approval, error) {
	result := make([]cvff.Approval, 0)
	for _, approval := range store.approvals {
		if approval.ApplicationID == applicationID {
			result = append(result, approval)
		}
	}
	return result, nil
}

func (store *fakeStore) CreateDocument(_ context.Context, document cvff.Document) (cvff.Document, error) {
	if store.documentErr != nil {
		return cvff.Document{}, store.documentErr
	}
	for _, existing := range store.documents {
		if existing.ApplicationID == document.ApplicationID && existing.IdempotencyKey == document.IdempotencyKey {
			if existing.SHA256Hex != document.SHA256Hex {
				return cvff.Document{}, cvff.ErrConflict
			}
			return existing, nil
		}
	}
	document.DocumentID = "doc-" + document.SHA256Hex[:8]
	document.CreatedAt = time.Now().UTC()
	store.documents = append(store.documents, document)
	store.createdDocs = append(store.createdDocs, document)
	return document, nil
}

// setState moves one seeded application for pipeline tests.
func (store *fakeStore) setState(applicationID string, state cvff.State) {
	for index, application := range store.applications {
		if application.ApplicationID == applicationID {
			store.applications[index].Application.State = state
		}
	}
}

func (store *fakeStore) ListDocuments(_ context.Context, applicationID string) ([]cvff.Document, error) {
	result := make([]cvff.Document, 0)
	for _, document := range store.documents {
		if document.ApplicationID == applicationID {
			result = append(result, document)
		}
	}
	return result, nil
}

type fakeBlobs struct {
	puts map[string][]byte
	err  error
}

func (blobs *fakeBlobs) Put(_ context.Context, key string, _ string, content io.Reader, _ int64) error {
	if blobs.err != nil {
		return blobs.err
	}
	body, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	blobs.puts[key] = body
	return nil
}

func (blobs *fakeBlobs) Backend() string { return "s3" }

type fakeScanner struct{ err error }

func (scanner fakeScanner) Scan(_ context.Context, _ string, _ []byte) error { return scanner.err }

func testLimits() Limits {
	return Limits{
		MaxDocumentBytes:           testDocBytes,
		MaxDocumentsPerApplication: 12,
		DocumentContentTypes:       []string{"application/pdf", "image/png", "image/jpeg"},
	}
}

func newTestHandler(t *testing.T, store *fakeStore, blobs *fakeBlobs, scanner Scanner) http.Handler {
	t.Helper()
	return newTestHandlerWithStarter(t, store, blobs, scanner, &fakeStarter{})
}

func newTestHandlerWithStarter(t *testing.T, store *fakeStore, blobs *fakeBlobs, scanner Scanner, starter WorkflowStarter) http.Handler {
	t.Helper()
	handler, err := NewHandler(store,
		stubAuthenticator{principal: Principal{Subject: testSubject, Roles: []string{BeneficiaryRole}}},
		blobs, scanner, testLimits(), starter, &fakeSignaler{}, testPolicyEnforcer(t))
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler
}

func seededApplication() cvff.ApplicationDetail {
	created, _ := time.Parse(time.RFC3339, "2026-08-01T09:30:00Z")
	entered, _ := time.Parse(time.RFC3339, testNow)
	return cvff.ApplicationDetail{
		Application: cvff.Application{
			ApplicationID: "cvff-app-001",
			ExternalRef:   "7f3a1c2e-9b0d-4e5f-a1b2-c3d4e5f60718",
			BeneficiaryID: testSubject,
			Amount:        750_000_000,
			Currency:      "NGN",
			State:         cvff.StateUnderwritingPrimary,
			CreatedAt:     created,
			UpdatedAt:     entered,
			Version:       3,
		},
		VesselName:       "MV Adaeze",
		IMONumber:        "9074729",
		OfficialNumber:   "NIMASA-4421",
		VesselClass:      cvff.VesselClassCargoCoaster,
		CabotageRoute:    cvff.RouteLagosOnne,
		BusinessName:     "Adaeze Coastal Logistics Ltd",
		BusinessRCNumber: "RC123456",
		BusinessAddress:  "14 Marina Road, Lagos Island, Lagos",
		StateEnteredAt:   entered,
	}
}

func TestNewHandlerFailClosed(t *testing.T) {
	store := &fakeStore{}
	blobs := &fakeBlobs{puts: map[string][]byte{}}
	auth := stubAuthenticator{principal: Principal{Subject: testSubject}}
	signaler := &fakeSignaler{}
	policy := testPolicyEnforcer(t)
	if _, err := NewHandler(nil, auth, blobs, fakeScanner{}, testLimits(), &fakeStarter{}, signaler, policy); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := NewHandler(store, nil, blobs, fakeScanner{}, testLimits(), &fakeStarter{}, signaler, policy); err == nil {
		t.Fatal("nil authenticator accepted")
	}
	if _, err := NewHandler(store, auth, nil, fakeScanner{}, testLimits(), &fakeStarter{}, signaler, policy); err == nil {
		t.Fatal("nil blob store accepted")
	}
	if _, err := NewHandler(store, auth, blobs, nil, testLimits(), &fakeStarter{}, signaler, policy); err == nil {
		t.Fatal("nil scanner accepted")
	}
	if _, err := NewHandler(store, auth, blobs, fakeScanner{}, Limits{}, &fakeStarter{}, signaler, policy); err == nil {
		t.Fatal("empty limits accepted")
	}
	if _, err := NewHandler(store, auth, blobs, fakeScanner{}, testLimits(), nil, signaler, policy); err == nil {
		t.Fatal("nil workflow starter accepted")
	}
	if _, err := NewHandler(store, auth, blobs, fakeScanner{}, testLimits(), &fakeStarter{}, nil, policy); err == nil {
		t.Fatal("nil workflow signaler accepted")
	}
	if _, err := NewHandler(store, auth, blobs, fakeScanner{}, testLimits(), &fakeStarter{}, signaler, nil); err == nil {
		t.Fatal("nil policy enforcer accepted")
	}
}

func TestHealthzUnauthenticated(t *testing.T) {
	handler := newTestHandler(t, &fakeStore{}, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz = %d", recorder.Code)
	}
}

func TestListApplicationsShape(t *testing.T) {
	store := &fakeStore{applications: []cvff.ApplicationDetail{seededApplication()}}
	handler := newTestHandler(t, store, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{})
	request := httptest.NewRequest(http.MethodGet, "/v1/cvff/applications", nil)
	request.Header.Set("Authorization", testBearer)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", recorder.Code, recorder.Body)
	}
	var summaries []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &summaries); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %d", len(summaries))
	}
	summary := summaries[0]
	// Contract: ApplicationSummary in the portal applications.ts.
	for _, key := range []string{"application_id", "vessel_name", "amount", "currency", "state", "state_entered_at", "created_at", "updated_at"} {
		if _, ok := summary[key]; !ok {
			t.Fatalf("summary missing %q: %v", key, summary)
		}
	}
	if summary["application_id"] != "cvff-app-001" || summary["vessel_name"] != "MV Adaeze" ||
		summary["amount"] != 750_000_000.0 || summary["currency"] != "NGN" ||
		summary["state"] != "UNDERWRITING_PRIMARY" || summary["state_entered_at"] != testNow {
		t.Fatalf("summary mismatch: %v", summary)
	}
}

func TestListApplicationsEmptyIsJSONArray(t *testing.T) {
	handler := newTestHandler(t, &fakeStore{}, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{})
	request := httptest.NewRequest(http.MethodGet, "/v1/cvff/applications", nil)
	request.Header.Set("Authorization", testBearer)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("empty list = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestGetApplicationDetailShape(t *testing.T) {
	store := &fakeStore{applications: []cvff.ApplicationDetail{seededApplication()}}
	handler := newTestHandler(t, store, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{})
	request := httptest.NewRequest(http.MethodGet, "/v1/cvff/applications/cvff-app-001", nil)
	request.Header.Set("Authorization", testBearer)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", recorder.Code, recorder.Body)
	}
	var detail map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	// Contract: ApplicationDetail in the portal applications.ts and the
	// mobile beneficiary store.
	for _, key := range []string{
		"application_id", "vessel_name", "amount", "currency", "state", "state_entered_at", "created_at", "updated_at",
		"imo_number", "official_number", "vessel_class", "cabotage_route", "business_name", "business_rc_number", "business_address",
	} {
		if _, ok := detail[key]; !ok {
			t.Fatalf("detail missing %q: %v", key, detail)
		}
	}
	if detail["imo_number"] != "9074729" || detail["vessel_class"] != "CARGO_COASTER" ||
		detail["cabotage_route"] != "LAGOS_ONNE" || detail["business_rc_number"] != "RC123456" {
		t.Fatalf("detail mismatch: %v", detail)
	}
}

func TestGetApplicationForeignOwnerFailsClosed404(t *testing.T) {
	foreign := seededApplication()
	foreign.BeneficiaryID = "kc-beneficiary-999"
	store := &fakeStore{applications: []cvff.ApplicationDetail{foreign}}
	handler := newTestHandler(t, store, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{})
	for _, path := range []string{
		"/v1/cvff/applications/cvff-app-001",
		"/v1/cvff/applications/cvff-app-001/events",
		"/v1/cvff/applications/cvff-app-001/documents",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", testBearer)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404 (no 403 ownership leak)", path, recorder.Code)
		}
		if contentType := recorder.Header().Get("Content-Type"); contentType != "application/problem+json" {
			t.Fatalf("404 content type = %q", contentType)
		}
	}
}

func TestListEventsShape(t *testing.T) {
	store := &fakeStore{
		applications: []cvff.ApplicationDetail{seededApplication()},
		approvals: []cvff.Approval{{
			ApprovalID:    "b0e0d1c2-1234-4e5f-a1b2-c3d4e5f60718",
			ApplicationID: "cvff-app-001",
			Role:          cvff.RoleUnderwriterPrimary,
			PrincipalID:   "kc-underwriter-7",
			Decision:      cvff.DecisionApprove,
			FromState:     cvff.StateUnderwritingPrimary,
			ToState:       cvff.StateUnderwritingSecondary,
			CreatedAt:     time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC),
		}},
	}
	handler := newTestHandler(t, store, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{})
	request := httptest.NewRequest(http.MethodGet, "/v1/cvff/applications/cvff-app-001/events", nil)
	request.Header.Set("Authorization", testBearer)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("events = %d: %s", recorder.Code, recorder.Body)
	}
	var events []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	event := events[0]
	// Contract: ApprovalEvent in the portal applications.ts and mobile store.
	for _, key := range []string{"approval_id", "application_id", "role", "principal_id", "decision", "from_state", "to_state", "created_at"} {
		if _, ok := event[key]; !ok {
			t.Fatalf("event missing %q: %v", key, event)
		}
	}
	if event["decision"] != "APPROVE" || event["from_state"] != "UNDERWRITING_PRIMARY" ||
		event["to_state"] != "UNDERWRITING_SECONDARY" || event["role"] != "UNDERWRITER_PRIMARY" {
		t.Fatalf("event mismatch: %v", event)
	}
}

func validCreateBody() string {
	return `{"vessel_name":"MV Adaeze","imo_number":"9074729","official_number":"NIMASA-4421","vessel_class":"CARGO_COASTER","cabotage_route":"LAGOS_ONNE","amount":750000000,"currency":"NGN","business_name":"Adaeze Coastal Logistics Ltd","business_rc_number":"RC123456","business_address":"14 Marina Road, Lagos Island, Lagos"}`
}

func TestCreateApplicationContract(t *testing.T) {
	store := &fakeStore{}
	handler := newTestHandler(t, store, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{})
	request := httptest.NewRequest(http.MethodPost, "/v1/cvff/applications", strings.NewReader(validCreateBody()))
	request.Header.Set("Authorization", testBearer)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "7f3a1c2e-9b0d-4e5f-a1b2-c3d4e5f60718")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", recorder.Code, recorder.Body)
	}
	if store.submitted.BeneficiaryID != testSubject || store.submitted.Amount != 750_000_000 ||
		store.submitted.IdempotencyKey != "7f3a1c2e-9b0d-4e5f-a1b2-c3d4e5f60718" || store.submitted.Currency != "NGN" {
		t.Fatalf("submitted intake mismatch: %+v", store.submitted)
	}
	var detail map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode created detail: %v", err)
	}
	if detail["application_id"] == "" || detail["state"] != "SUBMITTED" || detail["currency"] != "NGN" {
		t.Fatalf("created detail mismatch: %v", detail)
	}
}

func TestCreateApplicationIdempotentReplay(t *testing.T) {
	store := &fakeStore{applications: []cvff.ApplicationDetail{seededApplication()}}
	handler := newTestHandler(t, store, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{})
	// seededApplication external_ref is this key; replay returns the original.
	request := httptest.NewRequest(http.MethodPost, "/v1/cvff/applications", strings.NewReader(validCreateBody()))
	request.Header.Set("Authorization", testBearer)
	request.Header.Set("Idempotency-Key", "7f3a1c2e-9b0d-4e5f-a1b2-c3d4e5f60718")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("replay = %d: %s", recorder.Code, recorder.Body)
	}
	var detail map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode replay detail: %v", err)
	}
	if detail["application_id"] != "cvff-app-001" {
		t.Fatalf("replay did not return the original application: %v", detail)
	}
}

func TestCreateApplicationConflict(t *testing.T) {
	foreign := seededApplication()
	foreign.BeneficiaryID = "kc-beneficiary-999"
	store := &fakeStore{applications: []cvff.ApplicationDetail{foreign}}
	handler := newTestHandler(t, store, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{})
	request := httptest.NewRequest(http.MethodPost, "/v1/cvff/applications", strings.NewReader(validCreateBody()))
	request.Header.Set("Authorization", testBearer)
	request.Header.Set("Idempotency-Key", "7f3a1c2e-9b0d-4e5f-a1b2-c3d4e5f60718")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("conflicting key = %d, want 409", recorder.Code)
	}
}

func TestCreateApplicationFieldErrorsShape(t *testing.T) {
	store := &fakeStore{}
	handler := newTestHandler(t, store, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{})
	body := `{"vessel_name":"X","imo_number":"1234566","official_number":"!","vessel_class":"SUPERYACHT","cabotage_route":"DEEP_SEA","amount":10,"currency":"USD","business_name":"","business_rc_number":"123","business_address":"short"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/cvff/applications", strings.NewReader(body))
	request.Header.Set("Authorization", testBearer)
	request.Header.Set("Idempotency-Key", "7f3a1c2e-9b0d-4e5f-a1b2-c3d4e5f60718")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid create = %d: %s", recorder.Code, recorder.Body)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("422 content type = %q", contentType)
	}
	var problem map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem["title"] == nil || problem["status"] != 422.0 {
		t.Fatalf("problem document malformed: %v", problem)
	}
	errorsMap, ok := problem["errors"].(map[string]any)
	if !ok {
		t.Fatalf("problem errors missing: %v", problem)
	}
	// Keys are payload field names, as the portal mapServerErrors expects.
	for _, field := range []string{"vessel_name", "imo_number", "official_number", "vessel_class", "cabotage_route", "amount", "currency", "business_name", "business_rc_number", "business_address"} {
		if _, ok := errorsMap[field]; !ok {
			t.Fatalf("errors missing %q: %v", field, errorsMap)
		}
	}
}

func TestCreateApplicationRequiresIdempotencyKey(t *testing.T) {
	handler := newTestHandler(t, &fakeStore{}, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{})
	request := httptest.NewRequest(http.MethodPost, "/v1/cvff/applications", strings.NewReader(validCreateBody()))
	request.Header.Set("Authorization", testBearer)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing key = %d, want 422", recorder.Code)
	}
}

func multipartBody(t *testing.T, documentType string, fileName string, contentType string, content []byte) (string, []byte) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	if err := writer.WriteField("document_type", documentType); err != nil {
		t.Fatalf("write field: %v", err)
	}
	header := make(map[string][]string)
	header["Content-Disposition"] = []string{`form-data; name="file"; filename="` + fileName + `"`}
	header["Content-Type"] = []string{contentType}
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write content: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return writer.FormDataContentType(), buffer.Bytes()
}

func TestUploadDocumentContract(t *testing.T) {
	store := &fakeStore{applications: []cvff.ApplicationDetail{seededApplication()}}
	blobs := &fakeBlobs{puts: map[string][]byte{}}
	handler := newTestHandler(t, store, blobs, fakeScanner{})
	contentType, body := multipartBody(t, "VESSEL_REGISTRATION", "vessel-registration.pdf", "application/pdf", []byte("pdf-bytes"))
	request := httptest.NewRequest(http.MethodPost, "/v1/cvff/applications/cvff-app-001/documents", bytes.NewReader(body))
	request.Header.Set("Authorization", testBearer)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Idempotency-Key", "doc-key-001")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", recorder.Code, recorder.Body)
	}
	var document map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	// Contract: UploadedDocument in the portal applications.ts.
	for _, key := range []string{"document_id", "application_id", "document_type", "file_name", "content_type", "size_bytes", "uploaded_at"} {
		if _, ok := document[key]; !ok {
			t.Fatalf("document missing %q: %v", key, document)
		}
	}
	if document["document_type"] != "VESSEL_REGISTRATION" || document["file_name"] != "vessel-registration.pdf" ||
		document["content_type"] != "application/pdf" || document["size_bytes"] != 9.0 {
		t.Fatalf("document mismatch: %v", document)
	}
	// Bytes are stored under the content-addressed key recorded in metadata.
	if len(store.createdDocs) != 1 {
		t.Fatalf("created docs = %d", len(store.createdDocs))
	}
	stored, ok := blobs.puts[store.createdDocs[0].StorageKey]
	if !ok || string(stored) != "pdf-bytes" {
		t.Fatalf("object store mismatch: %v", blobs.puts)
	}
	if !strings.HasPrefix(store.createdDocs[0].StorageKey, "cvff-documents/cvff-app-001/") {
		t.Fatalf("storage key not content-addressed under the application: %q", store.createdDocs[0].StorageKey)
	}
}

func TestUploadDocumentIdempotentReplay(t *testing.T) {
	existing := cvff.Document{
		DocumentID:     "doc-existing",
		ApplicationID:  "cvff-app-001",
		BeneficiaryID:  testSubject,
		DocumentType:   "VESSEL_REGISTRATION",
		FileName:       "vessel-registration.pdf",
		ContentType:    "application/pdf",
		SizeBytes:      9,
		SHA256Hex:      "6f74497c0b5d2f5b7d5b5c1ad3f7b6f58f4f0c0a3c69a6f9c1c8dbb1b8fb2d0b",
		StorageBackend: "s3",
		StorageKey:     "cvff-documents/cvff-app-001/6f74",
		IdempotencyKey: "doc-key-001",
		CreatedAt:      time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC),
	}
	content := []byte("pdf-bytes")
	sum := sha256Of(content)
	existing.SHA256Hex = sum
	store := &fakeStore{
		applications: []cvff.ApplicationDetail{seededApplication()},
		documents:    []cvff.Document{existing},
	}
	blobs := &fakeBlobs{puts: map[string][]byte{}}
	handler := newTestHandler(t, store, blobs, fakeScanner{})
	contentType, body := multipartBody(t, "VESSEL_REGISTRATION", "vessel-registration.pdf", "application/pdf", content)
	request := httptest.NewRequest(http.MethodPost, "/v1/cvff/applications/cvff-app-001/documents", bytes.NewReader(body))
	request.Header.Set("Authorization", testBearer)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Idempotency-Key", "doc-key-001")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("replay upload = %d: %s", recorder.Code, recorder.Body)
	}
	var document map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode replay document: %v", err)
	}
	if document["document_id"] != "doc-existing" {
		t.Fatalf("replay returned a different document: %v", document)
	}
}

func sha256Of(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func TestUploadDocumentFailClosedMatrix(t *testing.T) {
	content := []byte("pdf-bytes")
	cases := map[string]struct {
		documentType string
		fileName     string
		contentType  string
		content      []byte
		scanner      Scanner
		blobsErr     error
		want         int
	}{
		"unapproved document type": {"PASSPORT", "file.pdf", "application/pdf", content, fakeScanner{}, nil, http.StatusUnprocessableEntity},
		"unapproved content type":  {"VESSEL_REGISTRATION", "file.exe", "application/x-msdownload", content, fakeScanner{}, nil, http.StatusUnprocessableEntity},
		"empty file":               {"VESSEL_REGISTRATION", "file.pdf", "application/pdf", []byte{}, fakeScanner{}, nil, http.StatusUnprocessableEntity},
		"scanner infected verdict": {"VESSEL_REGISTRATION", "file.pdf", "application/pdf", content, fakeScanner{err: ErrDocumentInfected}, nil, http.StatusUnprocessableEntity},
		"scanner unavailable":      {"VESSEL_REGISTRATION", "file.pdf", "application/pdf", content, fakeScanner{err: ErrScannerUnavailable}, nil, http.StatusServiceUnavailable},
		"object storage failure":   {"VESSEL_REGISTRATION", "file.pdf", "application/pdf", content, fakeScanner{}, errors.New("backend down"), http.StatusBadGateway},
		"missing idempotency key":  {"VESSEL_REGISTRATION", "file.pdf", "application/pdf", content, fakeScanner{}, nil, http.StatusUnprocessableEntity},
	}
	for name, testCase := range cases {
		store := &fakeStore{applications: []cvff.ApplicationDetail{seededApplication()}}
		handler := newTestHandler(t, store, &fakeBlobs{puts: map[string][]byte{}, err: testCase.blobsErr}, testCase.scanner)
		contentType, body := multipartBody(t, testCase.documentType, testCase.fileName, testCase.contentType, testCase.content)
		request := httptest.NewRequest(http.MethodPost, "/v1/cvff/applications/cvff-app-001/documents", bytes.NewReader(body))
		request.Header.Set("Authorization", testBearer)
		request.Header.Set("Content-Type", contentType)
		if name != "missing idempotency key" {
			request.Header.Set("Idempotency-Key", "doc-key-"+strings.ReplaceAll(name, " ", "-"))
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != testCase.want {
			t.Fatalf("case %s: status = %d, want %d (%s)", name, recorder.Code, testCase.want, recorder.Body)
		}
		if len(store.createdDocs) != 0 {
			t.Fatalf("case %s: document persisted despite rejection", name)
		}
	}
}

func TestUploadDocumentQuota(t *testing.T) {
	store := &fakeStore{applications: []cvff.ApplicationDetail{seededApplication()}}
	for index := 0; index < testLimits().MaxDocumentsPerApplication; index++ {
		store.documents = append(store.documents, cvff.Document{
			DocumentID:    "doc-full",
			ApplicationID: "cvff-app-001",
			BeneficiaryID: testSubject,
		})
	}
	handler := newTestHandler(t, store, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{})
	contentType, body := multipartBody(t, "BANK_DETAILS", "bank-details.pdf", "application/pdf", []byte("pdf-bytes"))
	request := httptest.NewRequest(http.MethodPost, "/v1/cvff/applications/cvff-app-001/documents", bytes.NewReader(body))
	request.Header.Set("Authorization", testBearer)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Idempotency-Key", "doc-key-quota")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("quota breach = %d, want 409", recorder.Code)
	}
}

func TestUploadDocumentOversize(t *testing.T) {
	store := &fakeStore{applications: []cvff.ApplicationDetail{seededApplication()}}
	handler := newTestHandler(t, store, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{})
	oversize := make([]byte, testDocBytes+1)
	contentType, body := multipartBody(t, "BANK_DETAILS", "bank-details.pdf", "application/pdf", oversize)
	request := httptest.NewRequest(http.MethodPost, "/v1/cvff/applications/cvff-app-001/documents", bytes.NewReader(body))
	request.Header.Set("Authorization", testBearer)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Idempotency-Key", "doc-key-oversize")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge && recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversize upload = %d, want 413 or 422", recorder.Code)
	}
	if len(store.createdDocs) != 0 {
		t.Fatal("oversize document persisted")
	}
}

func TestListDocumentsShape(t *testing.T) {
	store := &fakeStore{
		applications: []cvff.ApplicationDetail{seededApplication()},
		documents: []cvff.Document{{
			DocumentID:     "doc-001",
			ApplicationID:  "cvff-app-001",
			BeneficiaryID:  testSubject,
			DocumentType:   "CABOTAGE_LICENSE",
			FileName:       "cabotage-license.pdf",
			ContentType:    "application/pdf",
			SizeBytes:      1024,
			SHA256Hex:      strings.Repeat("b", 64),
			StorageBackend: "s3",
			StorageKey:     "cvff-documents/cvff-app-001/" + strings.Repeat("b", 64),
			IdempotencyKey: "doc-key-001",
			CreatedAt:      time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC),
		}},
	}
	handler := newTestHandler(t, store, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{})
	request := httptest.NewRequest(http.MethodGet, "/v1/cvff/applications/cvff-app-001/documents", nil)
	request.Header.Set("Authorization", testBearer)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("documents = %d: %s", recorder.Code, recorder.Body)
	}
	var documents []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &documents); err != nil {
		t.Fatalf("decode documents: %v", err)
	}
	if len(documents) != 1 {
		t.Fatalf("documents = %d", len(documents))
	}
	document := documents[0]
	for _, key := range []string{"document_id", "application_id", "document_type", "file_name", "content_type", "size_bytes", "uploaded_at"} {
		if _, ok := document[key]; !ok {
			t.Fatalf("document missing %q: %v", key, document)
		}
	}
	if document["uploaded_at"] != "2026-08-21T09:00:00Z" || document["document_type"] != "CABOTAGE_LICENSE" {
		t.Fatalf("document mismatch: %v", document)
	}
}
