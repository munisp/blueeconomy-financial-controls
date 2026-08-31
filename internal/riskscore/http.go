package riskscore

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Handler serves POST /v1/risk-scores against the loaded rules. The scoring
// route requires a verified Keycloak RS256 bearer token; only the health
// endpoint is public.
type Handler struct {
	rules Rules
	auth  Authenticator
	mux   *http.ServeMux
}

// NewHandler fails closed when the rules carry no model version (i.e. were
// not loaded through LoadRules) or when no authenticator is wired: there is
// no unauthenticated scoring path.
func NewHandler(rules Rules, auth Authenticator) (*Handler, error) {
	if err := rules.Validate(); err != nil {
		return nil, err
	}
	if auth == nil {
		return nil, errors.New("riskscore: an authenticator is required")
	}
	handler := &Handler{rules: rules, auth: auth, mux: http.NewServeMux()}
	handler.mux.HandleFunc("GET /healthz", handler.health)
	handler.mux.HandleFunc("POST /v1/risk-scores", handler.score)
	return handler, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handler.mux.ServeHTTP(writer, request)
}

func (handler *Handler) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok", "model_version": handler.rules.ModelVersion, "rule_based": "true"})
}

func (handler *Handler) score(writer http.ResponseWriter, request *http.Request) {
	if err := handler.auth.Authenticate(request.Context(), request.Header.Get("Authorization")); err != nil {
		writeError(writer, http.StatusUnauthorized, errors.New("bearer token is absent or unverifiable"))
		return
	}
	var payload ScoreRequest
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeError(writer, http.StatusBadRequest, errors.New("request body is not a valid scoring payload"))
		return
	}
	if decoder.More() {
		writeError(writer, http.StatusBadRequest, errors.New("request body must contain exactly one JSON document"))
		return
	}
	if err := payload.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	writeJSON(writer, http.StatusOK, handler.rules.Score(payload))
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, map[string]string{"error": err.Error()})
}
