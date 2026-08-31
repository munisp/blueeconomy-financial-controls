package revenue

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
	"github.com/munisp/blueeconomy-financial-controls/internal/tariff"
)

// Handler exposes the revenue-assurance chain over HTTP. Every endpoint
// except /healthz requires a verified bearer token (the tariff
// Authenticator contract — the verified subject is the maker/checker/actor
// identity, so dual control cannot be faked).
type Handler struct {
	authenticator tariff.Authenticator
	store         *Store
	verifier      *envelope.Verifier
	logger        *slog.Logger
	mux           *http.ServeMux
	now           func() time.Time
}

// NewHandler fails closed on any missing dependency.
func NewHandler(authenticator tariff.Authenticator, store *Store, verifier *envelope.Verifier, logger *slog.Logger) (*Handler, error) {
	if authenticator == nil {
		return nil, errors.New("authenticator is required")
	}
	if store == nil {
		return nil, errors.New("store is required")
	}
	if logger == nil {
		return nil, errors.New("logger is required")
	}
	handler := &Handler{
		authenticator: authenticator,
		store:         store,
		verifier:      verifier,
		logger:        logger,
		mux:           http.NewServeMux(),
		now:           func() time.Time { return time.Now().UTC() },
	}
	handler.mux.HandleFunc("/healthz", handler.healthz)
	handler.mux.HandleFunc("POST /v1/revenue/debit-notes", handler.createDebitNote)
	handler.mux.HandleFunc("POST /v1/revenue/debit-notes/{id}/issue", handler.issueDebitNote)
	handler.mux.HandleFunc("POST /v1/revenue/debit-notes/{id}/transitions", handler.transitionDebitNote)
	handler.mux.HandleFunc("GET /v1/revenue/debit-notes/{id}", handler.getDebitNote)
	handler.mux.HandleFunc("GET /v1/revenue/debit-notes/{id}/envelope", handler.getDebitNoteEnvelope)
	handler.mux.HandleFunc("GET /v1/revenue/debit-notes/{id}/transitions", handler.listTransitions)
	handler.mux.HandleFunc("POST /v1/revenue/tsa/split-rules", handler.createSplitRule)
	handler.mux.HandleFunc("POST /v1/revenue/tsa/split-rules/{id}/activate", handler.activateSplitRule)
	handler.mux.HandleFunc("POST /v1/revenue/tsa/compute", handler.computeSplit)
	handler.mux.HandleFunc("POST /v1/revenue/tsa/remittance-advices", handler.issueAdvice)
	handler.mux.HandleFunc("POST /v1/revenue/settlements", handler.recordSettlement)
	handler.mux.HandleFunc("POST /v1/revenue/statements", handler.ingestStatement)
	handler.mux.HandleFunc("POST /v1/revenue/recon/runs", handler.runRecon)
	handler.mux.HandleFunc("GET /v1/revenue/recon/exceptions", handler.listExceptions)
	handler.mux.HandleFunc("POST /v1/revenue/recon/exceptions/{id}/resolve", handler.resolveException)
	handler.mux.HandleFunc("GET /v1/revenue/reports/agency", handler.reportAgency)
	handler.mux.HandleFunc("GET /v1/revenue/reports/lines", handler.reportLines)
	handler.mux.HandleFunc("GET /v1/revenue/reports/aging", handler.reportAging)
	handler.mux.HandleFunc("GET /v1/revenue/reports/exceptions", handler.reportExceptions)
	return handler, nil
}

// ServeHTTP dispatches the mux.
func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handler.mux.ServeHTTP(writer, request)
}

