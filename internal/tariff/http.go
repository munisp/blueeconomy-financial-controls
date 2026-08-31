package tariff

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Handler exposes the tariff engine over HTTP. Every endpoint except
// /healthz requires a verified bearer token; the verified subject is the
// maker/checker/requester identity, so self-approval is impossible to fake.
type Handler struct {
	authenticator Authenticator
	store         *Store
	logger        *slog.Logger
	mux           *http.ServeMux
	now           func() time.Time
}

// NewHandler fails closed on any missing dependency.
func NewHandler(authenticator Authenticator, store *Store, logger *slog.Logger) (*Handler, error) {
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
		logger:        logger,
		mux:           http.NewServeMux(),
		now:           func() time.Time { return time.Now().UTC() },
	}
	handler.mux.HandleFunc("/healthz", handler.healthz)
	handler.mux.HandleFunc("POST /v1/tariffs/assess", handler.assess)
	handler.mux.HandleFunc("GET /v1/tariffs/assessments/{id}", handler.getAssessment)
	handler.mux.HandleFunc("GET /v1/tariffs/assessments/{id}/exemption-audits", handler.getExemptionAudits)
	handler.mux.HandleFunc("POST /v1/tariffs/rates", handler.createRate)
	handler.mux.HandleFunc("POST /v1/tariffs/rates/{id}/activate", handler.activateRate)
	handler.mux.HandleFunc("GET /v1/tariffs/rates", handler.listRates)
	handler.mux.HandleFunc("POST /v1/tariffs/exemptions", handler.createExemption)
	handler.mux.HandleFunc("POST /v1/tariffs/exemptions/{id}/activate", handler.activateExemption)
	return handler, nil
}

// ServeHTTP dispatches the mux.
func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handler.mux.ServeHTTP(writer, request)
}

func (handler *Handler) healthz(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

// subject authenticates the request and returns the verified token subject.
func (handler *Handler) subject(writer http.ResponseWriter, request *http.Request) (string, bool) {
	subject, err := handler.authenticator.Authenticate(request.Context(), request.Header.Get("Authorization"))
	if err != nil {
		handler.logger.Warn("tariff request rejected", "path", request.URL.Path, "error", err.Error())
		writeError(writer, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	return subject, true
}

func (handler *Handler) assess(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	var body AssessRequest
	if !decodeBody(writer, request, &body) {
		return
	}
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeError(writer, http.StatusBadRequest, "Idempotency-Key header is required")
		return
	}
	correlationID := strings.TrimSpace(request.Header.Get("X-Correlation-ID"))
	if correlationID == "" {
		correlationID = idempotencyKey
	}
	assessment, err := handler.store.Assess(request.Context(), body, idempotencyKey, subject, correlationID, handler.now())
	if err != nil {
		handler.logger.Warn("tariff assessment rejected", "error", err.Error())
		writeTariffError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, assessment)
}

func (handler *Handler) getAssessment(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	assessment, err := handler.store.GetAssessment(request.Context(), request.PathValue("id"))
	if err != nil {
		writeTariffError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, assessment)
}

func (handler *Handler) getExemptionAudits(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	audits, err := handler.store.ListExemptionAudits(request.Context(), request.PathValue("id"))
	if err != nil {
		writeTariffError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"audits": audits})
}

type rateBody struct {
	RateID             string `json:"rateId"`
	Instrument         string `json:"instrument"`
	Agency             string `json:"agency"`
	BandLogic          string `json:"bandLogic"`
	Currency           string `json:"currency"`
	RateMinorPerUnit   int64  `json:"rateMinorPerUnit"`
	RateBps            int64  `json:"rateBps"`
	BandFloor          int64  `json:"bandFloor"`
	BandCeiling        *int64 `json:"bandCeiling"`
	StatutoryReference string `json:"statutoryReference"`
	Provisional        bool   `json:"provisional"`
	EffectiveFrom      string `json:"effectiveFrom"`
	EffectiveTo        string `json:"effectiveTo"`
}

func parseDateField(value, name string) (time.Time, error) {
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, errors.New(name + " must be YYYY-MM-DD")
	}
	return parsed.UTC(), nil
}

