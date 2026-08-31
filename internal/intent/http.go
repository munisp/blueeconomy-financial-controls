package intent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvffapi"
	"github.com/munisp/blueeconomy-financial-controls/internal/pbac"
)

// APIStore is the persistence boundary used by the HTTP API.
type APIStore interface {
	Create(ctx context.Context, request CreateRequest) (Intent, error)
	Approve(ctx context.Context, intentID string, expectedVersion int64, checker string) (Intent, error)
	VoidDraft(ctx context.Context, intentID string, expectedVersion int64, actor string) (Intent, error)
}

// AmbiguousResolver applies officer dispositions to AMBIGUOUS intents,
// including the compensating ledger handling a VOID disposition requires.
// The orchestration Resolver implements it against the real TigerBeetle
// ledger.
type AmbiguousResolver interface {
	ResolveAmbiguous(ctx context.Context, intentID string, expectedVersion int64, officer string, resolution Resolution) (Intent, error)
}

// Handler implements the openapi.yaml financial-intent contract. Every money
// route is gated by Keycloak bearer verification, a realm-role binding and
// the PBAC policy layer; actor identity (maker/checker/officer) is derived
// from the verified token subject only, never from the request body.
type Handler struct {
	store         APIStore
	authenticator cvffapi.Authenticator
	policy        *pbac.Enforcer
	resolver      AmbiguousResolver
	mux           *http.ServeMux
}

// NewHandler fails closed when any dependency is absent: there is no default
// store, no default authenticator, no default authorization policy and no
// default officer resolver.
func NewHandler(store APIStore, authenticator cvffapi.Authenticator, policy *pbac.Enforcer, resolver AmbiguousResolver) (*Handler, error) {
	if store == nil {
		return nil, errors.New("intent store is required")
	}
	if authenticator == nil {
		return nil, errors.New("keycloak authenticator is required")
	}
	if policy == nil {
		return nil, errors.New("authorization policy enforcer is required; requests are denied without one")
	}
	if resolver == nil {
		return nil, errors.New("ambiguous-intent resolver is required; officer resolution must never be a state-only edit")
	}
	handler := &Handler{store: store, authenticator: authenticator, policy: policy, resolver: resolver, mux: http.NewServeMux()}
	handler.mux.HandleFunc("GET /healthz", handler.health)
	handler.mux.Handle("POST /v1/financial-intents", handler.requireAccess(IntentMakerRole, "create", handler.create))
	handler.mux.Handle("POST /v1/financial-intents/{intent_id}/approve", handler.requireAccess(IntentCheckerRole, "approve", handler.approve))
	handler.mux.Handle("POST /v1/financial-intents/{intent_id}/void", handler.requireAccess(IntentMakerRole, "void", handler.voidDraft))
	handler.mux.Handle("POST /v1/financial-intents/{intent_id}/resolve", handler.requireAccess(FinancialControllerRole, "resolve", handler.resolve))
	return handler, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handler.mux.ServeHTTP(writer, request)
}

func (handler *Handler) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

// createIntentRequest is the create body contract. There is deliberately no
// maker field: the maker is the verified token subject. A body-supplied maker
// is rejected as an unknown field.
type createIntentRequest struct {
	IntentID        string `json:"intent_id"`
	ExternalRef     string `json:"external_ref"`
	DebitAccountID  string `json:"debit_account_id"`
	CreditAccountID string `json:"credit_account_id"`
	Amount          uint64 `json:"amount"`
	Ledger          uint32 `json:"ledger"`
	Code            uint16 `json:"code"`
	Currency        string `json:"currency"`
}