func (handler *Handler) healthz(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (handler *Handler) subject(writer http.ResponseWriter, request *http.Request) (string, bool) {
	subject, err := handler.authenticator.Authenticate(request.Context(), request.Header.Get("Authorization"))
	if err != nil {
		handler.logger.Warn("revenue request rejected", "path", request.URL.Path, "error", err.Error())
		writeError(writer, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	return subject, true
}

func idempotencyKey(request *http.Request) string {
	return strings.TrimSpace(request.Header.Get("Idempotency-Key"))
}

func correlationID(request *http.Request) string {
	if value := strings.TrimSpace(request.Header.Get("X-Correlation-Id")); value != "" {
		return value
	}
	return "corr-" + strings.ReplaceAll(request.Header.Get("X-Request-Id"), " ", "")
}

func (handler *Handler) createDebitNote(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	var body IssueRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	note, err := handler.store.CreateDebitNote(request.Context(), body, idempotencyKey(request), subject, correlationID(request))
	respond(writer, note, err)
}

func (handler *Handler) issueDebitNote(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	note, err := handler.store.Issue(request.Context(), request.PathValue("id"), subject)
	respond(writer, note, err)
}

func (handler *Handler) transitionDebitNote(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	var body TransitionRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	note, err := handler.store.transitionTraced(request.Context(), request.PathValue("id"), body, subject)
	respond(writer, note, err)
}

func (handler *Handler) getDebitNote(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	note, err := handler.store.GetDebitNote(request.Context(), request.PathValue("id"))
	respond(writer, note, err)
}

func (handler *Handler) getDebitNoteEnvelope(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	bundle, _, err := handler.store.GetDebitNoteEnvelope(request.Context(), request.PathValue("id"))
	if err != nil {
		respond(writer, nil, err)
		return
	}
	writer.Header().Set("Content-Type", "application/fhir+json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(bundle)
}

func (handler *Handler) listTransitions(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	transitions, err := handler.store.ListTransitions(request.Context(), request.PathValue("id"))
	respond(writer, map[string]any{"transitions": transitions}, err)
}

func (handler *Handler) createSplitRule(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	var body SplitRuleInput
	if !decodeBody(writer, request, &body) {
		return
	}
	rule, err := handler.store.CreateSplitRule(request.Context(), body, subject)
	respond(writer, rule, err)
}

func (handler *Handler) activateSplitRule(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	rule, err := handler.store.ActivateSplitRule(request.Context(), request.PathValue("id"), subject)
	respond(writer, rule, err)
}

type computeSplitBody struct {
	RevenueLine string `json:"revenueLine"`
	AmountMinor int64  `json:"amountMinor"`
	Currency    string `json:"currency"`
	AsOf        string `json:"asOf"` // YYYY-MM-DD
}

func (handler *Handler) computeSplit(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	var body computeSplitBody
	if !decodeBody(writer, request, &body) {
		return
	}
	asOf, err := time.Parse("2006-01-02", body.AsOf)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "asOf must be YYYY-MM-DD")
		return
	}
	computation, err := handler.store.ComputeSplitAt(request.Context(), body.RevenueLine, body.AmountMinor, body.Currency, asOf.UTC())
	respond(writer, computation, err)
}

type issueAdviceBody struct {
	SettlementID string `json:"settlementId"`
	RevenueLine  string `json:"revenueLine"`
	AsOf         string `json:"asOf"`
}

func (handler *Handler) issueAdvice(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	var body issueAdviceBody
	if !decodeBody(writer, request, &body) {
		return
	}
	asOf, err := time.Parse("2006-01-02", body.AsOf)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "asOf must be YYYY-MM-DD")
		return
	}
	advice, _, err := handler.store.IssueRemittanceAdvice(request.Context(), body.SettlementID,
		body.RevenueLine, asOf.UTC(), idempotencyKey(request), subject, correlationID(request))
	respond(writer, advice, err)
}

func (handler *Handler) recordSettlement(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	var body SettlementInput
	if !decodeBody(writer, request, &body) {
		return
	}
	settlement, err := handler.store.RecordSettlement(request.Context(), body, idempotencyKey(request), subject)
	respond(writer, settlement, err)
}

func (handler *Handler) ingestStatement(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	if handler.verifier == nil {
		// No trusted keys configured: statement ingest is disabled (fail-closed).
		writeError(writer, http.StatusServiceUnavailable, "statement ingest is not configured")
		return
	}
	var bundle json.RawMessage
	if !decodeBody(writer, request, &bundle) {
		return
	}
	result, err := handler.store.IngestStatement(request.Context(), handler.verifier, bundle, subject)
	respond(writer, result, err)
}

