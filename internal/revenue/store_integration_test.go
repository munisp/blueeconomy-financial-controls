//go:build integration

package revenue

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
)

// Integration harness: real PostgreSQL, real migrations (DATABASE_URL +
// MIGRATION_PATH, repo convention). MIGRATION_PATH points at
// 0009_revenue_assurance.sql; the harness applies the 0008 tariff
// migration from the same directory first (debit notes reference
// assessments). Each test gets a clean public schema.
func openStore(t *testing.T) (*Store, *pgxpool.Pool, *envelope.Signer, *envelope.Verifier) {
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
		t.Fatalf("open pool: %v", err)
	}
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	for _, path := range []string{
		filepath.Join(filepath.Dir(migrationPath), "0008_tariff.sql"),
		migrationPath,
		// 0010 adds the revenue-intake assessments the recon batch reads.
		filepath.Join(filepath.Dir(migrationPath), "0010_revenue_intake.sql"),
	} {
		statement, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read migration %s: %v", path, err)
		}
		if _, err := pool.Exec(ctx, string(statement)); err != nil {
			t.Fatalf("apply migration %s: %v", path, err)
		}
	}
	public, _, seed := generateTestKey(t)
	signer, err := envelope.NewSigner("revenue-test-signer", seed)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool, signer)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := envelope.NewVerifier(map[string]string{
		"revenue-test-signer": base64.RawURLEncoding.EncodeToString(public),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, pool, signer, verifier
}

func generateTestKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	seed := make([]byte, ed25519.SeedSize)
	copy(seed, private.Seed())
	return public, private, seed
}

