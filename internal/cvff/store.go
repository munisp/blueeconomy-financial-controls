package cvff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return NewStore(pool), nil
}
func (store *Store) Close()              { store.pool.Close() }
func (store *Store) Pool() *pgxpool.Pool { return store.pool }
func (store *Store) Exec(ctx context.Context, statement string) error {
	_, err := store.pool.Exec(ctx, statement)
	return err
}

const applicationColumns = `application_id, external_ref, beneficiary_id, amount, currency, state, created_at, updated_at, version`

func scanApplication(row pgx.Row) (Application, error) {
	var retained Application
	err := row.Scan(&retained.ApplicationID, &retained.ExternalRef, &retained.BeneficiaryID, &retained.Amount,
		&retained.Currency, &retained.State, &retained.CreatedAt, &retained.UpdatedAt, &retained.Version)
	return retained, err
}

func scanApproval(row pgx.Row) (Approval, error) {
	var retained Approval
	err := row.Scan(&retained.ApprovalID, &retained.ApplicationID, &retained.Role, &retained.PrincipalID,
		&retained.Decision, &retained.FromState, &retained.ToState, &retained.CreatedAt)
	return retained, err
}

// Submit persists a new application in SUBMITTED state with its role
// assignments and an outbox event in one transaction. The database unique
// constraint on (application_id, principal_id) enforces role separation.
func (store *Store) Submit(ctx context.Context, application Application, assignments map[Role]string) (Application, error) {
	if err := application.Validate(); err != nil {
		return Application{}, err
	}
	if err := ValidateRoleAssignments(assignments); err != nil {
		return Application{}, err
	}
	application.State = StateSubmitted
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Application{}, fmt.Errorf("begin submit: %w", err)
	}
	defer tx.Rollback(ctx)
	createdAt := time.Now().UTC()
	retained, err := scanApplication(tx.QueryRow(ctx, `
		INSERT INTO cvff_applications (application_id, external_ref, beneficiary_id, amount, currency, state, created_at, updated_at, version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7,1)
		RETURNING `+applicationColumns,
		application.ApplicationID, application.ExternalRef, application.BeneficiaryID, application.Amount, application.Currency, StateSubmitted, createdAt))
	if err != nil {
		return Application{}, fmt.Errorf("insert cvff application: %w", err)
	}
	for _, role := range []Role{RoleUnderwriterPrimary, RoleUnderwriterSecondary, RoleUnderwriterTertiary, RoleNIMASAApprover, RoleReceivingBank, RoleBeneficiary} {
		if _, err := tx.Exec(ctx, `
			INSERT INTO cvff_role_assignments (application_id, role, principal_id, created_at) VALUES ($1,$2,$3,$4)`,
			retained.ApplicationID, role, assignments[role], createdAt); err != nil {
			return Application{}, fmt.Errorf("assign cvff role %s: %w", role, err)
		}
	}
	if err := appendEvent(ctx, tx, retained.ApplicationID, "cvff.application.submitted", retained, createdAt); err != nil {
		return Application{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Application{}, fmt.Errorf("commit submit: %w", err)
	}
	return retained, nil
}