func (handler *Handler) create(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	var payload createIntentRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	createRequest := CreateRequest{
		IntentID:        payload.IntentID,
		ExternalRef:     payload.ExternalRef,
		DebitAccountID:  payload.DebitAccountID,
		CreditAccountID: payload.CreditAccountID,
		Amount:          payload.Amount,
		Ledger:          payload.Ledger,
		Code:            payload.Code,
		Currency:        payload.Currency,
		Maker:           principal.Subject,
	}
	if err := createRequest.Validate(); err != nil {
		writeError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	created, err := handler.store.Create(request.Context(), createRequest)
	if err != nil {
		if errors.Is(err, ErrImmutableConflict) {
			writeError(writer, http.StatusConflict, err)
			return
		}
		writeError(writer, http.StatusInternalServerError, errors.New("create financial intent"))
		return
	}
	writeJSON(writer, http.StatusCreated, created)
}

// approvalRequest is the approve body contract. There is deliberately no
// checker field: the checker is the verified token subject and the store
// enforces maker != checker on the verified principals. A body-supplied
// checker is rejected as an unknown field.
type approvalRequest struct {
	ExpectedVersion int64 `json:"expected_version"`
}

func (handler *Handler) approve(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	intentID := request.PathValue("intent_id")
	if err := ValidateIdentifier("intent_id", intentID); err != nil {
		writeError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	var approval approvalRequest
	if err := decodeJSON(request, &approval); err != nil {
		writeError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	if approval.ExpectedVersion <= 0 {
		writeError(writer, http.StatusUnprocessableEntity, errors.New("expected_version is required"))
		return
	}
	updated, err := handler.store.Approve(request.Context(), intentID, approval.ExpectedVersion, principal.Subject)
	if err != nil {
		switch {
		case errors.Is(err, ErrMakerChecker), errors.Is(err, ErrConflict), errors.Is(err, ErrInvalidState):
			writeError(writer, http.StatusConflict, err)
		case errors.Is(err, ErrNotFound):
			writeError(writer, http.StatusNotFound, err)
		default:
			writeError(writer, http.StatusInternalServerError, errors.New("approve financial intent"))
		}
		return
	}
	writeJSON(writer, http.StatusOK, updated)
}

// voidRequest is the DRAFT-void body contract. The actor is the verified
// token subject and must be the recorded maker; the body carries no actor
// identity.
type voidRequest struct {
	ExpectedVersion int64 `json:"expected_version"`
}

func (handler *Handler) voidDraft(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	intentID := request.PathValue("intent_id")
	if err := ValidateIdentifier("intent_id", intentID); err != nil {
		writeError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	var payload voidRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	if payload.ExpectedVersion <= 0 {
		writeError(writer, http.StatusUnprocessableEntity, errors.New("expected_version is required"))
		return
	}
	updated, err := handler.store.VoidDraft(request.Context(), intentID, payload.ExpectedVersion, principal.Subject)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotMaker):
			writeError(writer, http.StatusForbidden, err)
		case errors.Is(err, ErrNotFound):
			writeError(writer, http.StatusNotFound, err)
		case errors.Is(err, ErrInvalidState), errors.Is(err, ErrConflict):
			writeError(writer, http.StatusConflict, err)
		default:
			writeError(writer, http.StatusInternalServerError, errors.New("void draft financial intent"))
		}
		return
	}
	writeJSON(writer, http.StatusOK, updated)
}

// resolutionRequest is the officer-resolution body contract. The officer is
// the verified token subject, recorded in the audit event; the body carries
// no actor identity.
type resolutionRequest struct {
	ExpectedVersion int64      `json:"expected_version"`
	Resolution      Resolution `json:"resolution"`
}

func (handler *Handler) resolve(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	intentID := request.PathValue("intent_id")
	if err := ValidateIdentifier("intent_id", intentID); err != nil {
		writeError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	var payload resolutionRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeError(writer, http.StatusUnprocessableEntity, err)
		return
	}
	if payload.ExpectedVersion <= 0 {
		writeError(writer, http.StatusUnprocessableEntity, errors.New("expected_version is required"))
		return
	}
	updated, err := handler.resolver.ResolveAmbiguous(request.Context(), intentID, payload.ExpectedVersion, principal.Subject, payload.Resolution)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidResolution):
			writeError(writer, http.StatusUnprocessableEntity, err)
		case errors.Is(err, ErrNotFound):
			writeError(writer, http.StatusNotFound, err)
		case errors.Is(err, ErrInvalidState), errors.Is(err, ErrConflict), errors.Is(err, ErrResolutionRejected):
			writeError(writer, http.StatusConflict, err)
		default:
			writeError(writer, http.StatusInternalServerError, errors.New("resolve financial intent"))
		}
		return
	}
	writeJSON(writer, http.StatusOK, updated)
}

// ValidateIdentifier reports whether value is canonical identifier text.
func ValidateIdentifier(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 256 || !idPattern.MatchString(value) {
		return errors.New(name + " is not canonical approved identifier text")
	}
	return nil
}

func decodeJSON(request *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("request body is not valid contract JSON")
	}
	if decoder.More() {
		return errors.New("request body must contain exactly one JSON document")
	}
	return nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, map[string]string{"error": err.Error()})
}
