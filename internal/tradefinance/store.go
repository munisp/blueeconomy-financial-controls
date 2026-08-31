package tradefinance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

// Store persists consents, applications and their audit evidence. Every
// write path runs in one transaction with its outbox event; approval and
// consent-audit tables are immutable in the database.
type Store struct {
	pool   *pgxpool.Pool
	signer *envelope.Signer
}

// NewStore binds the pool and the consent envelope signer. The signer is
// mandatory: an activated consent without its envelope signature is never
// persistable (fail-closed).
func NewStore(pool *pgxpool.Pool, signer *envelope.Signer) (*Store, error) {
	if pool == nil {
		return nil, errors.New("postgres pool is required")
	}
	if signer == nil {
		return nil, errors.New("consent envelope signer is required")
	}
	return &Store{pool: pool, signer: signer}, nil
}

// Open connects and pings, failing closed on any gap.
func Open(ctx context.Context, databaseURL string, signer *envelope.Signer) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres DSN: %w", err)
	}
	if err := telemetry.ApplyPoolEnv(config); err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return NewStore(pool, signer)
}

func (store *Store) Close()              { store.pool.Close() }
func (store *Store) Pool() *pgxpool.Pool { return store.pool }

// Exec executes a raw statement; it exists for migration application in tests.
func (store *Store) Exec(ctx context.Context, statement string) error {
	_, err := store.pool.Exec(ctx, statement)
	return err
}

const consentColumns = `consent_id, trader_id, bank_id, scopes, dataset_refs, state, expires_at, maker_principal, checker_principal, envelope_jws, envelope_bundle, created_at, updated_at, version`

func scanConsent(row pgx.Row) (Consent, error) {
	var retained Consent
	var checker, jws *string
	var bundle []byte
	var refs []byte
	err := row.Scan(&retained.ConsentID, &retained.TraderID, &retained.BankID, &retained.Scopes, &refs,
		&retained.State, &retained.ExpiresAt, &retained.MakerPrincipal, &checker, &jws, &bundle,
		&retained.CreatedAt, &retained.UpdatedAt, &retained.Version)
	if err != nil {
		return Consent{}, err
	}
	if checker != nil {
		retained.CheckerPrincipal = *checker
	}
	if jws != nil {
		retained.EnvelopeJWS = *jws
	}
	retained.EnvelopeBundle = bundle
	if err := json.Unmarshal(refs, &retained.DatasetRefs); err != nil {
		return Consent{}, fmt.Errorf("decode consent dataset refs: %w", err)
	}
	return retained, nil
}

// RequestConsent persists a new PENDING_CHECKER consent with its genesis
// audit entry and outbox event in one transaction.
func (store *Store) RequestConsent(ctx context.Context, consent Consent, now time.Time) (Consent, error) {
	validated, err := NewConsent(consent.ConsentID, consent.TraderID, consent.BankID, consent.Scopes, consent.DatasetRefs, consent.ExpiresAt, consent.MakerPrincipal, now)
	if err != nil {
		return Consent{}, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Consent{}, fmt.Errorf("begin consent request: %w", err)
	}
	defer tx.Rollback(ctx)
	createdAt := now.UTC()
	refs, err := json.Marshal(validated.DatasetRefs)
	if err != nil {
		return Consent{}, fmt.Errorf("encode dataset refs: %w", err)
	}
	retained, err := scanConsent(tx.QueryRow(ctx, `
		INSERT INTO tf_consents (consent_id, trader_id, bank_id, scopes, dataset_refs, state, expires_at, maker_principal, created_at, updated_at, version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,1)
		RETURNING `+consentColumns,
		validated.ConsentID, validated.TraderID, validated.BankID, validated.Scopes, refs,
		ConsentPendingChecker, validated.ExpiresAt, validated.MakerPrincipal, createdAt))
	if err != nil {
		return Consent{}, fmt.Errorf("%w: insert consent: %v", ErrConsentConflict, err)
	}
	if err := appendAudit(ctx, tx, retained.ConsentID, retained.MakerPrincipal, AuditRequested, "", ConsentPendingChecker, createdAt); err != nil {
		return Consent{}, err
	}
	if err := appendEvent(ctx, tx, retained.ConsentID, "tradefinance.consent.requested", retained, createdAt); err != nil {
		return Consent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Consent{}, fmt.Errorf("commit consent request: %w", err)
	}
	return retained, nil
}

