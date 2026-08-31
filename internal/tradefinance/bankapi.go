package tradefinance

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
)

// BankRegistryEntry binds one registered bank to its allowed client
// identities. Banks authenticate either by OIDC bearer subject (Keycloak
// client per bank) or by mTLS client-certificate CN; both lists are
// env-configured and the registry is fail-closed when empty.
type BankRegistryEntry struct {
	BankID          string   `json:"bank_id"`
	OIDCSubjects    []string `json:"oidc_subjects"`
	MTLSCommonNames []string `json:"mtls_common_names"`
}

// EnvBankRegistry carries the JSON-encoded bank registry.
const EnvBankRegistry = "TRADEFINANCE_BANK_REGISTRY_JSON"

// BankRegistry resolves authenticated client identities to bank ids.
type BankRegistry struct {
	bySubject map[string]string
	byCN      map[string]string
}

var (
	// ErrBankRegistryRequired fails closed when no bank registry is configured.
	ErrBankRegistryRequired = errors.New("tradefinance bank registry is required")
	// ErrBankUnauthenticated rejects requests without a verified bank identity (401).
	ErrBankUnauthenticated = errors.New("bank client identity is absent or unverifiable")
	// ErrBankForbidden rejects verified identities without bank binding or scope (403).
	ErrBankForbidden = errors.New("bank client is not registered for this dataset")
)

// NewBankRegistry builds the registry from env JSON, failing closed on an
// empty or malformed registry.
func NewBankRegistry(registryJSON string) (*BankRegistry, error) {
	trimmed := strings.TrimSpace(registryJSON)
	if trimmed == "" {
		return nil, ErrBankRegistryRequired
	}
	var entries []BankRegistryEntry
	if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
		return nil, fmt.Errorf("%w: registry JSON malformed: %v", ErrBankRegistryRequired, err)
	}
	if len(entries) == 0 {
		return nil, ErrBankRegistryRequired
	}
	registry := &BankRegistry{bySubject: map[string]string{}, byCN: map[string]string{}}
	for _, entry := range entries {
		if err := ValidateIdentifier("bank_id", entry.BankID); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBankRegistryRequired, err)
		}
		if len(entry.OIDCSubjects)+len(entry.MTLSCommonNames) == 0 {
			return nil, fmt.Errorf("%w: bank %s has no client identity", ErrBankRegistryRequired, entry.BankID)
		}
		for _, subject := range entry.OIDCSubjects {
			if strings.TrimSpace(subject) == "" {
				return nil, fmt.Errorf("%w: bank %s carries an empty OIDC subject", ErrBankRegistryRequired, entry.BankID)
			}
			registry.bySubject[subject] = entry.BankID
		}
		for _, cn := range entry.MTLSCommonNames {
			if strings.TrimSpace(cn) == "" {
				return nil, fmt.Errorf("%w: bank %s carries an empty mTLS CN", ErrBankRegistryRequired, entry.BankID)
			}
			registry.byCN[cn] = entry.BankID
		}
	}
	return registry, nil
}

// BearerAuthenticator verifies an Authorization bearer token and returns its
// subject. Keycloak OIDC is the production implementation; the interface
// keeps the handler testable without weakening production auth.
type BearerAuthenticator interface {
	AuthenticateSubject(ctx context.Context, authorizationHeader string) (string, error)
}

// resolveBank maps a request to a registered bank id. mTLS takes precedence
// when a verified peer certificate chain is present; otherwise the bearer
// subject is used. Absence of both fails closed.
func (registry *BankRegistry) resolveBank(request *http.Request, authenticator BearerAuthenticator) (string, error) {
	if request.TLS != nil && len(request.TLS.VerifiedChains) > 0 {
		cn := request.TLS.VerifiedChains[0][0].Subject.CommonName
		if bankID, ok := registry.byCN[cn]; ok {
			return bankID, nil
		}
		return "", ErrBankForbidden
	}
	if authenticator == nil {
		return "", ErrBankUnauthenticated
	}
	subject, err := authenticator.AuthenticateSubject(request.Context(), request.Header.Get("Authorization"))
	if err != nil || subject == "" {
		return "", ErrBankUnauthenticated
	}
	if bankID, ok := registry.bySubject[subject]; ok {
		return bankID, nil
	}
	return "", ErrBankForbidden
}

// BankAPI serves the bank-facing data-sharing surface. Every successful
// response is an envelope v1.0 bundle (FHIR R4 + JWS EdDSA/JCS) carrying
// only the consented scope's digest references; every failure is an unsigned
// RFC 7807 problem with no dataset leakage.
type BankAPI struct {
	store         *Store
	registry      *BankRegistry
	authenticator BearerAuthenticator
	signer        *envelope.Signer
	now           func() time.Time
}

// NewBankAPI fails closed on any missing dependency.
func NewBankAPI(store *Store, registry *BankRegistry, authenticator BearerAuthenticator, signer *envelope.Signer) (*BankAPI, error) {
	if store == nil || registry == nil || signer == nil {
		return nil, errors.New("bank API store, registry and signer are required")
	}
	return &BankAPI{store: store, registry: registry, authenticator: authenticator, signer: signer, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Mux returns the bank API routes.
func (api *BankAPI) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/tradefinance/bank/datasets/{traderID}/{scope}", api.handleDataset)
	return mux
}

func problem(writer http.ResponseWriter, status int, detail string) {
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"type":   "about:blank",
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
	})
}

// handleDataset serves one consented dataset class for one trader. The
// response is signed; only the requested, consented scope is ever included.
func (api *BankAPI) handleDataset(writer http.ResponseWriter, request *http.Request) {
	bankID, err := api.registry.resolveBank(request, api.authenticator)
	if errors.Is(err, ErrBankUnauthenticated) {
		problem(writer, http.StatusUnauthorized, "bank client authentication required")
		return
	}
	if err != nil {
		problem(writer, http.StatusForbidden, "bank client is not registered")
		return
	}
	traderID := request.PathValue("traderID")
	scope := Scope(request.PathValue("scope"))
	consent, refs, err := api.store.ConsentedDataset(request.Context(), bankID, traderID, scope, api.now())
	if err != nil {
		// Fail-closed: unconsented, expired and unknown all map to 403 with
		// no oracle on which consents exist.
		problem(writer, http.StatusForbidden, "no active consent covers this dataset")
		return
	}
	payload := map[string]any{
		"bank_id":    consent.BankID,
		"trader_id":  consent.TraderID,
		"scope":      string(scope),
		"consent_id": consent.ConsentID,
		"expires_at": consent.ExpiresAt.UTC().Format(time.RFC3339),
		"dataset_refs": func() []any {
			list := make([]any, 0, len(refs))
			for _, ref := range refs {
				list = append(list, ref)
			}
			return list
		}(),
	}
	bundle, _, err := api.signer.Sign("tradefinance.dataset", consent.ConsentID+"-"+string(scope), payload, api.now())
	if err != nil {
		problem(writer, http.StatusInternalServerError, "dataset envelope sealing failed")
		return
	}
	writer.Header().Set("Content-Type", "application/fhir+json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(bundle)
}

// RequireMTLS configures a TLS server to demand bank client certificates
// from the given CA pool. When the pool is nil the server must not be
// started for the bank surface (fail-closed).
func RequireMTLS(pool *x509.CertPool) (*tls.Config, error) {
	if pool == nil {
		return nil, errors.New("bank API mTLS client CA pool is required")
	}
	return &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  pool,
		MinVersion: tls.VersionTLS13,
	}, nil
}
