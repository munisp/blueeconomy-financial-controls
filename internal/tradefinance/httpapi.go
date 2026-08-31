package tradefinance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Principal is the verified identity behind one trader/operations request.
type Principal struct {
	Subject string
	Roles   []string
}

// HasRole reports whether the principal holds the given realm role.
func (principal Principal) HasRole(role string) bool {
	for _, held := range principal.Roles {
		if held == role {
			return true
		}
	}
	return false
}

// Realm roles bound by GitOps.
const (
	RoleNameTrader            = "trader"
	RoleNameComplianceChecker = "compliance-checker"
	RoleNameBankOfficer       = "bank-officer"
	RoleNameAuditor           = "auditor"
)

// Authenticator verifies the Authorization header of one request.
type Authenticator interface {
	Authenticate(ctx context.Context, authorizationHeader string) (Principal, error)
}

var (
	ErrUnauthenticated = errors.New("request is unauthenticated")
	ErrForbidden       = errors.New("principal does not hold the required role")
)

// HTTPAPI is the trader/operations-facing surface of the trade-finance rail.
type HTTPAPI struct {
	store         *Store
	authenticator Authenticator
}

// NewHTTPAPI fails closed on missing dependencies.
func NewHTTPAPI(store *Store, authenticator Authenticator) (*HTTPAPI, error) {
	if store == nil || authenticator == nil {
		return nil, errors.New("tradefinance HTTP API store and authenticator are required")
	}
	return &HTTPAPI{store: store, authenticator: authenticator}, nil
}

// Mux returns the trader/operations routes.
func (api *HTTPAPI) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tradefinance/consents", api.handleRequestConsent)
	mux.HandleFunc("GET /v1/tradefinance/consents", api.handleListConsents)
	mux.HandleFunc("GET /v1/tradefinance/consents/{consentID}", api.handleGetConsent)
	mux.HandleFunc("GET /v1/tradefinance/consents/{consentID}/audit", api.handleConsentAudit)
	mux.HandleFunc("POST /v1/tradefinance/consents/{consentID}/activate", api.consentMove(api.store.ActivateConsent))
	mux.HandleFunc("POST /v1/tradefinance/consents/{consentID}/reject", api.consentMove(api.store.RejectConsent))
	mux.HandleFunc("POST /v1/tradefinance/consents/{consentID}/request-revocation", api.consentMove(api.store.RequestRevocation))
	mux.HandleFunc("POST /v1/tradefinance/consents/{consentID}/confirm-revocation", api.consentMove(api.store.ConfirmRevocation))
	mux.HandleFunc("POST /v1/tradefinance/consents/{consentID}/reject-revocation", api.consentMove(api.store.RejectRevocation))
	mux.HandleFunc("POST /v1/tradefinance/applications", api.handleSubmitApplication)
	mux.HandleFunc("GET /v1/tradefinance/applications", api.handleListApplications)
	mux.HandleFunc("GET /v1/tradefinance/applications/{applicationID}", api.handleGetApplication)
	mux.HandleFunc("POST /v1/tradefinance/applications/{applicationID}/decisions", api.handleDecision)
	return mux
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func (api *HTTPAPI) principal(writer http.ResponseWriter, request *http.Request, role string) (Principal, bool) {
	principal, err := api.authenticator.Authenticate(request.Context(), request.Header.Get("Authorization"))
	if err != nil || principal.Subject == "" {
		problem(writer, http.StatusUnauthorized, "authentication required")
		return Principal{}, false
	}
	if role != "" && !principal.HasRole(role) {
		problem(writer, http.StatusForbidden, fmt.Sprintf("role %s required", role))
		return Principal{}, false
	}
	return principal, true
}

type consentRequestBody struct {
	ConsentID   string              `json:"consent_id"`
	TraderID    string              `json:"trader_id"`
	BankID      string              `json:"bank_id"`
	Scopes      []Scope             `json:"scopes"`
	DatasetRefs map[string][]string `json:"dataset_refs"`
	ExpiresAt   time.Time           `json:"expires_at"`
}