// seedAssessment inserts one immutable tariff assessment with charged lines
// (the fixture a debit note bills against). Test-only fixture.
func seedAssessment(t *testing.T, pool *pgxpool.Pool, assessmentID, entityRef string, usdTotal int64, lines ...noteLine) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tariff_assessments (assessment_id, idempotency_key, request_hash, request, as_of,
		     total_usd_minor, total_ngn_minor, requester, correlation_id)
		 VALUES ($1, $2, 'hash-' || $1, $3, DATE '2026-01-15', $4, 0, 'officer:assessor', 'corr-fixture')`,
		assessmentID, "idem-"+assessmentID,
		fmt.Sprintf(`{"entityRef":%q}`, entityRef), usdTotal); err != nil {
		t.Fatalf("seed assessment: %v", err)
	}
	for index, line := range lines {
		if _, err := pool.Exec(ctx,
			`INSERT INTO tariff_assessment_lines (assessment_id, line_no, instrument, agency, applicability,
			     basis, amount_minor, currency)
			 VALUES ($1, $2, $3, $4, 'CHARGED', 'fixture', $5, 'USD')`,
			assessmentID, index+1, line.Instrument, line.Agency, line.Amount); err != nil {
			t.Fatalf("seed assessment line: %v", err)
		}
	}
}

type noteLine = struct {
	Instrument string
	Agency     string
	Amount     int64
}

// TestDebitNoteLifecycleAndAudit covers the lifecycle gate: create, dual-
// controlled issue, debtor ack/dispute, audited transitions, idempotent
// replay, and the signed envelope.
func TestDebitNoteLifecycleAndAudit(t *testing.T) {
	store, pool, _, verifier := openStore(t)
	defer pool.Close()
	ctx := context.Background()
	seedAssessment(t, pool, "assess-lc-1", "ACME-SHIPPING", 500000,
		noteLine{"NPA_SHIP_DUES", "NPA", 400000}, noteLine{"NPA_LEVY_ACT", "NPA", 100000})

	request := IssueRequest{AssessmentID: "assess-lc-1", DueDate: "2026-03-01", EffectiveDate: "2026-02-01"}
	note, err := store.CreateDebitNote(ctx, request, "idem-dn-1", "officer:maker", "corr-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if note.State != StateDraft || note.Agency != "NPA" || note.AmountUSDMinor != 500000 || len(note.Lines) != 2 {
		t.Fatalf("draft note: %+v", note)
	}
	// Replay: same key + request returns the same note.
	replay, err := store.CreateDebitNote(ctx, request, "idem-dn-1", "officer:maker", "corr-1")
	if err != nil || replay.DebitNoteID != note.DebitNoteID {
		t.Fatalf("replay: %v / %+v", err, replay)
	}
	// Conflict: same key, different request.
	conflict := request
	conflict.DueDate = "2026-04-01"
	if _, err := store.CreateDebitNote(ctx, conflict, "idem-dn-1", "officer:maker", "corr-1"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("want idempotency conflict, got %v", err)
	}
	// Self-issuance refused (maker != checker).
	if _, err := store.Issue(ctx, note.DebitNoteID, "officer:maker"); !errors.Is(err, ErrMakerChecker) {
		t.Fatalf("want maker/checker refusal, got %v", err)
	}
	issued, err := store.Issue(ctx, note.DebitNoteID, "officer:checker")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if issued.State != StateIssued || issued.Checker != "officer:checker" {
		t.Fatalf("issued: %+v", issued)
	}
	if !strings.HasPrefix(issued.DocumentNumber, "NPA-2026-") {
		t.Fatalf("document number %q not in the NPA 2026 series", issued.DocumentNumber)
	}
	// Envelope: sealed and verifiable.
	bundle, _, err := store.GetDebitNoteEnvelope(ctx, note.DebitNoteID)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	verified, err := verifier.Verify(bundle)
	if err != nil {
		t.Fatalf("envelope does not verify: %v", err)
	}
	if verified.ArtifactKind != ArtifactDebitNote || verified.ArtifactID != note.DebitNoteID {
		t.Fatalf("verified artifact: %+v", verified)
	}
	if !strings.Contains(string(verified.Payload), issued.DocumentNumber) {
		t.Fatalf("payload missing document number: %s", verified.Payload)
	}
	// Debtor-facing moves.
	if _, err := store.Transition(ctx, note.DebitNoteID, TransitionRequest{ToState: StateAcked}, "debtor:acme"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if _, err := store.Transition(ctx, note.DebitNoteID, TransitionRequest{ToState: StateDisputed, Reason: "amount contested by debtor"}, "debtor:acme"); err != nil {
		t.Fatalf("dispute: %v", err)
	}
	// Illegal move refused.
	if _, err := store.Transition(ctx, note.DebitNoteID, TransitionRequest{ToState: StateIssued}, "officer:checker"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("want invalid transition, got %v", err)
	}
	// Manual SETTLED refused (recon-only).
	if _, err := store.Transition(ctx, note.DebitNoteID, TransitionRequest{ToState: StateSettled}, "officer:checker"); err == nil {
		t.Fatal("manual SETTLED must be refused")
	}
	// Audit: DRAFT-create, ISSUE, ACK, DISPUTE = 4 transitions.
	transitions, err := store.ListTransitions(ctx, note.DebitNoteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 4 {
		t.Fatalf("transitions: %+v", transitions)
	}
	if transitions[1].ToState != StateIssued || transitions[1].Approver != "officer:maker" {
		t.Fatalf("issuance audit: %+v", transitions[1])
	}
	// Outbox events exist for create + transitions.
	var outboxCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM revenue_outbox WHERE subject_id = $1`, note.DebitNoteID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 4 {
		t.Fatalf("outbox count %d, want 4", outboxCount)
	}
}

// TestDebitNoteCancellationDualControl covers CANCELLED-with-reason under
// maker/checker separation.
func TestDebitNoteCancellationDualControl(t *testing.T) {
	store, pool, _, _ := openStore(t)
	defer pool.Close()
	ctx := context.Background()
	seedAssessment(t, pool, "assess-cx-1", "ACME-SHIPPING", 120000,
		noteLine{"NIWA_INLAND_CHARGE", "NIWA", 120000})
	note, err := store.CreateDebitNote(ctx,
		IssueRequest{AssessmentID: "assess-cx-1", DueDate: "2026-03-01"}, "idem-cx-1", "officer:maker", "corr-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Issue(ctx, note.DebitNoteID, "officer:checker"); err != nil {
		t.Fatal(err)
	}
	// Reason required.
	if _, err := store.Transition(ctx, note.DebitNoteID, TransitionRequest{ToState: StateCancelled}, "officer:other"); err == nil {
		t.Fatal("cancellation without reason must be refused")
	}
	// Maker cannot cancel their own note (SQL-guarded).
	if _, err := store.Transition(ctx, note.DebitNoteID,
		TransitionRequest{ToState: StateCancelled, Reason: "issued in error, superseded"}, "officer:maker"); !errors.Is(err, ErrMakerChecker) {
		t.Fatalf("want maker/checker refusal, got %v", err)
	}
	cancelled, err := store.Transition(ctx, note.DebitNoteID,
		TransitionRequest{ToState: StateCancelled, Reason: "assessment voided by agency review"}, "officer:reviewer")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.State != StateCancelled || cancelled.CancelReason == "" {
		t.Fatalf("cancelled: %+v", cancelled)
	}
}