func (store *Store) Get(ctx context.Context, applicationID string) (Application, error) {
	retained, err := scanApplication(store.pool.QueryRow(ctx, `SELECT `+applicationColumns+` FROM cvff_applications WHERE application_id = $1`, applicationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, ErrNotFound
	}
	if err != nil {
		return Application{}, fmt.Errorf("get cvff application: %w", err)
	}
	return retained, nil
}

// RoleAssignments returns the durable role holders for an application.
func (store *Store) RoleAssignments(ctx context.Context, applicationID string) (map[Role]string, error) {
	rows, err := store.pool.Query(ctx, `SELECT role, principal_id FROM cvff_role_assignments WHERE application_id = $1`, applicationID)
	if err != nil {
		return nil, fmt.Errorf("list cvff role assignments: %w", err)
	}
	defer rows.Close()
	assignments := make(map[Role]string)
	for rows.Next() {
		var role Role
		var principal string
		if err := rows.Scan(&role, &principal); err != nil {
			return nil, fmt.Errorf("scan cvff role assignment: %w", err)
		}
		assignments[role] = principal
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cvff role assignments: %w", err)
	}
	if len(assignments) == 0 {
		return nil, ErrNotFound
	}
	return assignments, nil
}

// RecordDecision validates one party's decision against the state machine and
// role assignments, persists the immutable approval entry, advances state and
// writes an outbox event in one transaction.
func (store *Store) RecordDecision(ctx context.Context, applicationID string, expectedVersion int64, principalID string, decision Decision) (Application, Approval, error) {
	current, err := store.Get(ctx, applicationID)
	if err != nil {
		return Application{}, Approval{}, err
	}
	if current.Version != expectedVersion {
		return Application{}, Approval{}, ErrConflict
	}
	assignments, err := store.RoleAssignments(ctx, applicationID)
	if err != nil {
		return Application{}, Approval{}, err
	}
	updated, approval, err := ApplyDecision(current, assignments, principalID, decision)
	if err != nil {
		return Application{}, Approval{}, err
	}
	return store.commitDecision(ctx, current, updated, approval, expectedVersion)
}

func (store *Store) commitDecision(ctx context.Context, current, updated Application, approval Approval, expectedVersion int64) (Application, Approval, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Application{}, Approval{}, fmt.Errorf("begin decision: %w", err)
	}
	defer tx.Rollback(ctx)
	approval.ApprovalID = uuid.NewString()
	approval.CreatedAt = time.Now().UTC()
	persisted, err := scanApproval(tx.QueryRow(ctx, `
		INSERT INTO cvff_approvals (approval_id, application_id, role, principal_id, decision, from_state, to_state, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING approval_id, application_id, role, principal_id, decision, from_state, to_state, created_at`,
		approval.ApprovalID, approval.ApplicationID, approval.Role, approval.PrincipalID, approval.Decision, approval.FromState, approval.ToState, approval.CreatedAt))
	if err != nil {
		return Application{}, Approval{}, fmt.Errorf("insert cvff approval: %w", err)
	}
	retained, err := scanApplication(tx.QueryRow(ctx, `
		UPDATE cvff_applications SET state = $1, updated_at = $2, version = version + 1
		WHERE application_id = $3 AND state = $4 AND version = $5
		RETURNING `+applicationColumns,
		updated.State, approval.CreatedAt, current.ApplicationID, current.State, expectedVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, Approval{}, ErrConflict
	}
	if err != nil {
		return Application{}, Approval{}, fmt.Errorf("advance cvff application: %w", err)
	}
	if err := appendEvent(ctx, tx, retained.ApplicationID, "cvff.decision.recorded", persisted, approval.CreatedAt); err != nil {
		return Application{}, Approval{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Application{}, Approval{}, fmt.Errorf("commit decision: %w", err)
	}
	return retained, persisted, nil
}

// Transition applies a non-decision lifecycle move (begin underwriting, audit
// close, reconciliation branch) guarded by the state machine and version check.
func (store *Store) Transition(ctx context.Context, applicationID string, expectedVersion int64, move func(Application) (Application, error), eventType string) (Application, error) {
	current, err := store.Get(ctx, applicationID)
	if err != nil {
		return Application{}, err
	}
	if current.Version != expectedVersion {
		return Application{}, ErrConflict
	}
	updated, err := move(current)
	if err != nil {
		return Application{}, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Application{}, fmt.Errorf("begin transition: %w", err)
	}
	defer tx.Rollback(ctx)
	updatedAt := time.Now().UTC()
	retained, err := scanApplication(tx.QueryRow(ctx, `
		UPDATE cvff_applications SET state = $1, updated_at = $2, version = version + 1
		WHERE application_id = $3 AND state = $4 AND version = $5
		RETURNING `+applicationColumns,
		updated.State, updatedAt, current.ApplicationID, current.State, expectedVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, ErrConflict
	}
	if err != nil {
		return Application{}, fmt.Errorf("transition cvff application: %w", err)
	}
	if err := appendEvent(ctx, tx, retained.ApplicationID, eventType, retained, updatedAt); err != nil {
		return Application{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Application{}, fmt.Errorf("commit transition: %w", err)
	}
	return retained, nil
}

// RecordEscalation appends an SLA-expiry audit event without advancing state.
// It is fail-closed: escalation never approves or rejects on behalf of a party.
func (store *Store) RecordEscalation(ctx context.Context, applicationID string, tier UnderwritingTier, deadline time.Time) error {
	if _, err := SLABusinessDays(tier); err != nil {
		return err
	}
	if _, err := store.Get(ctx, applicationID); err != nil {
		return err
	}
	payload := map[string]any{
		"application_id": applicationID,
		"tier":           tier,
		"deadline":       deadline.UTC(),
		"reason":         "underwriting SLA expired",
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin escalation: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := appendEvent(ctx, tx, applicationID, "cvff.sla_escalated", payload, time.Now().UTC()); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit escalation: %w", err)
	}
	return nil
}

// ListApprovals returns the immutable decision trail in recording order.
func (store *Store) ListApprovals(ctx context.Context, applicationID string) ([]Approval, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT approval_id, application_id, role, principal_id, decision, from_state, to_state, created_at
		FROM cvff_approvals WHERE application_id = $1 ORDER BY created_at, approval_id`, applicationID)
	if err != nil {
		return nil, fmt.Errorf("list cvff approvals: %w", err)
	}
	defer rows.Close()
	approvals := make([]Approval, 0)
	for rows.Next() {
		retained, scanErr := scanApproval(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan cvff approval: %w", scanErr)
		}
		approvals = append(approvals, retained)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cvff approvals: %w", err)
	}
	return approvals, nil
}

func appendEvent(ctx context.Context, tx pgx.Tx, applicationID string, eventType string, value any, createdAt time.Time) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode cvff event: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cvff_outbox (event_id, application_id, event_type, payload, created_at) VALUES ($1,$2,$3,$4,$5)`, uuid.New(), applicationID, eventType, payload, createdAt); err != nil {
		return fmt.Errorf("write cvff event: %w", err)
	}
	return nil
}
