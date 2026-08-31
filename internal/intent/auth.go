package intent

import (
	"context"
	"errors"
	"net/http"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvffapi"
	"github.com/munisp/blueeconomy-financial-controls/internal/pbac"
)

// Approved realm roles for the financial-intent API. GitOps binds them to
// treasury identities only; the maker and checker roles are deliberately
// distinct so a single compromised credential can never complete a
// maker-checker pair.
const (
	// IntentMakerRole creates financial intents and voids own DRAFT intents.
	IntentMakerRole = "intent-maker"
	// IntentCheckerRole approves financial intents created by a different
	// verified principal.
	IntentCheckerRole = "intent-checker"
	// FinancialControllerRole resolves AMBIGUOUS intents (officer gate).
	FinancialControllerRole = "financial-controller"
)

// PBAC resource and classification for every financial-intent route.
const (
	resourceFinancialIntents = "financial.intents"
	classificationFiduciary  = "FIDUCIARY_SEGREGATED"
)

// errForbidden marks a verified identity without the role or policy grant a
// route requires. It maps to HTTP 403 without policy detail leakage.
var errForbidden = errors.New("the authenticated identity does not hold the required role or policy grant")

// principalContextKey carries the verified principal through the request
// context.
type principalContextKey struct{}

func principalFrom(ctx context.Context) cvffapi.Principal {
	principal, _ := ctx.Value(principalContextKey{}).(cvffapi.Principal)
	return principal
}

func hasRole(principal cvffapi.Principal, role string) bool {
	for _, held := range principal.Roles {
		if held == role {
			return true
		}
	}
	return false
}

// requireAccess wraps one route with the gold-standard gate: Keycloak bearer
// verification (signature, expiry, issuer, audience), the route's realm-role
// binding and the PBAC policy decision. 401 marks an unverifiable token, 403
// a verified identity without the role or the policy grant. There is no
// unauthenticated path to money movement.
func (handler *Handler) requireAccess(role string, action string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, err := handler.authenticator.Authenticate(request.Context(), request.Header.Get("Authorization"))
		if err != nil {
			writeError(writer, http.StatusUnauthorized, errors.New("a valid bearer token is required"))
			return
		}
		if !hasRole(principal, role) {
			writeError(writer, http.StatusForbidden, errForbidden)
			return
		}
		allowed := handler.policy.Allow(request.Context(), pbac.Input{
			Principal: pbac.Principal{
				Subject:   principal.Subject,
				Roles:     append([]string(nil), principal.Roles...),
				Clearance: principal.Clearance,
				TenantID:  principal.TenantID,
			},
			TenantID:       principal.TenantID,
			Resource:       resourceFinancialIntents,
			Action:         action,
			Classification: classificationFiduciary,
		})
		if !allowed {
			writeError(writer, http.StatusForbidden, errForbidden)
			return
		}
		ctx := context.WithValue(request.Context(), principalContextKey{}, principal)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}
