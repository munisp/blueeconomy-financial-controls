package cvff

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// applicationDetailColumns coalesces the nullable intake columns so rows
// seeded by intake paths that predate the beneficiary API still scan.
const applicationDetailColumns = applicationColumns + `, ` +
	`COALESCE(vessel_name, ''), COALESCE(imo_number, ''), COALESCE(official_number, ''), ` +
	`COALESCE(vessel_class, ''), COALESCE(cabotage_route, ''), ` +
	`COALESCE(business_name, ''), COALESCE(business_rc_number, ''), COALESCE(business_address, '')`

// stateEnteredAtExpression resolves the instant the current state was entered
// from the durable transition log, falling back to the creation time for rows
// that predate the transition log.
const stateEnteredAtExpression = `COALESCE((
	SELECT max(t.created_at) FROM cvff_transitions t
	WHERE t.application_id = cvff_applications.application_id
), cvff_applications.created_at)`

func scanApplicationDetail(row pgx.Row) (ApplicationDetail, error) {
	var retained ApplicationDetail
	err := row.Scan(
		&retained.ApplicationID, &retained.ExternalRef, &retained.BeneficiaryID, &retained.Amount,
		&retained.Currency, &retained.State, &retained.CreatedAt, &retained.UpdatedAt, &retained.Version,
		&retained.VesselName, &retained.IMONumber, &retained.OfficialNumber, &retained.VesselClass,
		&retained.CabotageRoute, &retained.BusinessName, &retained.BusinessRCNumber, &retained.BusinessAddress,
		&retained.StateEnteredAt)
	return retained, err
}

func scanDocument(row pgx.Row) (Document, error) {
	var retained Document
	err := row.Scan(&retained.DocumentID, &retained.ApplicationID, &retained.BeneficiaryID,
		&retained.DocumentType, &retained.FileName, &retained.ContentType, &retained.SizeBytes,
		&retained.SHA256Hex, &retained.StorageBackend, &retained.StorageKey, &retained.IdempotencyKey,
		&retained.CreatedAt)
	return retained, err
}

const documentColumns = `document_id, application_id, beneficiary_id, document_type, file_name, content_type, size_bytes, sha256_hex, storage_backend, storage_key, idempotency_key, created_at`

// SubmitIntake persists one beneficiary-submitted application in SUBMITTED
// state with its intake detail, the creation transition and the platform
// outbox event in one transaction. The idempotency key is bound to the
// durable external_ref: a replayed submission from the same beneficiary
// returns the original application, a key reused with different content is a
// conflict.
func (store *Store) SubmitIntake(ctx context.Context, intake Intake) (ApplicationDetail, error) {
	if errs := intake.Validate(); len(errs) > 0 {
		return ApplicationDetail{}, errs
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return ApplicationDetail{}, fmt.Errorf("begin intake: %w", err)
	}
	defer tx.Rollback(ctx)
	createdAt := time.Now().UTC()
	retained, err := scanApplicationDetail(tx.QueryRow(ctx, `
		INSERT INTO cvff_applications (
			application_id, external_ref, beneficiary_id, amount, currency, state, created_at, updated_at, version,
			vessel_name, imo_number, official_number, vessel_class, cabotage_route, business_name, business_rc_number, business_address)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7,1,$8,$9,$10,$11,$12,$13,$14,$15)
		RETURNING `+applicationDetailColumns+`, `+stateEnteredAtExpression,
		intake.ApplicationID, intake.IdempotencyKey, intake.BeneficiaryID, intake.Amount, intake.Currency, StateSubmitted, createdAt,
		intake.VesselName, intake.IMONumber, intake.OfficialNumber, intake.VesselClass, intake.CabotageRoute,
		intake.BusinessName, intake.BusinessRCNumber, intake.BusinessAddress))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "cvff_applications_external_ref_key" {
			return store.replayIntake(ctx, intake)
		}
		return ApplicationDetail{}, fmt.Errorf("insert cvff intake: %w", err)
	}
	if err := appendTransition(ctx, tx, retained.ApplicationID, StateSubmitted, StateSubmitted, RoleBeneficiary, intake.BeneficiaryID, "cvff.application.submitted", createdAt); err != nil {
		return ApplicationDetail{}, err
	}
	if err := appendEvent(ctx, tx, retained.ApplicationID, "cvff.application.submitted", retained, createdAt); err != nil {
		return ApplicationDetail{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplicationDetail{}, fmt.Errorf("commit intake: %w", err)
	}
	return retained, nil
}