// TestMakerCheckerSQLEnforcement proves the SQL layer itself refuses
// self-approval, even when the service checks are bypassed.
func TestMakerCheckerSQLEnforcement(t *testing.T) {
	store, pool, _, _ := openStore(t)
	defer pool.Close()
	ctx := context.Background()
	seedAssessment(t, pool, "assess-mc-1", "ACME-SHIPPING", 90000,
		noteLine{"NPA_SHIP_DUES", "NPA", 90000})
	note, err := store.CreateDebitNote(ctx,
		IssueRequest{AssessmentID: "assess-mc-1", DueDate: "2026-03-01"}, "idem-mc-1", "officer:maker", "corr-3")
	if err != nil {
		t.Fatal(err)
	}
	// Direct SQL: checker = maker violates the table CHECK.
	if _, err := pool.Exec(ctx,
		`UPDATE revenue_debit_notes SET checker = maker WHERE debit_note_id = $1`, note.DebitNoteID); err == nil {
		t.Fatal("SQL accepted checker = maker")
	}
	// Direct SQL: transition approver = actor violates the audit CHECK.
	if _, err := pool.Exec(ctx,
		`INSERT INTO revenue_debit_note_transitions (transition_id, debit_note_id, from_state, to_state, actor, approver)
		 VALUES ($1, $2, 'DRAFT', 'ISSUED', 'same-officer', 'same-officer')`,
		uuid.NewString(), note.DebitNoteID); err == nil {
		t.Fatal("SQL accepted approver = actor on transition")
	}
	// Direct SQL: the guarded issuance UPDATE refuses self-issuance even
	// when the caller skips the service check.
	result, err := pool.Exec(ctx,
		`UPDATE revenue_debit_notes SET state = 'ISSUED', checker = $2
		 WHERE debit_note_id = $1 AND state = 'DRAFT' AND maker <> $2`,
		note.DebitNoteID, "officer:maker")
	if err != nil {
		t.Fatalf("guarded update errored unexpectedly: %v", err)
	}
	if result.RowsAffected() != 0 {
		t.Fatal("guarded SQL issued a note for maker == checker")
	}
	// Same guarded UPDATE with a distinct checker succeeds.
	result, err = pool.Exec(ctx,
		`UPDATE revenue_debit_notes SET state = 'ISSUED', checker = $2,
		        document_number = 'NPA-2026-999001', envelope = '{}'::jsonb, envelope_jws = 'x.y.z'
		 WHERE debit_note_id = $1 AND state = 'DRAFT' AND maker <> $2`,
		note.DebitNoteID, "officer:checker")
	if err != nil || result.RowsAffected() != 1 {
		t.Fatalf("guarded update with distinct checker: %v / %d", err, result.RowsAffected())
	}
}