type runReconBody struct {
	AsOf string `json:"asOf"`
}

func (handler *Handler) runRecon(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	var body runReconBody
	if !decodeBody(writer, request, &body) {
		return
	}
	asOf, err := time.Parse("2006-01-02", body.AsOf)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "asOf must be YYYY-MM-DD")
		return
	}
	summary, err := handler.store.RunRecon(request.Context(), "MANUAL", subject, asOf.UTC())
	respond(writer, summary, err)
}

func (handler *Handler) listExceptions(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	openOnly := request.URL.Query().Get("state") != "all"
	exceptions, err := handler.store.ListExceptions(request.Context(), openOnly)
	respond(writer, map[string]any{"exceptions": exceptions}, err)
}

type resolveBody struct {
	Note string `json:"note"`
}

func (handler *Handler) resolveException(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	var body resolveBody
	if !decodeBody(writer, request, &body) {
		return
	}
	exception, err := handler.store.ResolveException(request.Context(), request.PathValue("id"), subject, body.Note)
	respond(writer, exception, err)
}

func (handler *Handler) reportAgency(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	from, to, ok := reportWindow(writer, request)
	if !ok {
		return
	}
	report, err := handler.store.RevenueByAgency(request.Context(), from, to)
	respond(writer, map[string]any{"agencies": report}, err)
}

func (handler *Handler) reportLines(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	from, to, ok := reportWindow(writer, request)
	if !ok {
		return
	}
	report, err := handler.store.RevenueByLine(request.Context(), from, to)
	respond(writer, map[string]any{"lines": report}, err)
}

func (handler *Handler) reportAging(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	asOfText := request.URL.Query().Get("asOf")
	asOf := handler.now()
	if asOfText != "" {
		parsed, err := time.Parse("2006-01-02", asOfText)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "asOf must be YYYY-MM-DD")
			return
		}
		asOf = parsed
	}
	report, err := handler.store.AgingUnsettled(request.Context(), asOf.UTC())
	respond(writer, map[string]any{"buckets": report}, err)
}

func (handler *Handler) reportExceptions(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	report, err := handler.store.ExceptionCounts(request.Context())
	respond(writer, map[string]any{"exceptions": report}, err)
}

func reportWindow(writer http.ResponseWriter, request *http.Request) (time.Time, time.Time, bool) {
	from, errFrom := time.Parse("2006-01-02", request.URL.Query().Get("from"))
	to, errTo := time.Parse("2006-01-02", request.URL.Query().Get("to"))
	if errFrom != nil || errTo != nil || to.Before(from) {
		writeError(writer, http.StatusBadRequest, "from/to must be YYYY-MM-DD with to >= from")
		return time.Time{}, time.Time{}, false
	}
	return from.UTC(), to.UTC(), true
}

func respond(writer http.ResponseWriter, payload any, err error) {
	if err == nil {
		writeJSON(writer, http.StatusOK, payload)
		return
	}
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(writer, http.StatusNotFound, "not found")
	case errors.Is(err, ErrIdempotencyConflict):
		writeError(writer, http.StatusConflict, "idempotency key conflict")
	case errors.Is(err, ErrMakerChecker):
		writeError(writer, http.StatusForbidden, "maker and checker must be distinct")
	case errors.Is(err, ErrInvalidTransition):
		writeError(writer, http.StatusConflict, "state transition is not permitted")
	case errors.Is(err, ErrSplitIncomplete):
		writeError(writer, http.StatusConflict, "active split rules do not sum to 10000 bps")
	case errors.Is(err, envelope.ErrSignature), errors.Is(err, envelope.ErrUntrustedKey),
		errors.Is(err, envelope.ErrMalformed), errors.Is(err, envelope.ErrNonCanonical):
		writeError(writer, http.StatusForbidden, "envelope verification failed")
	default:
		writeError(writer, http.StatusBadRequest, err.Error())
	}
}

func decodeBody(writer http.ResponseWriter, request *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(writer, http.StatusBadRequest, "request body is invalid: "+err.Error())
		return false
	}
	return true
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}