// replayIntake resolves an idempotent replay: the same idempotency key and
// beneficiary returns the originally persisted application; anything else is
// a hard conflict.
func (store *Store) replayIntake(ctx context.Context, intake Intake) (ApplicationDetail, error) {
	existing, err := scanApplicationDetail(store.pool.QueryRow(ctx, `
		SELECT `+applicationDetailColumns+`, `+stateEnteredAtExpression+`
		FROM cvff_applications WHERE external_ref = $1`, intake.IdempotencyKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return ApplicationDetail{}, fmt.Errorf("replay cvff intake: %w", ErrNotFound)
	}
	if err != nil {
		return ApplicationDetail{}, fmt.Errorf("replay cvff intake: %w", err)
	}
	if existing.BeneficiaryID != intake.BeneficiaryID ||
		existing.VesselName != intake.VesselName ||
		existing.IMONumber != intake.IMONumber ||
		existing.OfficialNumber != intake.OfficialNumber ||
		existing.VesselClass != intake.VesselClass ||
		existing.CabotageRoute != intake.CabotageRoute ||
		existing.Amount != intake.Amount ||
		existing.Currency != intake.Currency ||
		existing.BusinessName != intake.BusinessName ||
		existing.BusinessRCNumber != intake.BusinessRCNumber ||
		existing.BusinessAddress != intake.BusinessAddress {
		return ApplicationDetail{}, fmt.Errorf("%w: idempotency key was already used with different content", ErrConflict)
	}
	return existing, nil
}

// GetForBeneficiary returns one application owned by the beneficiary. It fails
// closed: applications owned by another principal are indistinguishable from
// missing ones.
func (store *Store) GetForBeneficiary(ctx context.Context, applicationID string, beneficiaryID string) (ApplicationDetail, error) {
	retained, err := scanApplicationDetail(store.pool.QueryRow(ctx, `
		SELECT `+applicationDetailColumns+`, `+stateEnteredAtExpression+`
		FROM cvff_applications WHERE application_id = $1 AND beneficiary_id = $2`, applicationID, beneficiaryID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ApplicationDetail{}, ErrNotFound
	}
	if err != nil {
		return ApplicationDetail{}, fmt.Errorf("get cvff application for beneficiary: %w", err)
	}
	return retained, nil
}

// ListForBeneficiary returns every application owned by the beneficiary,
// newest first.
func (store *Store) ListForBeneficiary(ctx context.Context, beneficiaryID string) ([]ApplicationDetail, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT `+applicationDetailColumns+`, `+stateEnteredAtExpression+`
		FROM cvff_applications WHERE beneficiary_id = $1
		ORDER BY created_at DESC, application_id`, beneficiaryID)
	if err != nil {
		return nil, fmt.Errorf("list cvff applications for beneficiary: %w", err)
	}
	defer rows.Close()
	applications := make([]ApplicationDetail, 0)
	for rows.Next() {
		retained, scanErr := scanApplicationDetail(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan cvff application detail: %w", scanErr)
		}
		applications = append(applications, retained)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cvff applications: %w", err)
	}
	return applications, nil
}

// CreateDocument persists document metadata after the bytes are durably
// stored. The (application_id, idempotency_key) unique constraint makes
// retried uploads safe: an identical replay returns the original record, a
// key reused with different content is a hard conflict.
func (store *Store) CreateDocument(ctx context.Context, document Document) (Document, error) {
	if err := document.Validate(); err != nil {
		return Document{}, err
	}
	document.DocumentID = uuid.NewString()
	document.CreatedAt = time.Now().UTC()
	retained, err := scanDocument(store.pool.QueryRow(ctx, `
		INSERT INTO cvff_documents (`+documentColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING `+documentColumns,
		document.DocumentID, document.ApplicationID, document.BeneficiaryID, document.DocumentType,
		document.FileName, document.ContentType, document.SizeBytes, document.SHA256Hex,
		document.StorageBackend, document.StorageKey, document.IdempotencyKey, document.CreatedAt))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "cvff_documents_application_id_idempotency_key_key" {
			return store.replayDocument(ctx, document)
		}
		return Document{}, fmt.Errorf("insert cvff document: %w", err)
	}
	return retained, nil
}

func (store *Store) replayDocument(ctx context.Context, document Document) (Document, error) {
	existing, err := scanDocument(store.pool.QueryRow(ctx, `
		SELECT `+documentColumns+` FROM cvff_documents
		WHERE application_id = $1 AND idempotency_key = $2`, document.ApplicationID, document.IdempotencyKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, fmt.Errorf("replay cvff document: %w", ErrNotFound)
	}
	if err != nil {
		return Document{}, fmt.Errorf("replay cvff document: %w", err)
	}
	if existing.BeneficiaryID != document.BeneficiaryID ||
		existing.DocumentType != document.DocumentType ||
		existing.SHA256Hex != document.SHA256Hex ||
		existing.SizeBytes != document.SizeBytes {
		return Document{}, fmt.Errorf("%w: idempotency key was already used with different content", ErrConflict)
	}
	return existing, nil
}

// ListDocuments returns the document metadata recorded for one application in
// upload order. Callers must scope by ownership before calling.
func (store *Store) ListDocuments(ctx context.Context, applicationID string) ([]Document, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT `+documentColumns+` FROM cvff_documents
		WHERE application_id = $1 ORDER BY created_at, document_id`, applicationID)
	if err != nil {
		return nil, fmt.Errorf("list cvff documents: %w", err)
	}
	defer rows.Close()
	documents := make([]Document, 0)
	for rows.Next() {
		retained, scanErr := scanDocument(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan cvff document: %w", scanErr)
		}
		documents = append(documents, retained)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cvff documents: %w", err)
	}
	return documents, nil
}
