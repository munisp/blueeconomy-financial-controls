//go:build integration

package glexport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
)

// Integration harness: real PostgreSQL, real migrations (DATABASE_URL +
// MIGRATION_PATH, repo convention). MIGRATION_PATH points at the migrations
// directory; the harness applies 0003 (cvff disbursements), 0008 (tariff —
// 0009 debit notes reference assessments), 0009 (settlement records) and 0014
// (glexport). Each test runs against a clean public schema.
func openService(t *testing.T) (*Service, *Store, *pgxpool.Pool, *envelope.Signer, *envelope.Verifier) {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}
	migrationPath := os.Getenv("MIGRATION_PATH")
	if migrationPath == "" {
		t.Fatal("MIGRATION_PATH is required for integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(migrationPath, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations under %s: %v", migrationPath, err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read migration %s: %v", file, err)
		}
		if _, err := pool.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("apply migration %s: %v", file, err)
		}
	}

	_, seed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	signer, err := envelope.NewSigner("glexport-test-key", seed.Seed())
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	verifier, err := envelope.NewVerifier(map[string]string{"glexport-test-key": signer.PublicKeyBase64()})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	service, err := NewService(store, signer, "Federation Treasury Single Account", "NIBONGNGLAG")
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service, store, pool, signer, verifier
}

// seedJournal inserts one NGN settlement collection and one disbursed CVFF
// application so the derived journal has movement on three GL accounts:
// TSA:NGN (cash), REVENUE:NGN and CVFF:EXPENSE:NGN.
func seedJournal(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	statements := []string{
		`INSERT INTO settlement_records (settlement_id, idempotency_key, request_hash, bank_reference, amount_minor, currency, payer_ref, value_date, recorded_by)
		 VALUES ('SETL-INT-1', 'idem-int-1', 'hash-int-1', 'BNK-INT-1', 250000, 'NGN', 'PAYER-INT', '2026-01-10', 'integration-test')`,
		`INSERT INTO cvff_applications (application_id, external_ref, beneficiary_id, amount, currency, state, created_at, updated_at, version)
		 VALUES ('CVFF-INT-1', 'EXT-INT-1', 'BENEF-INT', 100000, 'NGN', 'DISBURSED', '2026-01-20T08:00:00Z', '2026-01-20T08:00:00Z', 1)`,
		`INSERT INTO cvff_role_assignments (application_id, role, principal_id, created_at)
		 VALUES ('CVFF-INT-1', 'RECEIVING_BANK', 'BANK-PRINCIPAL-INT', '2026-01-19T08:00:00Z')`,
		`INSERT INTO fx_rates (rate_id, base_currency, quote_currency, ngn_per_usd_micro, effective_date, maker, checker, state, created_at, confirmed_at)
		 VALUES ('11111111-1111-1111-1111-111111111111', 'USD', 'NGN', 1500000000, '2026-01-19', 'fx-maker', 'fx-checker', 'CONFIRMED', '2026-01-19T08:00:00Z', '2026-01-19T09:00:00Z')`,
		`INSERT INTO cvff_disbursement_legs (application_id, rate_id, ngn_per_usd_micro, fee_transfer_id, cost_transfer_id, fee_ngn_minor, cost_usd_minor, cost_ngn_equivalent, created_at, rate_effective_date)
		 VALUES ('CVFF-INT-1', '11111111-1111-1111-1111-111111111111', 1500000000, 'tb-fee-1', 'tb-cost-1', 1000, 100, 150000, '2026-01-20T08:00:00Z', '2026-01-19')`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed journal: %v\n%s", err, statement)
		}
	}
}

var (
	periodStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	periodEnd   = time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
)