// TestSplitRuleDualControlAndDeterminism covers versioned rule admin and
// asOf determinism before/after a rule change.
func TestSplitRuleDualControlAndDeterminism(t *testing.T) {
	store, pool, _, _ := openStore(t)
	defer pool.Close()
	ctx := context.Background()

	input := func(beneficiary string, bps int64, from, to string) SplitRuleInput {
		return SplitRuleInput{
			RevenueLine: "TEST_LINE", Agency: "NPA", Beneficiary: beneficiary, ShareBps: bps,
			StatutoryReference: "test fixture rule", EffectiveFrom: from, EffectiveTo: to,
		}
	}
	// v1: 70/30 from 2026-01-01.
	agencyV1, err := store.CreateSplitRule(ctx, input(BeneficiaryAgencyRetained, 7000, "2026-01-01", "2026-05-31"), "officer:maker")
	if err != nil {
		t.Fatal(err)
	}
	fgnV1, err := store.CreateSplitRule(ctx, input(BeneficiaryFGN, 3000, "2026-01-01", "2026-05-31"), "officer:maker")
	if err != nil {
		t.Fatal(err)
	}
	// Self-activation refused.
	if _, err := store.ActivateSplitRule(ctx, agencyV1.RuleID, "officer:maker"); !errors.Is(err, ErrMakerChecker) {
		t.Fatalf("want maker/checker refusal, got %v", err)
	}
	for _, rule := range []SplitRuleRow{agencyV1, fgnV1} {
		if _, err := store.ActivateSplitRule(ctx, rule.RuleID, "officer:checker"); err != nil {
			t.Fatalf("activate: %v", err)
		}
	}
	// v2: 60/40 from 2026-06-01 (windows are exclusive of v1's).
	v2Agency, _ := store.CreateSplitRule(ctx, input(BeneficiaryAgencyRetained, 6000, "2026-06-01", ""), "officer:maker")
	v2FGN, _ := store.CreateSplitRule(ctx, input(BeneficiaryFGN, 4000, "2026-06-01", ""), "officer:maker")
	for _, rule := range []SplitRuleRow{v2Agency, v2FGN} {
		if _, err := store.ActivateSplitRule(ctx, rule.RuleID, "officer:checker"); err != nil {
			t.Fatalf("activate v2: %v", err)
		}
	}
	asOf := func(date string) time.Time {
		parsed, _ := time.Parse("2006-01-02", date)
		return parsed.UTC()
	}
	// Before the change: 70/30.
	before, err := store.ComputeSplitAt(ctx, "TEST_LINE", 1000000, "USD", asOf("2026-03-15"))
	if err != nil {
		t.Fatalf("compute before: %v", err)
	}
	// After: 60/40.
	after, err := store.ComputeSplitAt(ctx, "TEST_LINE", 1000000, "USD", asOf("2026-07-15"))
	if err != nil {
		t.Fatalf("compute after: %v", err)
	}
	if before.Allocations[0].AmountMinor != 700000 || after.Allocations[0].AmountMinor != 600000 {
		t.Fatalf("window-dependent splits: %+v / %+v", before.Allocations, after.Allocations)
	}
	// Determinism: repeat computations are byte-identical.
	repeat, err := store.ComputeSplitAt(ctx, "TEST_LINE", 1000000, "USD", asOf("2026-03-15"))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%+v", before.Allocations) != fmt.Sprintf("%+v", repeat.Allocations) {
		t.Fatalf("non-deterministic: %+v vs %+v", before.Allocations, repeat.Allocations)
	}
	// Incomplete window (no rules in force) fails closed.
	if _, err := store.ComputeSplitAt(ctx, "TEST_LINE", 1000000, "USD", asOf("2020-01-01")); !errors.Is(err, ErrSplitIncomplete) {
		t.Fatalf("want incomplete window refusal, got %v", err)
	}
	// Seeded NPA_SHIP_DUES rules (provisional 70/30) work out of the box.
	seeded, err := store.ComputeSplitAt(ctx, "NPA_SHIP_DUES", 3675000, "USD", asOf("2026-01-15"))
	if err != nil {
		t.Fatalf("seeded split: %v", err)
	}
	if !seeded.Provisional || len(seeded.Allocations) != 2 {
		t.Fatalf("seeded split: %+v", seeded)
	}
	// Seeded CVFF rule: 100% fiduciary segregation.
	cvff, err := store.ComputeSplitAt(ctx, "CABOTAGE_SURCHARGE", 80000, "USD", asOf("2026-01-15"))
	if err != nil {
		t.Fatalf("cvff split: %v", err)
	}
	if len(cvff.Allocations) != 1 || cvff.Allocations[0].Beneficiary != BeneficiaryCVFFFiduciary || cvff.Allocations[0].AmountMinor != 80000 {
		t.Fatalf("cvff fiduciary segregation: %+v", cvff.Allocations)
	}
}

// signStatement seals a statement fixture into an ingest envelope.
func signStatement(t *testing.T, signer *envelope.Signer, statement StatementRequest) []byte {
	t.Helper()
	bundle, _, err := signer.Sign(ArtifactBankStatement, uuid.NewString(), statement, time.Now().UTC())
	if err != nil {
		t.Fatalf("sign statement: %v", err)
	}
	return bundle
}

