package intent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

// APIStore is the persistence boundary used by the HTTP API.
type APIStore interface {
	Create(ctx context.Context, request CreateRequest) (Intent, error)
	Approve(ctx context.Context, intentID string, expectedVersion int64, checker string) (Intent, error)
}

// Handler implements the openapi.yaml financial-intent contract.
type Handler struct {
	store   APIStore
	mux     *http.ServeMux
	handler http.Handler
}

// NewHandler fails closed when the store or the telemetry pipeline is absent.
func NewHandler(store APIStore, pipeline *telemetry.Telemetry) (*Handler, error) {
	if store == nil {
		return nil, errors.New("intent store is required")
	}
	if pipeline == nil {
		return nil, errors.New("telemetry pipeline is required (fail-closed); use telemetry.Setup with a disabled config for no-op tracing")
	}
	handler := &Handler{store: store, mux: http.NewServeMux()}
	handler.mux.HandleFunc("GET /healthz", handler.health)
	handler.mux.HandleFunc("GET /readyz", handler.readyz)
	handler.mux.Handle("GET /metrics", pipeline.MetricsHandler())
	handler.mux.HandleFunc("POST /v1/financial-intents", handler.create)
	handler.mux.HandleFunc("POST /v1/financial-intents/{intent_id}/approve", handler.approve)
	handler.handler = pipeline.Middleware(handler.mux)
	return handler, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handler.handler.ServeHTTP(writer, request)
}

func (handler *Handler) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz fails closed unless the store exposes and passes a Ping.
func (handler *Handler) readyz(writer http.ResponseWriter, request *http.Request) {
	pinger, ok := handler.store.(interface{ Ping(context.Context) error })
	if !ok {
		writeError(writer, http.StatusServiceUnavailable, errors.New("intent store does not expose readiness"))
		return
	}
	if err := pinger.Ping(request.Context()); err != nil {
		writeError(writer, http.StatusServiceUnavailable, errors.New("intent store is not reachable"))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
}

func (handler *Handler) create(writer http.ResponseWriter, request *http.Request) {
	var createRequest CreateRequest
	if err := decodeJSON(request, &createRequest); err != nil {
		writeError(writer, http.StatusUnprocessableEntity, err)
		return
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

type approvalRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	Checker         string `json:"checker"`
}

func (handler *Handler) approve(writer http.ResponseWriter, request *http.Request) {
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
	if approval.ExpectedVersion <= 0 || strings.TrimSpace(approval.Checker) == "" {
		writeError(writer, http.StatusUnprocessableEntity, errors.New("expected_version and checker are required"))
		return
	}
	// Record the CVFF maker/checker approval on the active span (no-op when
	// telemetry is disabled). The checker identity is already approval
	// evidence; the maker subject is never logged here.
	trace.SpanFromContext(request.Context()).SetAttributes(
		attribute.String("cvff.intent_id", intentID),
		attribute.String("cvff.approval.checker", approval.Checker),
	)
	updated, err := handler.store.Approve(request.Context(), intentID, approval.ExpectedVersion, approval.Checker)
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