func TestJournalDerivationAndTrialBalance(t *testing.T) {
	service, store, pool, _, _ := openService(t)
	seedJournal(t, pool)
	ctx := context.Background()

	entries, err := store.JournalEntries(ctx, periodStart, periodEnd)
	if err != nil {
		t.Fatalf("JournalEntries: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("journal entries = %d, want 4 double-entry legs", len(entries))
	}
	var debits, credits int64
	for _, entry := range entries {
		if entry.Direction == DirectionDebit {
			debits += entry.AmountMinor
		} else {
			credits += entry.AmountMinor
		}
	}
	if debits != credits || debits != 350000 {
		t.Fatalf("journal out of balance: debits=%d credits=%d", debits, credits)
	}

	balance, err := service.store.TrialBalance(ctx, periodStart, periodEnd)
	if err != nil {
		t.Fatalf("TrialBalance: %v", err)
	}
	if len(balance) != 3 {
		t.Fatalf("trial balance accounts = %d, want 3", len(balance))
	}
	var net int64
	for _, row := range balance {
		net += row.NetMinor
	}
	if net != 0 {
		t.Fatalf("trial balance does not net to zero: %d", net)
	}
}

func TestExportStatementSignsAndRecordsBatch(t *testing.T) {
	service, store, pool, signer, verifier := openService(t)
	seedJournal(t, pool)
	ctx := context.Background()

	batch, err := service.ExportStatement(ctx, "TSA:NGN", "NGN", periodStart, periodEnd, "officer-maker")
	if err != nil {
		t.Fatalf("ExportStatement: %v", err)
	}
	if batch.EntryCount != 2 || batch.TotalDebitMinor != 250000 || batch.TotalCreditMinor != 100000 {
		t.Fatalf("unexpected batch totals: %+v", batch)
	}
	if batch.PayloadSHA256 != sha256Hex([]byte(batch.Payload)) {
		t.Fatal("recorded payload hash does not match the payload")
	}

	// The exported XML round-trips through the ISO 20022 structure.
	var probe camt053Probe
	if err := xml.Unmarshal([]byte(batch.Payload), &probe); err != nil {
		t.Fatalf("exported payload is not camt.053-shaped: %v", err)
	}
	if len(probe.Stmt.Stmt.Ntry) != 2 {
		t.Fatalf("statement entries = %d, want 2", len(probe.Stmt.Stmt.Ntry))
	}

	// The audit envelope verifies against the signer's public key and carries
	// the payload hash.
	verified, err := verifier.Verify(batch.Envelope)
	if err != nil {
		t.Fatalf("export envelope does not verify: %v", err)
	}
	if verified.ArtifactKind != artifactKind || verified.ArtifactID != batch.BatchID {
		t.Fatalf("unexpected artifact metadata: %+v", verified)
	}
	var payload struct {
		PayloadSHA256 string `json:"payload_sha256"`
		ExportedBy    string `json:"exported_by"`
	}
	if err := json.Unmarshal(verified.Payload, &payload); err != nil {
		t.Fatalf("decode envelope payload: %v", err)
	}
	if payload.PayloadSHA256 != batch.PayloadSHA256 || payload.ExportedBy != "officer-maker" {
		t.Fatalf("envelope payload mismatch: %+v", payload)
	}
	if signer.KeyID() != batch.SignerKeyID {
		t.Fatal("signer kid not recorded")
	}

	// Byte-identical re-export fails closed as a replay.
	if _, err := service.ExportStatement(ctx, "TSA:NGN", "NGN", periodStart, periodEnd, "officer-maker"); !errors.Is(err, ErrExportReplay) {
		t.Fatalf("replay must fail with ErrExportReplay, got %v", err)
	}

	// The stored payload streams back with integrity intact.
	stored, hash, err := store.GetExportPayload(ctx, batch.BatchID)
	if err != nil {
		t.Fatalf("GetExportPayload: %v", err)
	}
	if stored != batch.Payload || hash != batch.PayloadSHA256 {
		t.Fatal("stored payload diverges from exported payload")
	}
}

func TestExportCreditTransfersFromApprovedDisbursements(t *testing.T) {
	service, _, pool, _, verifier := openService(t)
	seedJournal(t, pool)
	ctx := context.Background()

	batch, err := service.ExportCreditTransfers(ctx, "NGN", periodStart, periodEnd, "officer-payments")
	if err != nil {
		t.Fatalf("ExportCreditTransfers: %v", err)
	}
	if batch.EntryCount != 1 || batch.TotalCreditMinor != 100000 {
		t.Fatalf("unexpected payment batch: %+v", batch)
	}
	var probe pain001Probe
	if err := xml.Unmarshal([]byte(batch.Payload), &probe); err != nil {
		t.Fatalf("exported payload is not pain.001-shaped: %v", err)
	}
	if probe.Initn.GrpHdr.CtrlSum != "1000.00" || probe.Initn.GrpHdr.NbOfTxs != 1 {
		t.Fatalf("control sum / tx count mismatch: %+v", probe.Initn.GrpHdr)
	}
	if _, err := verifier.Verify(batch.Envelope); err != nil {
		t.Fatalf("payment export envelope does not verify: %v", err)
	}

	// A period with no instructions fails closed rather than emitting an empty feed.
	empty := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if _, err := service.ExportCreditTransfers(ctx, "NGN", empty, empty.AddDate(0, 0, 30), "officer-payments"); err == nil {
		t.Fatal("empty instruction scope must fail")
	}
}

func TestPeriodCloseEnforcesMakerChecker(t *testing.T) {
	service, _, pool, _, _ := openService(t)
	seedJournal(t, pool)
	ctx := context.Background()

	close, err := service.RequestPeriodClose(ctx, periodStart, periodEnd, "officer-maker")
	if err != nil {
		t.Fatalf("RequestPeriodClose: %v", err)
	}
	if close.Status != "PENDING_APPROVAL" || close.RequestedBy != "officer-maker" {
		t.Fatalf("unexpected close state: %+v", close)
	}

	// Maker cannot self-approve (dual control).
	if _, err := service.ApprovePeriodClose(ctx, close.PeriodCloseID, "officer-maker"); !errors.Is(err, ErrPeriodApprove) {
		t.Fatalf("self-approval must fail with ErrPeriodApprove, got %v", err)
	}

	// Checker approves; trial balance is frozen.
	approved, err := service.ApprovePeriodClose(ctx, close.PeriodCloseID, "officer-checker")
	if err != nil {
		t.Fatalf("ApprovePeriodClose: %v", err)
	}
	if approved.Status != "CLOSED" || approved.ApprovedBy != "officer-checker" || approved.ApprovedAt == nil {
		t.Fatalf("unexpected approved close: %+v", approved)
	}
	var frozen []TrialBalanceRow
	if err := json.Unmarshal(approved.TrialBalance, &frozen); err != nil || len(frozen) != 3 {
		t.Fatalf("frozen trial balance invalid: %v (%d rows)", err, len(frozen))
	}

	// Second approval fails (already closed).
	if _, err := service.ApprovePeriodClose(ctx, close.PeriodCloseID, "officer-third"); !errors.Is(err, ErrPeriodApprove) {
		t.Fatalf("double approval must fail, got %v", err)
	}

	// Duplicate close for the same period fails closed.
	if _, err := service.RequestPeriodClose(ctx, periodStart, periodEnd, "officer-other"); !errors.Is(err, ErrPeriodExists) {
		t.Fatalf("duplicate period close must fail with ErrPeriodExists, got %v", err)
	}
}

func TestReconciliationDetectsDriftAndBalances(t *testing.T) {
	service, _, pool, _, _ := openService(t)
	seedJournal(t, pool)
	ctx := context.Background()

	// No exports yet: every account must surface as a difference.
	run, err := service.RunReconciliation(ctx, periodStart, periodEnd, "officer-recon")
	if err != nil {
		t.Fatalf("RunReconciliation: %v", err)
	}
	if run.Balanced {
		t.Fatal("run must not balance before any export")
	}
	var differences []AccountDifference
	if err := json.Unmarshal(run.Differences, &differences); err != nil {
		t.Fatalf("decode differences: %v", err)
	}
	if len(differences) != 3 {
		t.Fatalf("differences = %d, want 3 unexported accounts", len(differences))
	}

	// Export every account's statement, then the run balances.
	for _, account := range []string{"TSA:NGN", "REVENUE:NGN", "CVFF:EXPENSE:NGN"} {
		if _, err := service.ExportStatement(ctx, account, "NGN", periodStart, periodEnd, "officer-exporter"); err != nil {
			t.Fatalf("ExportStatement(%s): %v", account, err)
		}
	}
	run, err = service.RunReconciliation(ctx, periodStart, periodEnd, "officer-recon")
	if err != nil {
		t.Fatalf("RunReconciliation: %v", err)
	}
	if !run.Balanced {
		detail, _ := json.Marshal(run.Differences)
		t.Fatalf("run must balance after full export: %s", detail)
	}
	if run.JournalEntryCount != 4 || run.ExportedEntryCount != 4 {
		t.Fatalf("unexpected run counts: %+v", run)
	}

	// Runs are listed back as persisted evidence.
	runs, err := service.store.ListReconciliationRuns(ctx, periodStart, periodEnd, 10)
	if err != nil {
		t.Fatalf("ListReconciliationRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
}

// stubAuthenticator maps the Authorization header straight to a subject for
// HTTP-level tests (test-only; the production handler is wired to the real
// Keycloak verifier).
type stubAuthenticator struct{}

func (stubAuthenticator) Authenticate(_ context.Context, header string) (string, error) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return "", fmt.Errorf("missing bearer token")
	}
	return header[len(prefix):], nil
}
