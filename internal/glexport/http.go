package glexport

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/tariff"
)

// Handler exposes the GL-export layer over HTTP. Every endpoint except
// /healthz requires a verified bearer token (the tariff Authenticator
// contract): the verified subject is the exporter/maker/checker identity, so
// dual control cannot be faked and every export is attributable.
type Handler struct {
	authenticator tariff.Authenticator
	service       *Service
	store         *Store
	logger        *slog.Logger
	mux           *http.ServeMux
}

// NewHandler fails closed on any missing dependency.
func NewHandler(authenticator tariff.Authenticator, service *Service, store *Store, logger *slog.Logger) (*Handler, error) {
	if authenticator == nil {
		return nil, errors.New("authenticator is required")
	}
	if service == nil {
		return nil, errors.New("service is required")
	}
	if store == nil {
		return nil, errors.New("store is required")
	}
	if logger == nil {
		return nil, errors.New("logger is required")
	}
	handler := &Handler{
		authenticator: authenticator,
		service:       service,
		store:         store,
		logger:        logger,
		mux:           http.NewServeMux(),
	}
	handler.mux.HandleFunc("/healthz", handler.healthz)
	handler.mux.HandleFunc("POST /v1/gl/statements", handler.exportStatement)
	handler.mux.HandleFunc("POST /v1/gl/payments", handler.exportPayments)
	handler.mux.HandleFunc("GET /v1/gl/exports", handler.listExports)
	handler.mux.HandleFunc("GET /v1/gl/exports/{id}", handler.getExportPayload)
	handler.mux.HandleFunc("POST /v1/gl/periods/close", handler.requestPeriodClose)
	handler.mux.HandleFunc("POST /v1/gl/periods/{id}/approve", handler.approvePeriodClose)
	handler.mux.HandleFunc("GET /v1/gl/periods", handler.listPeriodCloses)
	handler.mux.HandleFunc("GET /v1/gl/periods/{id}", handler.getPeriodClose)
	handler.mux.HandleFunc("GET /v1/gl/trial-balance", handler.trialBalance)
	handler.mux.HandleFunc("POST /v1/gl/reconciliation/runs", handler.runReconciliation)
	handler.mux.HandleFunc("GET /v1/gl/reconciliation/runs", handler.listReconciliationRuns)
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
		handler.logger.Warn("glexport request rejected", "path", request.URL.Path, "error", err.Error())
		writeError(writer, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	return subject, true
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

type periodScope struct {
	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
}

func (scope periodScope) parse() (time.Time, time.Time, error) {
	return parsePeriod(scope.PeriodStart, scope.PeriodEnd)
}

func parsePeriod(startRaw, endRaw string) (time.Time, time.Time, error) {
	start, err := time.Parse("2006-01-02", startRaw)
	if err != nil {
		return time.Time{}, time.Time{}, errors.New("period_start must be YYYY-MM-DD")
	}
	end, err := time.Parse("2006-01-02", endRaw)
	if err != nil {
		return time.Time{}, time.Time{}, errors.New("period_end must be YYYY-MM-DD")
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, errors.New("period_end precedes period_start")
	}
	return start, end, nil
}

func decodeBody(writer http.ResponseWriter, request *http.Request, target any) bool {
	if err := json.NewDecoder(request.Body).Decode(target); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

type exportStatementRequest struct {
	Account  string `json:"account"`
	Currency string `json:"currency"`
	periodScope
}

func (handler *Handler) exportStatement(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	var body exportStatementRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	start, end, err := body.parse()
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	batch, err := handler.service.ExportStatement(request.Context(), body.Account, body.Currency, start, end, subject)
	if err != nil {
		switch {
		case errors.Is(err, ErrExportReplay):
			writeError(writer, http.StatusConflict, "identical statement already exported")
		default:
			handler.logger.Warn("statement export failed", "error", err.Error())
			writeError(writer, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(writer, http.StatusCreated, batch)
}

type exportPaymentsRequest struct {
	Currency string `json:"currency"`
	periodScope
}

func (handler *Handler) exportPayments(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	var body exportPaymentsRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	start, end, err := body.parse()
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	batch, err := handler.service.ExportCreditTransfers(request.Context(), body.Currency, start, end, subject)
	if err != nil {
		switch {
		case errors.Is(err, ErrExportReplay):
			writeError(writer, http.StatusConflict, "identical payment initiation already exported")
		default:
			handler.logger.Warn("payment export failed", "error", err.Error())
			writeError(writer, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(writer, http.StatusCreated, batch)
}

func (handler *Handler) listExports(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	query := request.URL.Query()
	start, end, err := parsePeriod(query.Get("period_start"), query.Get("period_end"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	batches, err := handler.store.ExportBatches(request.Context(), query.Get("account"), start, end)
	if err != nil {
		handler.logger.Error("list exports failed", "error", err.Error())
		writeError(writer, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"exports": batches})
}

// getExportPayload streams the stored ISO 20022 XML after verifying it still
// hashes to the recorded digest (fail-closed audit download).
func (handler *Handler) getExportPayload(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	payload, payloadHash, err := handler.store.GetExportPayload(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, http.StatusNotFound, "export not found")
		return
	}
	digest := sha256Hex([]byte(payload))
	if digest != payloadHash {
		handler.logger.Error("export payload hash mismatch", "batch_id", request.PathValue("id"))
		writeError(writer, http.StatusInternalServerError, "export payload integrity check failed")
		return
	}
	writer.Header().Set("Content-Type", "application/xml")
	writer.Header().Set("X-Payload-SHA256", payloadHash)
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte(payload))
}

func (handler *Handler) requestPeriodClose(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	var body periodScope
	if !decodeBody(writer, request, &body) {
		return
	}
	start, end, err := body.parse()
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	close, err := handler.service.RequestPeriodClose(request.Context(), start, end, subject)
	if err != nil {
		if errors.Is(err, ErrPeriodExists) {
			writeError(writer, http.StatusConflict, "period close already exists")
			return
		}
		handler.logger.Warn("period close request failed", "error", err.Error())
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, close)
}

func (handler *Handler) approvePeriodClose(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	close, err := handler.service.ApprovePeriodClose(request.Context(), request.PathValue("id"), subject)
	if err != nil {
		if errors.Is(err, ErrPeriodApprove) {
			writeError(writer, http.StatusConflict, "period close cannot be approved (unknown, already closed, or maker equals checker)")
			return
		}
		handler.logger.Warn("period close approval failed", "error", err.Error())
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, close)
}

func (handler *Handler) listPeriodCloses(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	closes, err := handler.store.ListPeriodCloses(request.Context())
	if err != nil {
		handler.logger.Error("list period closes failed", "error", err.Error())
		writeError(writer, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"period_closes": closes})
}

func (handler *Handler) getPeriodClose(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	close, err := handler.store.GetPeriodClose(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, http.StatusNotFound, "period close not found")
		return
	}
	writeJSON(writer, http.StatusOK, close)
}

func (handler *Handler) trialBalance(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	query := request.URL.Query()
	start, end, err := parsePeriod(query.Get("period_start"), query.Get("period_end"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	balance, err := handler.store.TrialBalance(request.Context(), start, end)
	if err != nil {
		handler.logger.Error("trial balance failed", "error", err.Error())
		writeError(writer, http.StatusInternalServerError, "trial balance failed")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"period_start":  start.Format("2006-01-02"),
		"period_end":    end.Format("2006-01-02"),
		"trial_balance": balance,
	})
}

func (handler *Handler) runReconciliation(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	var body periodScope
	if !decodeBody(writer, request, &body) {
		return
	}
	start, end, err := body.parse()
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	run, err := handler.service.RunReconciliation(request.Context(), start, end, subject)
	if err != nil {
		handler.logger.Warn("reconciliation run failed", "error", err.Error())
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusCreated
	if !run.Balanced {
		// Drift is a finding, not a crash: 200 with balanced=false so callers
		// surface the differences rather than treating them as a transport error.
		status = http.StatusOK
	}
	writeJSON(writer, status, run)
}

func (handler *Handler) listReconciliationRuns(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	query := request.URL.Query()
	start, end, err := parsePeriod(query.Get("period_start"), query.Get("period_end"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	limit := 50
	if raw := query.Get("limit"); raw != "" {
		if parsed, convErr := strconv.Atoi(raw); convErr == nil && parsed > 0 && parsed <= 500 {
			limit = parsed
		}
	}
	runs, err := handler.store.ListReconciliationRuns(request.Context(), start, end, limit)
	if err != nil {
		handler.logger.Error("list reconciliation runs failed", "error", err.Error())
		writeError(writer, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"reconciliation_runs": runs})
}