func parseOptionalDateField(value, name string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := parseDateField(value, name)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func (handler *Handler) createRate(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	var body rateBody
	if !decodeBody(writer, request, &body) {
		return
	}
	from, err := parseDateField(body.EffectiveFrom, "effectiveFrom")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	to, err := parseOptionalDateField(body.EffectiveTo, "effectiveTo")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	currency := strings.ToUpper(body.Currency)
	if currency == "" {
		currency = "USD"
	}
	rate, err := handler.store.CreateRate(request.Context(), RateRow{
		RateID:             body.RateID,
		Instrument:         body.Instrument,
		Agency:             body.Agency,
		BandLogic:          body.BandLogic,
		Currency:           currency,
		RateMinorPerUnit:   body.RateMinorPerUnit,
		RateBps:            body.RateBps,
		BandFloor:          body.BandFloor,
		BandCeiling:        body.BandCeiling,
		StatutoryReference: body.StatutoryReference,
		Provisional:        body.Provisional,
		EffectiveFrom:      from,
		EffectiveTo:        to,
	}, subject)
	if err != nil {
		writeTariffError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, rate)
}

func (handler *Handler) activateRate(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	rate, err := handler.store.ActivateRate(request.Context(), request.PathValue("id"), subject)
	if err != nil {
		writeTariffError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, rate)
}

func (handler *Handler) listRates(writer http.ResponseWriter, request *http.Request) {
	if _, ok := handler.subject(writer, request); !ok {
		return
	}
	rates, err := handler.store.ListRates(request.Context())
	if err != nil {
		writeTariffError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"rates": rates})
}

type exemptionBody struct {
	ExemptionID         string `json:"exemptionId"`
	Instrument          string `json:"instrument"`
	MatchKind           string `json:"matchKind"`
	MatchValue          string `json:"matchValue"`
	StatutoryBasis      string `json:"statutoryBasis"`
	EvidenceRequirement string `json:"evidenceRequirement"`
	EffectiveFrom       string `json:"effectiveFrom"`
	EffectiveTo         string `json:"effectiveTo"`
}

func (handler *Handler) createExemption(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	var body exemptionBody
	if !decodeBody(writer, request, &body) {
		return
	}
	from, err := parseDateField(body.EffectiveFrom, "effectiveFrom")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	to, err := parseOptionalDateField(body.EffectiveTo, "effectiveTo")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	exemption, err := handler.store.CreateExemption(request.Context(), ExemptionRow{
		ExemptionID:         body.ExemptionID,
		Instrument:          body.Instrument,
		MatchKind:           strings.ToUpper(body.MatchKind),
		MatchValue:          body.MatchValue,
		StatutoryBasis:      body.StatutoryBasis,
		EvidenceRequirement: body.EvidenceRequirement,
		EffectiveFrom:       from,
		EffectiveTo:         to,
	}, subject)
	if err != nil {
		writeTariffError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, exemption)
}

func (handler *Handler) activateExemption(writer http.ResponseWriter, request *http.Request) {
	subject, ok := handler.subject(writer, request)
	if !ok {
		return
	}
	exemption, err := handler.store.ActivateExemption(request.Context(), request.PathValue("id"), subject)
	if err != nil {
		writeTariffError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, exemption)
}

func writeTariffError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(writer, http.StatusNotFound, "not found")
	case errors.Is(err, ErrIdempotencyConflict):
		writeError(writer, http.StatusConflict, "idempotency key conflict")
	case errors.Is(err, ErrMakerChecker):
		writeError(writer, http.StatusConflict, "maker and checker must be distinct")
	case errors.Is(err, ErrInvalidTransition):
		writeError(writer, http.StatusConflict, "state transition is not permitted")
	default:
		writeError(writer, http.StatusBadRequest, "request could not be processed")
	}
}

func decodeBody(writer http.ResponseWriter, request *http.Request, target any) bool {
	defer request.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(writer, http.StatusBadRequest, "request body is invalid")
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