// GetConsent returns one consent by id.
func (store *Store) GetConsent(ctx context.Context, consentID string) (Consent, error) {
	retained, err := scanConsent(store.pool.QueryRow(ctx, `SELECT `+consentColumns+` FROM tf_consents WHERE consent_id = $1`, consentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Consent{}, ErrConsentNotFound
	}
	if err != nil {
		return Consent{}, fmt.Errorf("get consent: %w", err)
	}
	return retained, nil
}

// ListConsents returns the consents of one trader, newest first.
func (store *Store) ListConsents(ctx context.Context, traderID string) ([]Consent, error) {
	if err := ValidateIdentifier("trader_id", traderID); err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `SELECT `+consentColumns+` FROM tf_consents WHERE trader_id = $1 ORDER BY created_at DESC, consent_id`, traderID)
	if err != nil {
		return nil, fmt.Errorf("list consents: %w", err)
	}
	defer rows.Close()
	consents := make([]Consent, 0)
	for rows.Next() {
		retained, scanErr := scanConsent(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan consent: %w", scanErr)
		}
		consents = append(consents, retained)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate consents: %w", err)
	}
	return consents, nil
}

// consentArtifactPayload is the JCS-safe envelope payload of one activated consent.
func consentArtifactPayload(consent Consent) map[string]any {
	scopes := make([]any, 0, len(consent.Scopes))
	for _, scope := range consent.Scopes {
		scopes = append(scopes, string(scope))
	}
	refs := map[string]any{}
	for scope, digests := range consent.DatasetRefs {
		list := make([]any, 0, len(digests))
		for _, digest := range digests {
			list = append(list, digest)
		}
		refs[scope] = list
	}
	return map[string]any{
		"consent_id":        consent.ConsentID,
		"trader_id":         consent.TraderID,
		"bank_id":           consent.BankID,
		"scopes":            scopes,
		"dataset_refs":      refs,
		"expires_at":        consent.ExpiresAt.UTC().Format(time.RFC3339),
		"maker_principal":   consent.MakerPrincipal,
		"checker_principal": consent.CheckerPrincipal,
	}
}

// transitionConsent applies one maker-checker move with optimistic
// concurrency, envelope-signing on activation, audit chaining and outbox in
// one transaction.
func (store *Store) transitionConsent(ctx context.Context, consentID string, expectedVersion int64, actor string, action AuditAction, eventType string, move func(Consent) (Consent, error)) (Consent, error) {
	current, err := store.GetConsent(ctx, consentID)
	if err != nil {
		return Consent{}, err
	}
	if current.Version != expectedVersion {
		return Consent{}, ErrConsentConflict
	}
	updated, err := move(current)
	if err != nil {
		return Consent{}, err
	}
	updatedAt := time.Now().UTC()
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Consent{}, fmt.Errorf("begin consent transition: %w", err)
	}
	defer tx.Rollback(ctx)
	retained, err := scanConsent(tx.QueryRow(ctx, `
		UPDATE tf_consents SET state = $1, checker_principal = NULLIF($2,''), envelope_jws = NULLIF($3,''), envelope_bundle = $4, updated_at = $5, version = version + 1
		WHERE consent_id = $6 AND state = $7 AND version = $8
		RETURNING `+consentColumns,
		updated.State, updated.CheckerPrincipal, updated.EnvelopeJWS, updated.EnvelopeBundle, updatedAt,
		current.ConsentID, current.State, expectedVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return Consent{}, ErrConsentConflict
	}
	if err != nil {
		return Consent{}, fmt.Errorf("transition consent: %w", err)
	}
	if err := appendAudit(ctx, tx, retained.ConsentID, actor, action, current.State, retained.State, updatedAt); err != nil {
		return Consent{}, err
	}
	if err := appendEvent(ctx, tx, retained.ConsentID, eventType, retained, updatedAt); err != nil {
		return Consent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Consent{}, fmt.Errorf("commit consent transition: %w", err)
	}
	return retained, nil
}

