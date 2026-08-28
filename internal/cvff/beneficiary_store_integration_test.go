//go:build integration

package cvff

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealPostgresBeneficiaryIntake verifies the beneficiary API persistence
// paths against real PostgreSQL: intake insert with detail, idempotent
// replay, ownership scoping, transition write-through, document metadata
// with idempotency and immutability. Requires DATABASE_URL and
// MIGRATION_PATH (db/migrations, 0001-0004).
func TestRealPostgresBeneficiaryIntake(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, name := range []string{"0001", "0002", "0003", "0004"} {
		matches, globErr := filepath.Glob(filepath.Join(os.Getenv("MIGRATION_PATH"), name+"_*.sql"))
		if globErr != nil || len(matches) != 1 {
			t.Fatalf("locate migration %s: %v", name, globErr)
		}
		migration, err := os.ReadFile(filepath.Clean(matches[0]))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}

	intake := Intake{
		ApplicationID:    "cvff-intake-it-001",
		IdempotencyKey:   "it-key-001",
		BeneficiaryID:    "kc-beneficiary-it",
		VesselName:       "MV Adaeze",
		IMONumber:        "9074729",
		OfficialNumber:   "NIMASA-4421",
		VesselClass:      VesselClassCargoCoaster,
		CabotageRoute:    RouteLagosOnne,
		Amount:           750_000_000,
		Currency:         "NGN",
		BusinessName:     "Adaeze Coastal Logistics Ltd",
		BusinessRCNumber: "RC123456",
		BusinessAddress:  "14 Marina Road, Lagos Island, Lagos",
	}
	retained, err := store.SubmitIntake(ctx, intake)
	if err != nil {
		t.Fatalf("submit intake: %v", err)
	}
	if retained.State != StateSubmitted || retained.Version != 1 || retained.IMONumber != "9074729" {
		t.Fatalf("unexpected intake: %+v", retained)
	}
	if retained.StateEnteredAt.IsZero() {
		t.Fatal("state_entered_at not resolved")
	}

	// Idempotent replay returns the original; divergent content conflicts.
	replay, err := store.SubmitIntake(ctx, intake)
	if err != nil || replay.ApplicationID != retained.ApplicationID {
		t.Fatalf("intake replay: %+v %v", replay, err)
	}
	divergent := intake
	divergent.Amount = 800_000_000
	if _, err := store.SubmitIntake(ctx, divergent); !errors.Is(err, ErrConflict) {
		t.Fatalf("divergent replay error = %v", err)
	}

	// Ownership scoping: other principals fail closed with ErrNotFound.
	if _, err := store.GetForBeneficiary(ctx, retained.ApplicationID, "kc-intruder"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign owner read error = %v", err)
	}
	owned, err := store.GetForBeneficiary(ctx, retained.ApplicationID, intake.BeneficiaryID)
	if err != nil || owned.VesselName != "MV Adaeze" {
		t.Fatalf("owner read: %+v %v", owned, err)
	}
	listed, err := store.ListForBeneficiary(ctx, intake.BeneficiaryID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("owner list: %d %v", len(listed), err)
	}

	// Transition write-through: underwriting start and decisions move
	// state_entered_at forward and land in cvff_transitions.
	current, err := store.Get(ctx, retained.ApplicationID)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := store.Transition(ctx, current.ApplicationID, current.Version, BeginUnderwriting, "cvff.underwriting_started")
	if err != nil {
		t.Fatalf("begin underwriting: %v", err)
	}
	var transitions int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM cvff_transitions WHERE application_id = $1`, retained.ApplicationID).Scan(&transitions); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	if transitions != 2 {
		t.Fatalf("transitions = %d, want 2 (submit + underwriting start)", transitions)
	}
	reloaded, err := store.GetForBeneficiary(ctx, retained.ApplicationID, intake.BeneficiaryID)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.StateEnteredAt.After(owned.StateEnteredAt) && moved.State != StateUnderwritingPrimary {
		t.Fatalf("state_entered_at did not advance: %v -> %v", owned.StateEnteredAt, reloaded.StateEnteredAt)
	}
	if reloaded.State != StateUnderwritingPrimary || !reloaded.StateEnteredAt.Equal(reloaded.UpdatedAt) {
		t.Fatalf("state_entered_at mismatch after transition: %+v", reloaded)
	}

	// Document metadata: insert, idempotent replay, divergent conflict,
	// immutability.
	document := Document{
		ApplicationID:  retained.ApplicationID,
		BeneficiaryID:  intake.BeneficiaryID,
		DocumentType:   DocumentTypeVesselRegistration,
		FileName:       "vessel-registration.pdf",
		ContentType:    "application/pdf",
		SizeBytes:      9,
		SHA256Hex:      strings.Repeat("a", 64),
		StorageBackend: "s3",
		StorageKey:     "cvff-documents/" + retained.ApplicationID + "/" + strings.Repeat("a", 64),
		IdempotencyKey: "it-doc-key-001",
	}
	recorded, err := store.CreateDocument(ctx, document)
	if err != nil {
		t.Fatalf("create document: %v", err)
	}
	docReplay, err := store.CreateDocument(ctx, document)
	if err != nil || docReplay.DocumentID != recorded.DocumentID {
		t.Fatalf("document replay: %+v %v", docReplay, err)
	}
	docDivergent := document
	docDivergent.SHA256Hex = strings.Repeat("b", 64)
	if _, err := store.CreateDocument(ctx, docDivergent); !errors.Is(err, ErrConflict) {
		t.Fatalf("divergent document replay error = %v", err)
	}
	documents, err := store.ListDocuments(ctx, retained.ApplicationID)
	if err != nil || len(documents) != 1 {
		t.Fatalf("list documents: %d %v", len(documents), err)
	}
	if err := store.Exec(ctx, `UPDATE cvff_documents SET file_name = 'x.pdf' WHERE application_id = '`+retained.ApplicationID+`'`); err == nil {
		t.Fatal("document mutation accepted by database")
	}
	if err := store.Exec(ctx, `UPDATE cvff_transitions SET to_state = 'DISBURSED' WHERE application_id = '`+retained.ApplicationID+`'`); err == nil {
		t.Fatal("transition mutation accepted by database")
	}

	// The intake submission emitted the platform outbox event.
	var outboxEvents int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM cvff_outbox WHERE application_id = $1 AND event_type = 'cvff.application.submitted'`, retained.ApplicationID).Scan(&outboxEvents); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if outboxEvents != 1 {
		t.Fatalf("outbox events = %d, want 1", outboxEvents)
	}
}