func issueNoteFor(t *testing.T, store *Store, pool *pgxpool.Pool, assessmentID string, usd int64, dueDate string, lines ...noteLine) DebitNote {
	t.Helper()
	ctx := context.Background()
	seedAssessment(t, pool, assessmentID, "ACME-SHIPPING", usd, lines...)
	note, err := store.CreateDebitNote(ctx,
		IssueRequest{AssessmentID: assessmentID, DueDate: dueDate}, "idem-"+assessmentID, "officer:maker", "corr-"+assessmentID)
	if err != nil {
		t.Fatalf("create note: %v", err)
	}
	issued, err := store.Issue(ctx, note.DebitNoteID, "officer:checker")
	if err != nil {
		t.Fatalf("issue note: %v", err)
	}
	return issued
}

func statementWith(lines ...StatementLine) StatementRequest {
	return StatementRequest{
		Bank: "CBN", AccountRef: "TSA-0001", StatementRef: "STMT-" + uuid.NewString()[:8],
		PeriodStart: "2026-02-01", PeriodEnd: "2026-02-28", Lines: lines,
	}
}

func credit(ref, date string, amount int64) StatementLine {
	return StatementLine{BankReference: ref, ValueDate: date, Direction: "CREDIT", AmountMinor: amount, Currency: "USD"}
}