// ActivateConsent applies the checker's approval, sealing the consent into
// its envelope v1.0 artifact before persistence.
func (store *Store) ActivateConsent(ctx context.Context, consentID string, expectedVersion int64, checkerPrincipal string) (Consent, error) {
	return store.transitionConsent(ctx, consentID, expectedVersion, checkerPrincipal, AuditActivated, "tradefinance.consent.activated",
		func(current Consent) (Consent, error) {
			candidate := current
			candidate.CheckerPrincipal = checkerPrincipal
			bundle, jws, err := store.signer.Sign("tradefinance.consent", current.ConsentID, consentArtifactPayload(candidate), time.Now().UTC())
			if err != nil {
				return Consent{}, fmt.Errorf("seal consent envelope: %w", err)
			}
			return Activate(current, checkerPrincipal, jws, bundle)
		})
}

// RejectConsent applies the checker's decline of a pending grant.
func (store *Store) RejectConsent(ctx context.Context, consentID string, expectedVersion int64, checkerPrincipal string) (Consent, error) {
	return store.transitionConsent(ctx, consentID, expectedVersion, checkerPrincipal, AuditRejected, "tradefinance.consent.rejected",
		func(current Consent) (Consent, error) { return RejectGrant(current, checkerPrincipal) })
}

// RequestRevocation suspends sharing immediately pending checker confirmation.
func (store *Store) RequestRevocation(ctx context.Context, consentID string, expectedVersion int64, makerPrincipal string) (Consent, error) {
	return store.transitionConsent(ctx, consentID, expectedVersion, makerPrincipal, AuditRevocationRequested, "tradefinance.consent.revocation_requested",
		func(current Consent) (Consent, error) { return RequestRevocation(current, makerPrincipal) })
}

// ConfirmRevocation finalizes a revocation.
func (store *Store) ConfirmRevocation(ctx context.Context, consentID string, expectedVersion int64, checkerPrincipal string) (Consent, error) {
	return store.transitionConsent(ctx, consentID, expectedVersion, checkerPrincipal, AuditRevoked, "tradefinance.consent.revoked",
		func(current Consent) (Consent, error) { return ConfirmRevocation(current, checkerPrincipal) })
}

// RejectRevocation returns a revocation-pending consent to ACTIVE.
func (store *Store) RejectRevocation(ctx context.Context, consentID string, expectedVersion int64, checkerPrincipal string) (Consent, error) {
	return store.transitionConsent(ctx, consentID, expectedVersion, checkerPrincipal, AuditRevocationRejected, "tradefinance.consent.activated",
		func(current Consent) (Consent, error) { return RejectRevocation(current, checkerPrincipal) })
}