func (api *HTTPAPI) handleRequestConsent(writer http.ResponseWriter, request *http.Request) {
	principal, ok := api.principal(writer, request, RoleNameTrader)
	if !ok {
		return
	}
	var body consentRequestBody
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		problem(writer, http.StatusBadRequest, "malformed consent request")
		return
	}
	// The trader principal can only grant access to its own datasets.
	if body.TraderID != principal.Subject {
		problem(writer, http.StatusForbidden, "trader_id must be the authenticated trader")
		return
	}
	retained, err := api.store.RequestConsent(request.Context(), Consent{
		ConsentID: body.ConsentID, TraderID: body.TraderID, BankID: body.BankID,
		Scopes: body.Scopes, DatasetRefs: body.DatasetRefs, ExpiresAt: body.ExpiresAt,
		MakerPrincipal: principal.Subject,
	}, time.Now().UTC())
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, retained)
}

type versionBody struct {
	Version int64 `json:"version"`
}

// consentMove adapts a store maker-checker move to HTTP. Grant moves require
// the trader role (request/revoke-request) and checker moves require the
// compliance-checker role; the store enforces maker≠checker durably.
func (api *HTTPAPI) consentMove(move func(context.Context, string, int64, string) (Consent, error)) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		principal, err := api.authenticator.Authenticate(request.Context(), request.Header.Get("Authorization"))
		if err != nil || principal.Subject == "" {
			problem(writer, http.StatusUnauthorized, "authentication required")
			return
		}
		if !principal.HasRole(RoleNameTrader) && !principal.HasRole(RoleNameComplianceChecker) {
			problem(writer, http.StatusForbidden, "trader or compliance-checker role required")
			return
		}
		var body versionBody
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Version <= 0 {
			problem(writer, http.StatusBadRequest, "a positive version is required")
			return
		}
		retained, err := move(request.Context(), request.PathValue("consentID"), body.Version, principal.Subject)
		if err != nil {
			writeStoreError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, retained)
	}
}

// consentReadable reports whether the principal may read the consent: the
// consenting trader, a compliance checker or an auditor.
func consentReadable(principal Principal, consent Consent) bool {
	return consent.MakerPrincipal == principal.Subject || principal.HasRole(RoleNameComplianceChecker) || principal.HasRole(RoleNameAuditor)
}

func (api *HTTPAPI) handleGetConsent(writer http.ResponseWriter, request *http.Request) {
	principal, ok := api.principal(writer, request, "")
	if !ok {
		return
	}
	consent, err := api.store.GetConsent(request.Context(), request.PathValue("consentID"))
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	if !consentReadable(principal, consent) {
		problem(writer, http.StatusForbidden, "consent is not readable by this principal")
		return
	}
	writeJSON(writer, http.StatusOK, consent)
}

func (api *HTTPAPI) handleListConsents(writer http.ResponseWriter, request *http.Request) {
	principal, ok := api.principal(writer, request, "")
	if !ok {
		return
	}
	traderID := request.URL.Query().Get("trader_id")
	if traderID == "" {
		problem(writer, http.StatusBadRequest, "trader_id is required")
		return
	}
	if traderID != principal.Subject && !principal.HasRole(RoleNameComplianceChecker) && !principal.HasRole(RoleNameAuditor) {
		problem(writer, http.StatusForbidden, "cannot list another trader's consents")
		return
	}
	consents, err := api.store.ListConsents(request.Context(), traderID)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"consents": consents})
}

func (api *HTTPAPI) handleConsentAudit(writer http.ResponseWriter, request *http.Request) {
	principal, ok := api.principal(writer, request, "")
	if !ok {
		return
	}
	consent, err := api.store.GetConsent(request.Context(), request.PathValue("consentID"))
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	if !consentReadable(principal, consent) {
		problem(writer, http.StatusForbidden, "consent audit is not readable by this principal")
		return
	}
	entries, err := api.store.ConsentAudit(request.Context(), consent.ConsentID)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	if err := VerifyAuditChain(entries); err != nil {
		problem(writer, http.StatusInternalServerError, "consent audit chain verification failed")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"audit": entries, "chain_verified": true})
}

type applicationRequestBody struct {
	ApplicationID string          `json:"application_id"`
	ExternalRef   string          `json:"external_ref"`
	TraderID      string          `json:"trader_id"`
	BankID        string          `json:"bank_id"`
	ConsentID     string          `json:"consent_id"`
	Product       Product         `json:"product"`
	Amount        uint64          `json:"amount"`
	Currency      string          `json:"currency"`
	Assignments   map[Role]string `json:"assignments"`
}