// TestThreeWayReconAllClassesAndCleanMatch is the reconciliation gate: one
// batch produces a clean three-way match (note -> SETTLED, fully audited)
// plus every exception class; a second run is idempotent.
func TestThreeWayReconAllClassesAndCleanMatch(t *testing.T) {
	store, pool, signer, verifier := openStore(t)
	defer pool.Close()
	ctx := context.Background()

	// Leg (a): debit notes.
	clean := issueNoteFor(t, store, pool, "assess-rc-clean", 500000, "2026-03-15",
		noteLine{"NPA_SHIP_DUES", "NPA", 500000})
	mismatched := issueNoteFor(t, store, pool, "assess-rc-mismatch", 700000, "2026-03-15",
		noteLine{"NPA_SHIP_DUES", "NPA", 700000})
	pastDue := issueNoteFor(t, store, pool, "assess-rc-pastdue", 900000, "2026-01-31",
		noteLine{"NPA_SHIP_DUES", "NPA", 900000})

	// Leg (b): settlements.
	settle := func(id string, input SettlementInput) Settlement {
		t.Helper()
		settlement, err := store.RecordSettlement(ctx, input, "idem-set-"+id, "officer:collections")
		if err != nil {
			t.Fatalf("record settlement %s: %v", id, err)
		}
		return settlement
	}
	settle("clean", SettlementInput{DebitNoteID: clean.DebitNoteID, BankReference: "BR-CLEAN-1",
		AmountMinor: 500000, Currency: "USD", PayerRef: "ACME-SHIPPING", ValueDate: "2026-02-10"})
	settle("mismatch", SettlementInput{DebitNoteID: mismatched.DebitNoteID, BankReference: "BR-MISM-1",
		AmountMinor: 699999, Currency: "USD", PayerRef: "ACME-SHIPPING", ValueDate: "2026-02-10"})
	settle("orphan", SettlementInput{BankReference: "BR-GHOST-1",
		AmountMinor: 123400, Currency: "USD", PayerRef: "GHOST-LINE", ValueDate: "2026-02-10"})
	settle("dup1", SettlementInput{BankReference: "BR-DUP-1",
		AmountMinor: 55000, Currency: "USD", PayerRef: "ACME-SHIPPING", ValueDate: "2026-02-11"})
	settle("dup2", SettlementInput{BankReference: "BR-DUP-1",
		AmountMinor: 55000, Currency: "USD", PayerRef: "ACME-SHIPPING", ValueDate: "2026-02-12"})

	// Leg (c): signed statement ingest (clean line + unmatched line).
	bundle := signStatement(t, signer, statementWith(
		credit("BR-CLEAN-1", "2026-02-10", 500000),
		credit("BR-MISM-1", "2026-02-10", 699999),
		credit("BR-UNMATCHED-LINE", "2026-02-15", 77700),
	))
	if _, err := store.IngestStatement(ctx, verifier, bundle, "officer:treasury"); err != nil {
		t.Fatalf("ingest statement: %v", err)
	}

	asOf, _ := time.Parse("2006-01-02", "2026-02-28")
	summary, err := store.RunRecon(ctx, "MANUAL", "officer:recon", asOf.UTC())
	if err != nil {
		t.Fatalf("run recon: %v", err)
	}
	if summary.MatchedCount != 1 {
		t.Fatalf("matched %d, want 1", summary.MatchedCount)
	}
	// Clean match settles the note with a recon-audited transition.
	settled, err := store.GetDebitNote(ctx, clean.DebitNoteID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != StateSettled {
		t.Fatalf("clean note state %s, want SETTLED", settled.State)
	}
	transitions, err := store.ListTransitions(ctx, clean.DebitNoteID)
	if err != nil {
		t.Fatal(err)
	}
	last := transitions[len(transitions)-1]
	if last.ToState != StateSettled || last.Actor != reconEngineActor {
		t.Fatalf("settlement transition audit: %+v", last)
	}
	// Every exception class present; the duplicate-reference scenario
	// freezes BOTH siblings (fail-closed), so DUPLICATE_BANK_REF appears
	// twice, everything else exactly once.
	exceptions, err := store.ListExceptions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, exception := range exceptions {
		counts[exception.Class]++
	}
	for _, class := range []string{ExceptionUnmatchedAssessment, ExceptionUnmatchedSettlement,
		ExceptionUnmatchedStatement, ExceptionAmountMismatch} {
		if counts[class] != 1 {
			t.Fatalf("class %s count %d, want 1; queue: %+v", class, counts[class], exceptions)
		}
	}
	if counts[ExceptionDuplicateBankRef] != 2 {
		t.Fatalf("duplicate bank ref count %d, want 2; queue: %+v", counts[ExceptionDuplicateBankRef], exceptions)
	}
	_ = pastDue

	// Re-run: idempotent — no new exceptions, no duplicate matches.
	second, err := store.RunRecon(ctx, "MANUAL", "officer:recon", asOf.UTC())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.MatchedCount != 0 {
		t.Fatalf("second run matched %d, want 0", second.MatchedCount)
	}
	after, err := store.ListExceptions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(exceptions) {
		t.Fatalf("re-run raised %d exceptions, want %d (dedupe)", len(after), len(exceptions))
	}

	// Resolution workflow: audited, note required.
	var target Exception
	for _, exception := range exceptions {
		if exception.Class == ExceptionAmountMismatch {
			target = exception
		}
	}
	if _, err := store.ResolveException(ctx, target.ExceptionID, "officer:recon-lead", "short"); err == nil {
		t.Fatal("short resolution note must be refused")
	}
	resolved, err := store.ResolveException(ctx, target.ExceptionID, "officer:recon-lead",
		"debtor underpaid by 1 minor unit; debit note re-issued for the delta")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.State != ExceptionResolved || resolved.Resolver != "officer:recon-lead" {
		t.Fatalf("resolved: %+v", resolved)
	}
	var eventCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM recon_exception_events WHERE exception_id = $1`, target.ExceptionID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 2 { // RAISED + RESOLVED
		t.Fatalf("exception events %d, want 2", eventCount)
	}
}

// TestStatementReingestIdempotent: the same signed statement ingested twice
// produces no duplicate rows; a conflicting bank reference raises
// DUPLICATE_BANK_REF instead of inserting.
func TestStatementReingestIdempotent(t *testing.T) {
	store, pool, signer, verifier := openStore(t)
	defer pool.Close()
	ctx := context.Background()

	bundle := signStatement(t, signer, statementWith(
		credit("BR-IDEM-1", "2026-02-10", 100000),
		credit("BR-IDEM-2", "2026-02-11", 200000),
	))
	first, err := store.IngestStatement(ctx, verifier, bundle, "officer:treasury")
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	second, err := store.IngestStatement(ctx, verifier, bundle, "officer:treasury")
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if !second.Replay || second.StatementID != first.StatementID {
		t.Fatalf("re-ingest not a replay: %+v vs %+v", second, first)
	}
	var lineCount, statementCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM bank_statements`).Scan(&statementCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM bank_statement_lines`).Scan(&lineCount); err != nil {
		t.Fatal(err)
	}
	if statementCount != 1 || lineCount != 2 {
		t.Fatalf("re-ingest duplicated rows: %d statements, %d lines", statementCount, lineCount)
	}

	// Overlapping statement: one identical line (deduped) + one new line.
	overlap := signStatement(t, signer, statementWith(
		credit("BR-IDEM-1", "2026-02-10", 100000),
		credit("BR-IDEM-3", "2026-02-12", 300000),
	))
	third, err := store.IngestStatement(ctx, verifier, overlap, "officer:treasury")
	if err != nil {
		t.Fatalf("overlap ingest: %v", err)
	}
	if third.DedupedLines != 1 {
		t.Fatalf("overlap dedupe: %+v", third)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM bank_statement_lines`).Scan(&lineCount); err != nil {
		t.Fatal(err)
	}
	if lineCount != 3 {
		t.Fatalf("overlap inserted duplicates: %d lines", lineCount)
	}

	// Conflicting re-delivery of a known reference raises DUPLICATE_BANK_REF.
	conflict := signStatement(t, signer, statementWith(credit("BR-IDEM-2", "2026-02-11", 999999)))
	if _, err := store.IngestStatement(ctx, verifier, conflict, "officer:treasury"); err != nil {
		t.Fatalf("conflict ingest: %v", err)
	}
	exceptions, err := store.ListExceptions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, exception := range exceptions {
		if exception.Class == ExceptionDuplicateBankRef {
			found = true
		}
	}
	if !found {
		t.Fatalf("conflicting reference did not raise DUPLICATE_BANK_REF: %+v", exceptions)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM bank_statement_lines WHERE bank_reference = 'BR-IDEM-2'`).Scan(&lineCount); err != nil {
		t.Fatal(err)
	}
	if lineCount != 1 {
		t.Fatalf("conflicting reference inserted: %d rows", lineCount)
	}

	// Fail-closed: a forged statement is refused.
	_, _, otherSeed := generateTestKey(t)
	forged, err := envelope.NewSigner("revenue-test-signer", otherSeed)
	if err != nil {
		t.Fatal(err)
	}
	forgedBundle := signStatement(t, forged, statementWith(credit("BR-FORGED", "2026-02-12", 1)))
	if _, err := store.IngestStatement(ctx, verifier, forgedBundle, "officer:treasury"); !errors.Is(err, envelope.ErrSignature) {
		t.Fatalf("forged statement must fail signature verification, got %v", err)
	}
}

// TestRemittanceAdviceSignedAndIdempotent: the split artifact is sealed,
// verifiable and replay-safe.
func TestRemittanceAdviceSignedAndIdempotent(t *testing.T) {
	store, pool, _, verifier := openStore(t)
	defer pool.Close()
	ctx := context.Background()

	settlement, err := store.RecordSettlement(ctx, SettlementInput{
		BankReference: "BR-ADV-1", AmountMinor: 3675000, Currency: "USD",
		PayerRef: "ACME-SHIPPING", ValueDate: "2026-02-10",
	}, "idem-adv-1", "officer:collections")
	if err != nil {
		t.Fatal(err)
	}
	asOf, _ := time.Parse("2006-01-02", "2026-02-15")
	advice, bundle, err := store.IssueRemittanceAdvice(ctx, settlement.SettlementID,
		"NPA_SHIP_DUES", asOf.UTC(), "idem-advice-1", "officer:treasury", "corr-adv-1")
	if err != nil {
		t.Fatalf("issue advice: %v", err)
	}
	var total int64
	for _, allocation := range advice.Allocations {
		total += allocation.AmountMinor
	}
	if total != 3675000 {
		t.Fatalf("allocations sum %d, want 3675000", total)
	}
	verified, err := verifier.Verify(bundle)
	if err != nil {
		t.Fatalf("advice envelope does not verify: %v", err)
	}
	if verified.ArtifactKind != ArtifactRemittanceAdvice || verified.ArtifactID != advice.AdviceID {
		t.Fatalf("verified artifact: %+v", verified)
	}
	// Replay returns the stored advice; a conflicting replay conflicts.
	replay, _, err := store.IssueRemittanceAdvice(ctx, settlement.SettlementID,
		"NPA_SHIP_DUES", asOf.UTC(), "idem-advice-1", "officer:treasury", "corr-adv-1")
	if err != nil || replay.AdviceID != advice.AdviceID {
		t.Fatalf("replay: %v / %+v", err, replay)
	}
	if _, _, err := store.IssueRemittanceAdvice(ctx, settlement.SettlementID,
		"CABOTAGE_SURCHARGE", asOf.UTC(), "idem-advice-1", "officer:treasury", "corr-adv-1"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("want idempotency conflict, got %v", err)
	}
}

// TestReportingPrimitives: real SQL over the scenario tables.
func TestReportingPrimitives(t *testing.T) {
	store, pool, signer, verifier := openStore(t)
	defer pool.Close()
	ctx := context.Background()

	settled := issueNoteFor(t, store, pool, "assess-rp-1", 500000, "2026-03-15",
		noteLine{"NPA_SHIP_DUES", "NPA", 500000})
	issueNoteFor(t, store, pool, "assess-rp-2", 300000, "2026-04-15",
		noteLine{"NIWA_INLAND_CHARGE", "NIWA", 300000})
	settlement, err := store.RecordSettlement(ctx, SettlementInput{
		DebitNoteID: settled.DebitNoteID, BankReference: "BR-RP-1", AmountMinor: 500000,
		Currency: "USD", PayerRef: "ACME-SHIPPING", ValueDate: "2026-02-10",
	}, "idem-rp-1", "officer:collections")
	if err != nil {
		t.Fatal(err)
	}
	bundle := signStatement(t, signer, statementWith(credit("BR-RP-1", "2026-02-10", 500000)))
	if _, err := store.IngestStatement(ctx, verifier, bundle, "officer:treasury"); err != nil {
		t.Fatal(err)
	}
	asOf, _ := time.Parse("2006-01-02", "2026-02-28")
	if _, err := store.RunRecon(ctx, "MANUAL", "officer:recon", asOf.UTC()); err != nil {
		t.Fatal(err)
	}
	_ = settlement

	from, _ := time.Parse("2006-01-02", "2026-01-01")
	to, _ := time.Parse("2006-01-02", "2026-12-31")
	agencies, err := store.RevenueByAgency(ctx, from.UTC(), to.UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(agencies) != 2 {
		t.Fatalf("agency report: %+v", agencies)
	}
	byAgency := map[string]AgencyRevenue{}
	for _, row := range agencies {
		byAgency[row.Agency] = row
	}
	if byAgency["NPA"].SettledUSDMinor != 500000 || byAgency["NPA"].SettledNotes != 1 {
		t.Fatalf("NPA row: %+v", byAgency["NPA"])
	}
	if byAgency["NIWA"].OutstandingUSD != 300000 {
		t.Fatalf("NIWA row: %+v", byAgency["NIWA"])
	}
	lines, err := store.RevenueByLine(ctx, from.UTC(), to.UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("line report: %+v", lines)
	}
	aging, err := store.AgingUnsettled(ctx, asOf.UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(aging) != 1 || aging[0].Agency != "NIWA" || aging[0].NoteCount != 1 || aging[0].Bucket != "0-30" {
		t.Fatalf("aging: %+v", aging)
	}
	counts, err := store.ExceptionCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The past-due-free scenario raises no exceptions; queue is empty.
	if len(counts) != 0 {
		t.Fatalf("exception counts: %+v", counts)
	}
}

// TestReconBatchActivityDB exercises the Temporal activity against the real
// store (the workflow driver is unit-tested separately).
func TestReconBatchActivityDB(t *testing.T) {
	store, pool, _, _ := openStore(t)
	defer pool.Close()
	ctx := context.Background()
	issueNoteFor(t, store, pool, "assess-act-1", 100000, "2026-01-15",
		noteLine{"NPA_SHIP_DUES", "NPA", 100000})
	activities, err := NewReconActivities(store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := activities.RunReconBatchActivity(ctx, ReconBatchInput{Actor: "scheduler:nightly", AsOf: "2026-02-28"})
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	if result.RunID == "" || result.ExceptionCount != 1 { // past-due UNMATCHED_ASSESSMENT
		t.Fatalf("activity result: %+v", result)
	}
	summary, err := store.GetRun(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.State != "COMPLETED" {
		t.Fatalf("run: %+v", summary)
	}
}