// ConsentAudit returns the hash-chained audit trail of one consent in order.
func (store *Store) ConsentAudit(ctx context.Context, consentID string) ([]AuditEntry, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT audit_id, consent_id, seq, actor_principal, action, from_state, to_state, entry_hash, prev_hash, created_at
		FROM tf_consent_audit WHERE consent_id = $1 ORDER BY seq`, consentID)
	if err != nil {
		return nil, fmt.Errorf("list consent audit: %w", err)
	}
	defer rows.Close()
	entries := make([]AuditEntry, 0)
	for rows.Next() {
		var entry AuditEntry
		if err := rows.Scan(&entry.AuditID, &entry.ConsentID, &entry.Seq, &entry.ActorPrincipal, &entry.Action,
			&entry.FromState, &entry.ToState, &entry.EntryHash, &entry.PrevHash, &entry.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan consent audit: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate consent audit: %w", err)
	}
	return entries, nil
}

// appendAudit writes the next hash-chained audit entry inside the caller's
// transaction, locking the consent row so the chain cannot fork.
func appendAudit(ctx context.Context, tx pgx.Tx, consentID, actor string, action AuditAction, from, to ConsentState, now time.Time) error {
	var prevHash string
	var seq int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE((SELECT entry_hash FROM tf_consent_audit WHERE consent_id = $1 ORDER BY seq DESC LIMIT 1), ''),
		        COALESCE((SELECT max(seq) FROM tf_consent_audit WHERE consent_id = $1), 0)
		FROM tf_consents WHERE consent_id = $1 FOR UPDATE`, consentID).Scan(&prevHash, &seq)
	if err != nil {
		return fmt.Errorf("lock consent audit chain: %w", err)
	}
	if prevHash == "" {
		prevHash = GenesisPrevHash
	}
	entry := NewAuditEntry(uuid.NewString(), consentID, seq+1, actor, action, from, to, prevHash, now)
	if _, err := tx.Exec(ctx, `
		INSERT INTO tf_consent_audit (audit_id, consent_id, seq, actor_principal, action, from_state, to_state, entry_hash, prev_hash, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		entry.AuditID, entry.ConsentID, entry.Seq, entry.ActorPrincipal, entry.Action, entry.FromState, entry.ToState, entry.EntryHash, entry.PrevHash, entry.CreatedAt); err != nil {
		return fmt.Errorf("write consent audit: %w", err)
	}
	return nil
}

// activeConsent returns the shareable consent covering (trader, bank) at
// now, failing closed on absence, expiry or non-ACTIVE state.
func (store *Store) activeConsent(ctx context.Context, traderID, bankID string, now time.Time) (Consent, error) {
	rows, err := store.pool.Query(ctx, `SELECT `+consentColumns+`
		FROM tf_consents WHERE trader_id = $1 AND bank_id = $2 AND state = 'ACTIVE' ORDER BY created_at DESC`, traderID, bankID)
	if err != nil {
		return Consent{}, fmt.Errorf("lookup active consent: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		consent, scanErr := scanConsent(rows)
		if scanErr != nil {
			return Consent{}, fmt.Errorf("scan active consent: %w", scanErr)
		}
		if !consent.Shareable(now) {
			return Consent{}, ErrConsentExpired
		}
		return consent, nil
	}
	if err := rows.Err(); err != nil {
		return Consent{}, fmt.Errorf("iterate active consents: %w", err)
	}
	return Consent{}, ErrScopeNotConsented
}

// ConsentedDataset is the fail-closed bank-facing read: it returns ONLY the
// digest references of the requested scope when an active, unexpired consent
// covers (trader, bank, scope). Everything else fails closed.
func (store *Store) ConsentedDataset(ctx context.Context, bankID, traderID string, scope Scope, now time.Time) (Consent, []string, error) {
	if err := ValidateIdentifier("bank_id", bankID); err != nil {
		return Consent{}, nil, err
	}
	if err := ValidateIdentifier("trader_id", traderID); err != nil {
		return Consent{}, nil, err
	}
	if !validScope(scope) {
		return Consent{}, nil, ErrConsentScopeInvalid
	}
	consent, err := store.activeConsent(ctx, traderID, bankID, now)
	if err != nil {
		return Consent{}, nil, err
	}
	if !consent.Covers(scope) {
		return Consent{}, nil, ErrScopeNotConsented
	}
	refs := append([]string(nil), consent.DatasetRefs[string(scope)]...)
	if len(refs) == 0 {
		return Consent{}, nil, ErrScopeNotConsented
	}
	return consent, refs, nil
}

// ─── Application workflow ───────────────────────────────────────────────────

const applicationColumns = `application_id, external_ref, trader_id, bank_id, consent_id, product, amount, currency, state, facility_account_id, settlement_account_id, created_at, updated_at, version`

func scanApplication(row pgx.Row) (Application, error) {
	var retained Application
	var facility, settlement *string
	err := row.Scan(&retained.ApplicationID, &retained.ExternalRef, &retained.TraderID, &retained.BankID, &retained.ConsentID,
		&retained.Product, &retained.Amount, &retained.Currency, &retained.State, &facility, &settlement,
		&retained.CreatedAt, &retained.UpdatedAt, &retained.Version)
	if err != nil {
		return Application{}, err
	}
	if facility != nil {
		retained.FacilityAccountID = *facility
	}
	if settlement != nil {
		retained.SettlementAccountID = *settlement
	}
	return retained, nil
}

// SubmitApplication persists a new application in APPLICATION state with its
// role assignments and outbox event in one transaction. The referenced
// consent must exist; sharing enforcement happens at KYC_CONSENT_CHECK.
func (store *Store) SubmitApplication(ctx context.Context, application Application, assignments map[Role]string) (Application, error) {
	if err := application.Validate(); err != nil {
		return Application{}, err
	}
	if err := ValidateRoleAssignments(assignments); err != nil {
		return Application{}, err
	}
	if _, err := store.GetConsent(ctx, application.ConsentID); err != nil {
		return Application{}, fmt.Errorf("application consent: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Application{}, fmt.Errorf("begin application submit: %w", err)
	}
	defer tx.Rollback(ctx)
	createdAt := time.Now().UTC()
	retained, err := scanApplication(tx.QueryRow(ctx, `
		INSERT INTO tf_applications (application_id, external_ref, trader_id, bank_id, consent_id, product, amount, currency, state, created_at, updated_at, version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10,1)
		RETURNING `+applicationColumns,
		application.ApplicationID, application.ExternalRef, application.TraderID, application.BankID, application.ConsentID,
		application.Product, application.Amount, application.Currency, StateApplication, createdAt))
	if err != nil {
		return Application{}, fmt.Errorf("insert tradefinance application: %w", err)
	}
	for _, role := range chainRoles {
		if _, err := tx.Exec(ctx, `
			INSERT INTO tf_role_assignments (application_id, role, principal_id, created_at) VALUES ($1,$2,$3,$4)`,
			retained.ApplicationID, role, assignments[role], createdAt); err != nil {
			return Application{}, fmt.Errorf("assign tradefinance role %s: %w", role, err)
		}
	}
	if err := appendTransition(ctx, tx, retained.ApplicationID, StateApplication, StateApplication, "TRADER", application.TraderID, "tradefinance.application.submitted", createdAt); err != nil {
		return Application{}, err
	}
	if err := appendEvent(ctx, tx, retained.ApplicationID, "tradefinance.application.submitted", retained, createdAt); err != nil {
		return Application{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Application{}, fmt.Errorf("commit application submit: %w", err)
	}
	return retained, nil
}

// GetApplication returns one application by id.
func (store *Store) GetApplication(ctx context.Context, applicationID string) (Application, error) {
	retained, err := scanApplication(store.pool.QueryRow(ctx, `SELECT `+applicationColumns+` FROM tf_applications WHERE application_id = $1`, applicationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, ErrNotFound
	}
	if err != nil {
		return Application{}, fmt.Errorf("get tradefinance application: %w", err)
	}
	return retained, nil
}

// ListApplications returns the applications of one trader, newest first.
func (store *Store) ListApplications(ctx context.Context, traderID string) ([]Application, error) {
	if err := ValidateIdentifier("trader_id", traderID); err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `SELECT `+applicationColumns+` FROM tf_applications WHERE trader_id = $1 ORDER BY created_at DESC, application_id`, traderID)
	if err != nil {
		return nil, fmt.Errorf("list tradefinance applications: %w", err)
	}
	defer rows.Close()
	applications := make([]Application, 0)
	for rows.Next() {
		retained, scanErr := scanApplication(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan tradefinance application: %w", scanErr)
		}
		applications = append(applications, retained)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tradefinance applications: %w", err)
	}
	return applications, nil
}

// RoleAssignments returns the durable role holders for an application.
func (store *Store) RoleAssignments(ctx context.Context, applicationID string) (map[Role]string, error) {
	rows, err := store.pool.Query(ctx, `SELECT role, principal_id FROM tf_role_assignments WHERE application_id = $1`, applicationID)
	if err != nil {
		return nil, fmt.Errorf("list tradefinance role assignments: %w", err)
	}
	defer rows.Close()
	assignments := make(map[Role]string)
	for rows.Next() {
		var role Role
		var principal string
		if err := rows.Scan(&role, &principal); err != nil {
			return nil, fmt.Errorf("scan tradefinance role assignment: %w", err)
		}
		assignments[role] = principal
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tradefinance role assignments: %w", err)
	}
	if len(assignments) == 0 {
		return nil, ErrNotFound
	}
	return assignments, nil
}

// RecordDecision validates one party's decision against the state machine
// and role assignments, persists the immutable approval entry, advances
// state and writes an outbox event in one transaction. The KYC_CONSENT_CHECK
// approval is additionally gated on an active consent covering the
// application trader and bank (NTP TFC pattern: no consent, no financing).
func (store *Store) RecordDecision(ctx context.Context, applicationID string, expectedVersion int64, principalID string, decision Decision) (Application, Approval, error) {
	current, err := store.GetApplication(ctx, applicationID)
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
	if current.State == StateKYCConsentCheck && decision == DecisionApprove {
		if _, err := store.activeConsent(ctx, current.TraderID, current.BankID, time.Now().UTC()); err != nil {
			return Application{}, Approval{}, fmt.Errorf("%w: %v", ErrConsentRequired, err)
		}
	}
	if current.State == StateBankReview && decision == DecisionApprove {
		if current.FacilityAccountID == "" || current.SettlementAccountID == "" {
			return Application{}, Approval{}, ErrLedgerAccountsMissing
		}
	}
	committed, committedApproval, err := store.commitDecision(ctx, current, updated, approval, expectedVersion)
	if err != nil {
		return Application{}, Approval{}, err
	}
	return committed, committedApproval, nil
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
		INSERT INTO tf_approvals (approval_id, application_id, role, principal_id, decision, from_state, to_state, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING approval_id, application_id, role, principal_id, decision, from_state, to_state, created_at`,
		approval.ApprovalID, approval.ApplicationID, approval.Role, approval.PrincipalID, approval.Decision, approval.FromState, approval.ToState, approval.CreatedAt))
	if err != nil {
		return Application{}, Approval{}, fmt.Errorf("insert tradefinance approval: %w", err)
	}
	retained, err := scanApplication(tx.QueryRow(ctx, `
		UPDATE tf_applications SET state = $1, updated_at = $2, version = version + 1
		WHERE application_id = $3 AND state = $4 AND version = $5
		RETURNING `+applicationColumns,
		updated.State, approval.CreatedAt, current.ApplicationID, current.State, expectedVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, Approval{}, ErrConflict
	}
	if err != nil {
		return Application{}, Approval{}, fmt.Errorf("advance tradefinance application: %w", err)
	}
	if err := appendTransition(ctx, tx, persisted.ApplicationID, persisted.FromState, persisted.ToState, string(persisted.Role), persisted.PrincipalID, "tradefinance.application.decision_recorded", persisted.CreatedAt); err != nil {
		return Application{}, Approval{}, err
	}
	eventType := "tradefinance.application.decision_recorded"
	if retained.State == StateDisbursed {
		eventType = "tradefinance.application.disbursed"
	}
	if retained.State == StateSettled {
		eventType = "tradefinance.application.settled"
	}
	if err := appendEvent(ctx, tx, retained.ApplicationID, eventType, persisted, approval.CreatedAt); err != nil {
		return Application{}, Approval{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Application{}, Approval{}, fmt.Errorf("commit decision: %w", err)
	}
	return retained, persisted, nil
}

func scanApproval(row pgx.Row) (Approval, error) {
	var retained Approval
	err := row.Scan(&retained.ApprovalID, &retained.ApplicationID, &retained.Role, &retained.PrincipalID,
		&retained.Decision, &retained.FromState, &retained.ToState, &retained.CreatedAt)
	return retained, err
}

// Transition applies a non-decision lifecycle move (begin review) guarded by
// the state machine and version check.
func (store *Store) Transition(ctx context.Context, applicationID string, expectedVersion int64, actorRole, actorPrincipal string, move func(Application) (Application, error), eventType string) (Application, error) {
	current, err := store.GetApplication(ctx, applicationID)
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
		UPDATE tf_applications SET state = $1, updated_at = $2, version = version + 1
		WHERE application_id = $3 AND state = $4 AND version = $5
		RETURNING `+applicationColumns,
		updated.State, updatedAt, current.ApplicationID, current.State, expectedVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, ErrConflict
	}
	if err != nil {
		return Application{}, fmt.Errorf("transition tradefinance application: %w", err)
	}
	if err := appendTransition(ctx, tx, retained.ApplicationID, current.State, retained.State, actorRole, actorPrincipal, eventType, updatedAt); err != nil {
		return Application{}, err
	}
	if err := appendEvent(ctx, tx, retained.ApplicationID, eventType, retained, updatedAt); err != nil {
		return Application{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Application{}, fmt.Errorf("commit transition: %w", err)
	}
	return retained, nil
}

// BindLedgerAccounts records the TigerBeetle facility/settlement accounts on
// an in-flight application. Replay with identical accounts is idempotent;
// divergence is a hard conflict because the accounts are disbursement
// authority evidence.
func (store *Store) BindLedgerAccounts(ctx context.Context, applicationID string, expectedVersion int64, facilityAccountID, settlementAccountID string) (Application, error) {
	if err := ValidateIdentifier("facility_account_id", facilityAccountID); err != nil {
		return Application{}, err
	}
	if err := ValidateIdentifier("settlement_account_id", settlementAccountID); err != nil {
		return Application{}, err
	}
	if facilityAccountID == settlementAccountID {
		return Application{}, errors.New("facility and settlement accounts must differ")
	}
	current, err := store.GetApplication(ctx, applicationID)
	if err != nil {
		return Application{}, err
	}
	if current.Version != expectedVersion {
		return Application{}, ErrConflict
	}
	if current.State.Terminal() {
		return Application{}, ErrTerminalState
	}
	if current.FacilityAccountID != "" || current.SettlementAccountID != "" {
		if current.FacilityAccountID == facilityAccountID && current.SettlementAccountID == settlementAccountID {
			return current, nil
		}
		return Application{}, ErrConflict
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Application{}, fmt.Errorf("begin ledger account binding: %w", err)
	}
	defer tx.Rollback(ctx)
	updatedAt := time.Now().UTC()
	retained, err := scanApplication(tx.QueryRow(ctx, `
		UPDATE tf_applications SET facility_account_id = $1, settlement_account_id = $2, updated_at = $3, version = version + 1
		WHERE application_id = $4 AND version = $5 AND facility_account_id IS NULL
		RETURNING `+applicationColumns,
		facilityAccountID, settlementAccountID, updatedAt, applicationID, expectedVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, ErrConflict
	}
	if err != nil {
		return Application{}, fmt.Errorf("bind ledger accounts: %w", err)
	}
	if err := appendTransition(ctx, tx, retained.ApplicationID, current.State, retained.State, "BANK_TREASURY_OFFICER", "ledger-account-binding", "tradefinance.ledger_accounts_bound", updatedAt); err != nil {
		return Application{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Application{}, fmt.Errorf("commit ledger account binding: %w", err)
	}
	return retained, nil
}

// ListApprovals returns the immutable decision trail in recording order.
func (store *Store) ListApprovals(ctx context.Context, applicationID string) ([]Approval, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT approval_id, application_id, role, principal_id, decision, from_state, to_state, created_at
		FROM tf_approvals WHERE application_id = $1 ORDER BY created_at, approval_id`, applicationID)
	if err != nil {
		return nil, fmt.Errorf("list tradefinance approvals: %w", err)
	}
	defer rows.Close()
	approvals := make([]Approval, 0)
	for rows.Next() {
		retained, scanErr := scanApproval(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan tradefinance approval: %w", scanErr)
		}
		approvals = append(approvals, retained)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tradefinance approvals: %w", err)
	}
	return approvals, nil
}