func (api *HTTPAPI) handleSubmitApplication(writer http.ResponseWriter, request *http.Request) {
	principal, ok := api.principal(writer, request, RoleNameTrader)
	if !ok {
		return
	}
	var body applicationRequestBody
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		problem(writer, http.StatusBadRequest, "malformed application request")
		return
	}
	if body.TraderID != principal.Subject {
		problem(writer, http.StatusForbidden, "trader_id must be the authenticated trader")
		return
	}
	// The trader party in the chain is the authenticated trader.
	body.Assignments[RoleTrader] = principal.Subject
	retained, err := api.store.SubmitApplication(request.Context(), Application{
		ApplicationID: body.ApplicationID, ExternalRef: body.ExternalRef, TraderID: body.TraderID,
		BankID: body.BankID, ConsentID: body.ConsentID, Product: body.Product,
		Amount: body.Amount, Currency: body.Currency,
	}, body.Assignments)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, retained)
}

func (api *HTTPAPI) handleGetApplication(writer http.ResponseWriter, request *http.Request) {
	principal, ok := api.principal(writer, request, "")
	if !ok {
		return
	}
	application, err := api.store.GetApplication(request.Context(), request.PathValue("applicationID"))
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	if !api.applicationReadable(principal, application) {
		problem(writer, http.StatusForbidden, "application is not readable by this principal")
		return
	}
	approvals, err := api.store.ListApprovals(request.Context(), application.ApplicationID)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"application": application, "approvals": approvals})
}

func (api *HTTPAPI) applicationReadable(principal Principal, application Application) bool {
	if application.TraderID == principal.Subject {
		return true
	}
	if principal.HasRole(RoleNameAuditor) || principal.HasRole(RoleNameComplianceChecker) || principal.HasRole(RoleNameBankOfficer) {
		return true
	}
	assignments, err := api.store.RoleAssignments(context.Background(), application.ApplicationID)
	if err != nil {
		return false
	}
	for _, holder := range assignments {
		if holder == principal.Subject {
			return true
		}
	}
	return false
}

func (api *HTTPAPI) handleListApplications(writer http.ResponseWriter, request *http.Request) {
	principal, ok := api.principal(writer, request, "")
	if !ok {
		return
	}
	traderID := request.URL.Query().Get("trader_id")
	if traderID == "" {
		problem(writer, http.StatusBadRequest, "trader_id is required")
		return
	}
	if traderID != principal.Subject && !principal.HasRole(RoleNameComplianceChecker) && !principal.HasRole(RoleNameAuditor) {
		problem(writer, http.StatusForbidden, "cannot list another trader's applications")
		return
	}
	applications, err := api.store.ListApplications(request.Context(), traderID)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"applications": applications})
}

type decisionBody struct {
	Version  int64    `json:"version"`
	Decision Decision `json:"decision"`
}

func (api *HTTPAPI) handleDecision(writer http.ResponseWriter, request *http.Request) {
	principal, err := api.authenticator.Authenticate(request.Context(), request.Header.Get("Authorization"))
	if err != nil || principal.Subject == "" {
		problem(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	if !principal.HasRole(RoleNameTrader) && !principal.HasRole(RoleNameBankOfficer) && !principal.HasRole(RoleNameComplianceChecker) {
		problem(writer, http.StatusForbidden, "a deciding role is required")
		return
	}
	var body decisionBody
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Version <= 0 {
		problem(writer, http.StatusBadRequest, "a positive version is required")
		return
	}
	retained, approval, err := api.store.RecordDecision(request.Context(), request.PathValue("applicationID"), body.Version, principal.Subject, body.Decision)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"application": retained, "approval": approval})
}

// writeStoreError maps domain failures to statuses without leaking internals.
func writeStoreError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrConsentNotFound), errors.Is(err, ErrNotFound):
		problem(writer, http.StatusNotFound, "resource not found")
	case errors.Is(err, ErrConsentConflict), errors.Is(err, ErrConflict):
		problem(writer, http.StatusConflict, "concurrent modification or duplicate request")
	case errors.Is(err, ErrMakerChecker), errors.Is(err, ErrRoleSeparation), errors.Is(err, ErrRoleNotAssigned),
		errors.Is(err, ErrConsentRequired), errors.Is(err, ErrScopeNotConsented):
		problem(writer, http.StatusForbidden, "separation-of-duties or consent requirement not satisfied")
	case errors.Is(err, ErrTerminalState), errors.Is(err, ErrConsentTerminalState),
		errors.Is(err, ErrSequenceViolation), errors.Is(err, ErrConsentState), errors.Is(err, ErrInvalidState):
		problem(writer, http.StatusUnprocessableEntity, "invalid lifecycle transition")
	default:
		problem(writer, http.StatusBadRequest, "request rejected")
	}
}
