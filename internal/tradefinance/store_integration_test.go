//go:build integration

package tradefinance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
)

func http_get(url string) (*http.Response, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	return client.Get(url)
}

func readAll(t *testing.T, response *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// openTestStore applies migrations 0001-0005 (shared platform tables the
// outbox publisher drains) and 0011 (trade finance) against real PostgreSQL.
// Requires DATABASE_URL and MIGRATION_PATH.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	signer, err := newTestSigner()
	if err != nil {
		t.Fatal(err)
	}
	// Isolate each test in its own database (migrations are not idempotent).
	databaseURL := os.Getenv("DATABASE_URL")
	bootstrap, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	dbName := "tf_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := bootstrap.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		bootstrap.Close(ctx)
		t.Fatal(err)
	}
	bootstrap.Close(ctx)
	t.Cleanup(func() {
		cleanup, err := pgx.Connect(ctx, databaseURL)
		if err != nil {
			return
		}
		defer cleanup.Close(ctx)
		_, _ = cleanup.Exec(ctx, `DROP DATABASE `+dbName+` WITH (FORCE)`)
	})
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + dbName
	testURL := parsed.String()
	store, err := Open(ctx, testURL, signer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	for _, name := range []string{"0001", "0002", "0003", "0004", "0005", "0011"} {
		matches, globErr := filepath.Glob(filepath.Join(os.Getenv("MIGRATION_PATH"), name+"_*.sql"))
		if globErr != nil || len(matches) != 1 {
			t.Fatalf("locate migration %s: %v", name, globErr)
		}
		migration, err := os.ReadFile(filepath.Clean(matches[0]))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
	return store
}

func requestTestConsent(t *testing.T, store *Store, consentID, traderID, bankID string) Consent {
	t.Helper()
	now := time.Now().UTC()
	retained, err := store.RequestConsent(context.Background(), Consent{
		ConsentID: consentID, TraderID: traderID, BankID: bankID,
		Scopes:      []Scope{ScopeDeclarationDigests, ScopeDutyPaymentHistory},
		DatasetRefs: validRefs(), ExpiresAt: now.Add(90 * 24 * time.Hour),
		MakerPrincipal: traderID,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return retained
}

func activateTestConsent(t *testing.T, store *Store, consent Consent) Consent {
	t.Helper()
	retained, err := store.ActivateConsent(context.Background(), consent.ConsentID, consent.Version, "checker-001")
	if err != nil {
		t.Fatal(err)
	}
	return retained
}

// TestRealPostgresConsentLifecycle verifies maker-checker consent granting,
// envelope sealing, hash-chained audit and DB immutability against real PG.
func TestRealPostgresConsentLifecycle(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	retained := requestTestConsent(t, store, "tf-con-100", "trader-100", "bank-gtb")
	if retained.State != ConsentPendingChecker || retained.Version != 1 {
		t.Fatalf("requested consent: %+v", retained)
	}
	// Maker cannot activate its own grant.
	if _, err := store.ActivateConsent(ctx, retained.ConsentID, retained.Version, "trader-100"); !errors.Is(err, ErrMakerChecker) {
		t.Fatalf("self-activation error = %v", err)
	}
	// Duplicate open consent for the same trader/bank is DB-rejected.
	if _, err := store.RequestConsent(ctx, Consent{
		ConsentID: "tf-con-dup", TraderID: "trader-100", BankID: "bank-gtb",
		Scopes: []Scope{ScopeDeclarationDigests}, DatasetRefs: map[string][]string{string(ScopeDeclarationDigests): {digest("7")}},
		ExpiresAt: time.Now().UTC().Add(24 * time.Hour), MakerPrincipal: "trader-100",
	}, time.Now().UTC()); err == nil {
		t.Fatal("duplicate open consent accepted")
	}

	activated := activateTestConsent(t, store, retained)
	if activated.State != ConsentActive || activated.EnvelopeJWS == "" || len(activated.EnvelopeBundle) == 0 {
		t.Fatalf("activated consent: %+v", activated)
	}
	// The sealed consent envelope verifies end to end.
	signer, _ := newTestSigner()
	verifier, err := envelope.NewVerifier(map[string]string{signer.KeyID(): signer.PublicKeyBase64()})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifier.Verify(activated.EnvelopeBundle)
	if err != nil {
		t.Fatal(err)
	}
	if verified.ArtifactKind != "tradefinance.consent" || verified.ArtifactID != activated.ConsentID {
		t.Fatalf("verified consent artifact = %+v", verified)
	}
	// A verifier without the signer key fails closed.
	untrusted, err := envelope.NewVerifier(map[string]string{"other-key": signer.PublicKeyBase64()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.Verify(activated.EnvelopeBundle); !errors.Is(err, envelope.ErrUntrustedKey) {
		t.Fatalf("untrusted key error = %v", err)
	}

	// Hash-chained audit: REQUESTED + ACTIVATED, chain verifies.
	entries, err := store.ConsentAudit(ctx, activated.ConsentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("audit entries = %d, want 2", len(entries))
	}
	if err := VerifyAuditChain(entries); err != nil {
		t.Fatal(err)
	}
	// Audit is immutable in the database.
	if err := store.Exec(ctx, `UPDATE tf_consent_audit SET actor_principal = 'attacker' WHERE consent_id = 'tf-con-100'`); err == nil {
		t.Fatal("consent audit mutation accepted by database")
	}

	// Revocation: maker requests (sharing suspends), checker confirms.
	pending, err := store.RequestRevocation(ctx, activated.ConsentID, activated.Version, "trader-100")
	if err != nil || pending.State != ConsentRevocationPending {
		t.Fatalf("request revocation: %+v %v", pending, err)
	}
	if _, _, err := store.ConsentedDataset(ctx, "bank-gtb", "trader-100", ScopeDeclarationDigests, time.Now().UTC()); err == nil {
		t.Fatal("revocation-pending consent still shares data")
	}
	revoked, err := store.ConfirmRevocation(ctx, pending.ConsentID, pending.Version, "checker-001")
	if err != nil || revoked.State != ConsentRevoked {
		t.Fatalf("confirm revocation: %+v %v", revoked, err)
	}
	entries, err = store.ConsentAudit(ctx, revoked.ConsentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("audit entries = %d, want 4", len(entries))
	}
	if err := VerifyAuditChain(entries); err != nil {
		t.Fatal(err)
	}

	// Outbox carries the consent events.
	var count int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM tf_outbox WHERE subject_id = 'tf-con-100'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("consent outbox events = %d, want 4", count)
	}
}

// TestRealPostgresConsentScopeEnforcement is the NTP TFC negative-test core:
// a bank sees ONLY the datasets an active consent covers — wrong bank,
// unconsented scope, expired consent, pending consent and foreign trader all
// fail closed.
func TestRealPostgresConsentScopeEnforcement(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now().UTC()

	activated := activateTestConsent(t, store, requestTestConsent(t, store, "tf-con-200", "trader-200", "bank-gtb"))

	// Positive: consented scope returns ONLY that scope's digest refs.
	consent, refs, err := store.ConsentedDataset(ctx, "bank-gtb", "trader-200", ScopeDeclarationDigests, now)
	if err != nil {
		t.Fatal(err)
	}
	if consent.ConsentID != activated.ConsentID {
		t.Fatalf("dataset consent = %+v", consent)
	}
	if len(refs) != 2 {
		t.Fatalf("declaration digest refs = %v", refs)
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "aaaa") {
			t.Fatalf("ref %q is not digest evidence", ref)
		}
	}

	// Negative: unconsented scope on the same consent.
	if _, _, err := store.ConsentedDataset(ctx, "bank-gtb", "trader-200", ScopeTaxStampStatus, now); !errors.Is(err, ErrScopeNotConsented) {
		t.Fatalf("unconsented scope error = %v", err)
	}
	// Negative: another bank holds no consent.
	if _, _, err := store.ConsentedDataset(ctx, "bank-zenith", "trader-200", ScopeDeclarationDigests, now); !errors.Is(err, ErrScopeNotConsented) {
		t.Fatalf("foreign bank error = %v", err)
	}
	// Negative: another trader's data is invisible.
	if _, _, err := store.ConsentedDataset(ctx, "bank-gtb", "trader-999", ScopeDeclarationDigests, now); !errors.Is(err, ErrScopeNotConsented) {
		t.Fatalf("foreign trader error = %v", err)
	}
	// Negative: expired consent fails closed.
	if _, _, err := store.ConsentedDataset(ctx, "bank-gtb", "trader-200", ScopeDeclarationDigests, activated.ExpiresAt.Add(time.Second)); !errors.Is(err, ErrConsentExpired) {
		t.Fatalf("expired consent error = %v", err)
	}
	// Negative: PENDING_CHECKER consent shares nothing.
	pendingConsent := requestTestConsent(t, store, "tf-con-201", "trader-201", "bank-gtb")
	if _, _, err := store.ConsentedDataset(ctx, "bank-gtb", "trader-201", ScopeDeclarationDigests, now); !errors.Is(err, ErrScopeNotConsented) {
		t.Fatalf("pending consent error = %v", err)
	}
	_ = pendingConsent
}

// TestRealPostgresBankAPI exercises the bank-facing signed HTTP surface end
// to end against real PG: signed consented responses, 401/403 fail-closed
// paths and envelope verification failure on tampering.
func TestRealPostgresBankAPI(t *testing.T) {
	ctx := context.Background()
	_ = ctx
	store := openTestStore(t)
	activateTestConsent(t, store, requestTestConsent(t, store, "tf-con-300", "trader-300", "bank-gtb"))

	signer, _ := newTestSigner()
	registry, err := NewBankRegistry(`[{"bank_id":"bank-gtb","oidc_subjects":["kc-bank-gtb-client"]}]`)
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewBankAPI(store, registry, stubAuthenticator{subject: "kc-bank-gtb-client"}, signer)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Mux())
	defer server.Close()

	// Positive: consented scope is a verifiable envelope carrying ONLY that scope.
	response, err := http_get(server.URL + "/v1/tradefinance/bank/datasets/trader-300/DECLARATION_DIGESTS")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("status = %d", response.StatusCode)
	}
	bundle := readAll(t, response)
	verifier := newTestVerifier(t, signer)
	verified, err := verifier.Verify(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(verified.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["scope"] != string(ScopeDeclarationDigests) {
		t.Fatalf("payload scope = %v", payload["scope"])
	}
	if _, leaked := payload[string(ScopeDutyPaymentHistory)]; leaked {
		t.Fatal("response leaked an unrequested scope")
	}
	if _, leaked := payload["dataset_refs_full"]; leaked {
		t.Fatal("response leaked the full dataset ref map")
	}

	// Negative: tampered envelope fails verification.
	tampered := append([]byte(nil), bundle...)
	tampered[20] ^= 0xFF
	if _, err := verifier.Verify(tampered); err == nil {
		t.Fatal("tampered bank response verified")
	}

	// Negative: unconsented scope is 403.
	forbidden, err := http_get(server.URL + "/v1/tradefinance/bank/datasets/trader-300/TAX_STAMP_STATUS")
	if err != nil {
		t.Fatal(err)
	}
	forbidden.Body.Close()
	if forbidden.StatusCode != 403 {
		t.Fatalf("unconsented scope status = %d", forbidden.StatusCode)
	}

	// Negative: unknown trader is 403 (no consent oracle).
	unknown, err := http_get(server.URL + "/v1/tradefinance/bank/datasets/trader-zzz/DECLARATION_DIGESTS")
	if err != nil {
		t.Fatal(err)
	}
	unknown.Body.Close()
	if unknown.StatusCode != 403 {
		t.Fatalf("unknown trader status = %d", unknown.StatusCode)
	}

	// Negative: unauthenticated bank is 401.
	unauthAPI, err := NewBankAPI(store, registry, stubAuthenticator{err: errors.New("bad token")}, signer)
	if err != nil {
		t.Fatal(err)
	}
	unauthServer := httptest.NewServer(unauthAPI.Mux())
	defer unauthServer.Close()
	unauth, err := http_get(unauthServer.URL + "/v1/tradefinance/bank/datasets/trader-300/DECLARATION_DIGESTS")
	if err != nil {
		t.Fatal(err)
	}
	unauth.Body.Close()
	if unauth.StatusCode != 401 {
		t.Fatalf("unauthenticated status = %d", unauth.StatusCode)
	}
}

// TestRealPostgresApplicationWorkflow verifies the product lifecycle with
// four-party decisions, consent gating at KYC, approval immutability and the
// disbursement ledger legs.
func TestRealPostgresApplicationWorkflow(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	consent := requestTestConsent(t, store, "tf-con-400", "trader-400", "bank-gtb")
	assignments := validAssignments()
	assignments[RoleTrader] = "trader-400"
	application := validApplication()
	application.ApplicationID = "tf-app-400"
	application.ExternalRef = "tf-ref-400"
	application.TraderID = "trader-400"
	application.ConsentID = consent.ConsentID

	retained, err := store.SubmitApplication(ctx, application, assignments)
	if err != nil {
		t.Fatal(err)
	}
	if retained.State != StateApplication {
		t.Fatalf("submitted application: %+v", retained)
	}
	current, err := store.Transition(ctx, retained.ApplicationID, retained.Version, "TRADER", "trader-400", BeginReview, "tradefinance.application.submitted")
	if err != nil || current.State != StateKYCConsentCheck {
		t.Fatalf("begin review: %+v %v", current, err)
	}

	// KYC approval fails closed while the consent is still PENDING_CHECKER.
	if _, _, err := store.RecordDecision(ctx, current.ApplicationID, current.Version, "kyc-officer-1", DecisionApprove); !errors.Is(err, ErrConsentRequired) {
		t.Fatalf("KYC without active consent error = %v", err)
	}
	activated := activateTestConsent(t, store, consent)
	if !activated.Shareable(time.Now().UTC()) {
		t.Fatal("consent not shareable after activation")
	}

	current, _, err = store.RecordDecision(ctx, current.ApplicationID, current.Version, "kyc-officer-1", DecisionApprove)
	if err != nil || current.State != StateBankReview {
		t.Fatalf("KYC decision: %+v %v", current, err)
	}
	// Credit approval requires bound ledger accounts.
	if _, _, err := store.RecordDecision(ctx, current.ApplicationID, current.Version, "credit-officer-1", DecisionApprove); !errors.Is(err, ErrLedgerAccountsMissing) {
		t.Fatalf("credit without ledger accounts error = %v", err)
	}
	bound, err := store.BindLedgerAccounts(ctx, current.ApplicationID, current.Version, "tb-facility-gtb-400", "tb-settlement-trader-400")
	if err != nil {
		t.Fatal(err)
	}
	// Identical rebind is idempotent; divergent rebind conflicts.
	if _, err := store.BindLedgerAccounts(ctx, bound.ApplicationID, bound.Version, "tb-facility-gtb-400", "tb-settlement-trader-400"); err != nil {
		t.Fatalf("idempotent rebind: %v", err)
	}
	if _, err := store.BindLedgerAccounts(ctx, bound.ApplicationID, bound.Version, "tb-other", "tb-settlement-trader-400"); !errors.Is(err, ErrConflict) {
		t.Fatalf("divergent rebind error = %v", err)
	}

	chain := []struct {
		principal string
		to        State
	}{
		{"credit-officer-1", StateRegulatoryClearance},
		{"customs-officer-1", StateApproved},
		{"trader-400", StateDisbursementPending},
		{"treasury-officer-1", StateDisbursed},
		{"trader-400", StateSettled},
	}
	current = bound
	for _, step := range chain {
		if _, _, err := store.RecordDecision(ctx, current.ApplicationID, current.Version, "intruder", DecisionApprove); !errors.Is(err, ErrRoleNotAssigned) {
			t.Fatalf("intruder at %s error = %v", current.State, err)
		}
		current, _, err = store.RecordDecision(ctx, current.ApplicationID, current.Version, step.principal, DecisionApprove)
		if err != nil {
			t.Fatalf("%s decision: %v", step.principal, err)
		}
		if current.State != step.to {
			t.Fatalf("state = %s, want %s", current.State, step.to)
		}
	}
	approvals, err := store.ListApprovals(ctx, current.ApplicationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(approvals) != 6 {
		t.Fatalf("approvals = %d, want 6", len(approvals))
	}
	// Approvals are immutable in the database.
	if err := store.Exec(ctx, `UPDATE tf_approvals SET decision = 'REJECT' WHERE application_id = 'tf-app-400'`); err == nil {
		t.Fatal("approval mutation accepted by database")
	}
	// Role separation is enforced by the database.
	if err := store.Exec(ctx, `INSERT INTO tf_role_assignments (application_id, role, principal_id, created_at) VALUES ('tf-app-400', 'BANK_KYC_OFFICER', 'credit-officer-1', now())`); err == nil {
		t.Fatal("database accepted a principal holding two roles")
	}

	// Disbursement ledger legs.
	if err := store.RecordDisbursementLegs(ctx, current.ApplicationID, "tf-reserve-400", current.Amount, current.Currency); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteDisbursementLeg(ctx, current.ApplicationID, "tf-disburse-400"); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteSettlementLeg(ctx, current.ApplicationID, "tf-settle-400"); err != nil {
		t.Fatal(err)
	}
	// Forward-only stamping: a second settlement stamp conflicts.
	if err := store.CompleteSettlementLeg(ctx, current.ApplicationID, "tf-settle-401"); !errors.Is(err, ErrConflict) {
		t.Fatalf("restamp error = %v", err)
	}

	// Outbox: submitted + 6 decisions (with disbursed/settled event types).
	var count int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM tf_outbox WHERE subject_id = 'tf-app-400'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 8 {
		t.Fatalf("application outbox events = %d, want 8", count)
	}
	var eventTypes []string
	rows, err := store.Pool().Query(ctx, `SELECT DISTINCT event_type FROM tf_outbox WHERE subject_id = 'tf-app-400'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventType string
		if err := rows.Scan(&eventType); err != nil {
			t.Fatal(err)
		}
		eventTypes = append(eventTypes, eventType)
	}
	for _, required := range []string{"tradefinance.application.submitted", "tradefinance.application.decision_recorded", "tradefinance.application.disbursed", "tradefinance.application.settled"} {
		found := false
		for _, eventType := range eventTypes {
			if eventType == required {
				found = true
			}
		}
		if !found {
			t.Fatalf("outbox missing %s (has %v)", required, eventTypes)
		}
	}
}