// RecordDisbursementLegs persists the TigerBeetle transfer evidence of the
// disbursement lifecycle (reserve at APPROVED, post at DISBURSED, settlement
// post at SETTLED). Leg rows are insert-once; settlement columns update only
// forward.
func (store *Store) RecordDisbursementLegs(ctx context.Context, applicationID, reserveTransferID string, amount uint64, currency string) error {
	if err := ValidateIdentifier("application_id", applicationID); err != nil {
		return err
	}
	if err := ValidateIdentifier("reserve_transfer_id", reserveTransferID); err != nil {
		return err
	}
	if amount == 0 {
		return errors.New("disbursement amount must be non-zero")
	}
	if err := ValidateCurrency(currency); err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO tf_disbursement_legs (application_id, reserve_transfer_id, amount, currency, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$5)`,
		applicationID, reserveTransferID, amount, currency, now); err != nil {
		return fmt.Errorf("record disbursement legs: %w", err)
	}
	return nil
}

// CompleteDisbursementLeg stamps the disbursement post transfer id.
func (store *Store) CompleteDisbursementLeg(ctx context.Context, applicationID, disburseTransferID string) error {
	return store.stampLeg(ctx, applicationID, "disburse_transfer_id", disburseTransferID)
}

// CompleteSettlementLeg stamps the settlement post transfer id.
func (store *Store) CompleteSettlementLeg(ctx context.Context, applicationID, settleTransferID string) error {
	return store.stampLeg(ctx, applicationID, "settle_transfer_id", settleTransferID)
}

func (store *Store) stampLeg(ctx context.Context, applicationID, column, transferID string) error {
	if err := ValidateIdentifier("transfer_id", transferID); err != nil {
		return err
	}
	result, err := store.pool.Exec(ctx, fmt.Sprintf(`
		UPDATE tf_disbursement_legs SET %s = $1, updated_at = $2 WHERE application_id = $3 AND %s IS NULL`, column, column),
		transferID, time.Now().UTC(), applicationID)
	if err != nil {
		return fmt.Errorf("stamp disbursement leg: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func appendTransition(ctx context.Context, tx pgx.Tx, applicationID string, fromState State, toState State, actorRole string, actorPrincipal string, eventType string, createdAt time.Time) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO tf_transitions (transition_id, application_id, from_state, to_state, actor_role, actor_principal_id, event_type, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		uuid.New(), applicationID, fromState, toState, actorRole, actorPrincipal, eventType, createdAt); err != nil {
		return fmt.Errorf("write tradefinance transition: %w", err)
	}
	return nil
}

func appendEvent(ctx context.Context, tx pgx.Tx, subjectID string, eventType string, value any, createdAt time.Time) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode tradefinance event: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tf_outbox (event_id, subject_id, event_type, payload, created_at) VALUES ($1,$2,$3,$4,$5)`, uuid.New(), subjectID, eventType, payload, createdAt); err != nil {
		return fmt.Errorf("write tradefinance event: %w", err)
	}
	return nil
}
