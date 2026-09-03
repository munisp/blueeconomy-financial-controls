package mojaloop

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvffapi"
	"github.com/munisp/blueeconomy-financial-controls/internal/pbac"
)

type stubAuthenticator struct {
	principal cvffapi.Principal
	err       error
}

func (stub stubAuthenticator) Authenticate(_ context.Context, _ string) (cvffapi.Principal, error) {
	return stub.principal, stub.err
}

func payoutTestPolicy(t *testing.T, allow bool) *pbac.Enforcer {
	t.Helper()
	verdict := "false"
	if allow {
		verdict = "true"
	}
	enforcer, err := pbac.NewEnforcer(map[string]string{"payout.rego": "package cvff\nimport rego.v1\nallow if " + verdict + "\n"})
	if err != nil {
		t.Fatalf("compile test policy: %v", err)
	}
	return enforcer
}

func payoutTestHandler(auth cvffapi.Authenticator, policy *pbac.Enforcer) PayoutHandler {
	return PayoutHandler{Authenticator: auth, Policy: policy}
}

func payoutRequest() *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/payouts", strings.NewReader(`{"payerPartyIdType":"MSISDN","payerPartyIdentifier":"payer-1","payeePartyIdType":"MSISDN","payeePartyIdentifier":"payee-1","amount":"100","currency":"NGN"}`))
	request.Header.Set("Authorization", "Bearer test-token")
	return request
}

func TestPayoutHandlerRejectsUnauthenticated(t *testing.T) {
	handler := payoutTestHandler(stubAuthenticator{err: errors.New("bad token")}, payoutTestPolicy(t, true))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, payoutRequest())
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestPayoutHandlerDenyByDefaultPolicy(t *testing.T) {
	handler := payoutTestHandler(stubAuthenticator{principal: cvffapi.Principal{Subject: "officer-1"}}, payoutTestPolicy(t, false))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, payoutRequest())
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
}

func TestPayoutHandlerFailsClosedWithoutRail(t *testing.T) {
	// Authenticated and authorized, but the durable store/client are not
	// wired: the endpoint fails closed rather than proceeding.
	handler := payoutTestHandler(stubAuthenticator{principal: cvffapi.Principal{Subject: "officer-1"}}, payoutTestPolicy(t, true))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, payoutRequest())
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}

func TestPayoutHandlerRejectsWrongMethod(t *testing.T) {
	response := httptest.NewRecorder()
	PayoutHandler{}.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/payouts", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}
