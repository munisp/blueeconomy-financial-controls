package revenue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SplitRuleInput is one new split-rule draft.
type SplitRuleInput struct {
	RevenueLine        string `json:"revenueLine"`
	Agency             string `json:"agency"`
	Beneficiary        string `json:"beneficiary"`
	ShareBps           int64  `json:"shareBps"`
	StatutoryReference string `json:"statutoryReference"`
	Provisional        bool   `json:"provisional,omitempty"`
	EffectiveFrom      string `json:"effectiveFrom"`
	EffectiveTo        string `json:"effectiveTo,omitempty"`
}

// Validate fails closed on a malformed rule draft.
func (input SplitRuleInput) Validate() error {
	if strings.TrimSpace(input.RevenueLine) == "" || len(input.RevenueLine) > 64 {
		return errors.New("revenueLine is required")
	}
	switch input.Agency {
	case AgencyNPA, AgencyNIMASA, AgencyNIWA, AgencyFMMBE:
	default:
		return fmt.Errorf("agency %q is not a charging agency", input.Agency)
	}
	switch input.Beneficiary {
	case BeneficiaryAgencyRetained, BeneficiaryFGN, BeneficiaryCVFFFiduciary:
	default:
		return fmt.Errorf("beneficiary %q is not a TSA beneficiary", input.Beneficiary)
	}
	if input.ShareBps <= 0 || input.ShareBps > 10000 {
		return errors.New("shareBps must be in 1..10000")
	}
	if strings.TrimSpace(input.StatutoryReference) == "" {
		return errors.New("statutoryReference is required")
	}
	if _, err := time.Parse("2006-01-02", input.EffectiveFrom); err != nil {
		return errors.New("effectiveFrom must be YYYY-MM-DD")
	}
	if input.EffectiveTo != "" {
		if _, err := time.Parse("2006-01-02", input.EffectiveTo); err != nil {
			return errors.New("effectiveTo must be YYYY-MM-DD")
		}
	}
	return nil
}

// CreateSplitRule drafts one rule (maker). Activation is dual-controlled.
func (store *Store) CreateSplitRule(ctx context.Context, input SplitRuleInput, maker string) (SplitRuleRow, error) {
	if err := input.Validate(); err != nil {
		return SplitRuleRow{}, err
	}
	if maker == "" {
		return SplitRuleRow{}, errors.New("maker is required")
	}
	rule := SplitRuleRow{
		RuleID:             uuid.NewString(),
		RevenueLine:        input.RevenueLine,
		Agency:             input.Agency,
		Beneficiary:        input.Beneficiary,
		ShareBps:           input.ShareBps,
		StatutoryReference: input.StatutoryReference,
		Provisional:        input.Provisional,
		State:              RuleDraft,
		Maker:              maker,
	}
	rule.EffectiveFrom, _ = time.Parse("2006-01-02", input.EffectiveFrom)
	if input.EffectiveTo != "" {
		parsed, _ := time.Parse("2006-01-02", input.EffectiveTo)
		rule.EffectiveTo = &parsed
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return SplitRuleRow{}, fmt.Errorf("begin split-rule transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO tsa_split_rules (rule_id, revenue_line, agency, beneficiary, share_bps,
		     statutory_reference, provisional, effective_from, effective_to, state, maker)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'DRAFT', $10)`,
		rule.RuleID, rule.RevenueLine, rule.Agency, rule.Beneficiary, rule.ShareBps,
		rule.StatutoryReference, rule.Provisional, rule.EffectiveFrom, rule.EffectiveTo, maker); err != nil {
		return SplitRuleRow{}, fmt.Errorf("insert split rule: %w", err)
	}
	if err := insertOutbox(ctx, tx, rule.RuleID, "revenue.split_rule.created", map[string]any{
		"ruleId": rule.RuleID, "revenueLine": rule.RevenueLine, "agency": rule.Agency,
		"beneficiary": rule.Beneficiary, "shareBps": rule.ShareBps, "maker": maker,
	}); err != nil {
		return SplitRuleRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SplitRuleRow{}, fmt.Errorf("commit split rule: %w", err)
	}
	return rule, nil
}

// ActivateSplitRule activates a draft under dual control: the checker must
// differ from the maker, enforced in SQL (checker <> maker on the guarded
// UPDATE plus the table CHECK).
func (store *Store) ActivateSplitRule(ctx context.Context, ruleID, checker string) (SplitRuleRow, error) {
	if ruleID == "" || checker == "" {
		return SplitRuleRow{}, errors.New("rule id and checker are required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return SplitRuleRow{}, fmt.Errorf("begin activation transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx,
		`UPDATE tsa_split_rules SET state = 'ACTIVE', checker = $2
		 WHERE rule_id = $1 AND state = 'DRAFT' AND maker <> $2`, ruleID, checker)
	if err != nil {
		if isCheckViolation(err) {
			return SplitRuleRow{}, ErrMakerChecker
		}
		return SplitRuleRow{}, fmt.Errorf("activate split rule: %w", err)
	}
	if result.RowsAffected() == 0 {
		var maker string
		probe := store.pool.QueryRow(ctx, `SELECT maker FROM tsa_split_rules WHERE rule_id = $1`, ruleID).Scan(&maker)
		if errors.Is(probe, pgx.ErrNoRows) {
			return SplitRuleRow{}, ErrNotFound
		}
		if probe == nil && maker == checker {
			return SplitRuleRow{}, ErrMakerChecker
		}
		return SplitRuleRow{}, ErrInvalidTransition
	}
	if err := insertOutbox(ctx, tx, ruleID, "revenue.split_rule.activated", map[string]any{
		"ruleId": ruleID, "checker": checker,
	}); err != nil {
		return SplitRuleRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SplitRuleRow{}, fmt.Errorf("commit activation: %w", err)
	}
	return store.GetSplitRule(ctx, ruleID)
}

// GetSplitRule loads one rule.
func (store *Store) GetSplitRule(ctx context.Context, ruleID string) (SplitRuleRow, error) {
	var rule SplitRuleRow
	var checker *string
	err := store.pool.QueryRow(ctx,
		`SELECT rule_id, revenue_line, agency, beneficiary, share_bps, statutory_reference,
		        provisional, effective_from, effective_to, state, maker, checker
		 FROM tsa_split_rules WHERE rule_id = $1`, ruleID).
		Scan(&rule.RuleID, &rule.RevenueLine, &rule.Agency, &rule.Beneficiary, &rule.ShareBps,
			&rule.StatutoryReference, &rule.Provisional, &rule.EffectiveFrom, &rule.EffectiveTo,
			&rule.State, &rule.Maker, &checker)
	if errors.Is(err, pgx.ErrNoRows) {
		return SplitRuleRow{}, ErrNotFound
	}
	if err != nil {
		return SplitRuleRow{}, fmt.Errorf("load split rule: %w", err)
	}
	if checker != nil {
		rule.Checker = *checker
	}
	return rule, nil
}

// activeSplitRules selects the rules in force for a revenue line at asOf:
// exact revenue-line rules win; the '*' catch-all applies only when no
// exact-line rule exists for the window.
func (store *Store) activeSplitRules(ctx context.Context, revenueLine string, asOf time.Time) ([]SplitRuleRow, error) {
	query := `
		SELECT rule_id, revenue_line, agency, beneficiary, share_bps, statutory_reference,
		       provisional, effective_from, effective_to, state, maker, checker
		FROM tsa_split_rules
		WHERE state = 'ACTIVE' AND revenue_line = $1
		  AND effective_from <= $2 AND (effective_to IS NULL OR effective_to >= $2)
		ORDER BY share_bps DESC, beneficiary`
	exact, err := store.querySplitRules(ctx, query, revenueLine, asOf)
	if err != nil {
		return nil, err
	}
	if len(exact) > 0 {
		return exact, nil
	}
	return store.querySplitRules(ctx, query, "*", asOf)
}

func (store *Store) querySplitRules(ctx context.Context, query string, args ...any) ([]SplitRuleRow, error) {
	rows, err := store.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list split rules: %w", err)
	}
	defer rows.Close()
	rules := []SplitRuleRow{}
	for rows.Next() {
		var rule SplitRuleRow
		var checker *string
		if err := rows.Scan(&rule.RuleID, &rule.RevenueLine, &rule.Agency, &rule.Beneficiary,
			&rule.ShareBps, &rule.StatutoryReference, &rule.Provisional, &rule.EffectiveFrom,
			&rule.EffectiveTo, &rule.State, &rule.Maker, &checker); err != nil {
			return nil, fmt.Errorf("scan split rule: %w", err)
		}
		if checker != nil {
			rule.Checker = *checker
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

// SplitComputation is the deterministic result returned by ComputeSplitAt.
type SplitComputation struct {
	RevenueLine string       `json:"revenueLine"`
	AsOf        string       `json:"asOf"`
	AmountMinor int64        `json:"amountMinor"`
	Currency    string       `json:"currency"`
	Allocations []Allocation `json:"allocations"`
	Provisional bool         `json:"provisional"`
}

// ComputeSplitAt runs the deterministic split for one revenue line at asOf.
// It persists nothing — the pure read path behind the API and the advice
// issuance. Fails closed when the window does not sum to 10000 bps.
func (store *Store) ComputeSplitAt(ctx context.Context, revenueLine string, amountMinor int64, currency string, asOf time.Time) (SplitComputation, error) {
	if strings.TrimSpace(revenueLine) == "" {
		return SplitComputation{}, errors.New("revenue line is required")
	}
	if amountMinor <= 0 {
		return SplitComputation{}, errors.New("amount must be positive")
	}
	if currency != "USD" && currency != "NGN" {
		return SplitComputation{}, errors.New("currency must be USD or NGN")
	}
	rules, err := store.activeSplitRules(ctx, revenueLine, asOf)
	if err != nil {
		return SplitComputation{}, err
	}
	allocations, err := ComputeSplitTraced(ctx, amountMinor, rules, asOf)
	if err != nil {
		return SplitComputation{}, err
	}
	provisional := false
	for _, rule := range rules {
		if rule.Provisional {
			provisional = true
		}
	}
	return SplitComputation{
		RevenueLine: revenueLine,
		AsOf:        asOf.UTC().Format("2006-01-02"),
		AmountMinor: amountMinor,
		Currency:    currency,
		Allocations: allocations,
		Provisional: provisional,
	}, nil
}

// IssueRemittanceAdvice seals the deterministic split of one settlement into
// a signed envelope v1.0 artifact. Idempotent by key; replay returns the
// stored advice.
func (store *Store) IssueRemittanceAdvice(ctx context.Context, settlementID, revenueLine string, asOf time.Time, idempotencyKey, actor, correlationID string) (RemittanceAdvice, []byte, error) {
	if settlementID == "" || revenueLine == "" {
		return RemittanceAdvice{}, nil, errors.New("settlement id and revenue line are required")
	}
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return RemittanceAdvice{}, nil, errors.New("idempotency key is required")
	}
	if actor == "" || correlationID == "" {
		return RemittanceAdvice{}, nil, errors.New("actor and correlation id are required")
	}
	settlement, err := store.GetSettlement(ctx, settlementID)
	if err != nil {
		return RemittanceAdvice{}, nil, err
	}
	requestHashInput := struct {
		SettlementID string `json:"settlementId"`
		RevenueLine  string `json:"revenueLine"`
		AsOf         string `json:"asOf"`
	}{settlementID, revenueLine, asOf.UTC().Format("2006-01-02")}
	_, requestHash, err := canonicalJSON(requestHashInput)
	if err != nil {
		return RemittanceAdvice{}, nil, err
	}
	if existing, err := store.adviceByIdempotency(ctx, idempotencyKey); err == nil {
		if existing.hash != requestHash {
			return RemittanceAdvice{}, nil, ErrIdempotencyConflict
		}
		advice, bundle, err := store.getAdvice(ctx, existing.id)
		return advice, bundle, err
	} else if !errors.Is(err, ErrNotFound) {
		return RemittanceAdvice{}, nil, err
	}

	computation, err := store.ComputeSplitAt(ctx, revenueLine, settlement.AmountMinor, settlement.Currency, asOf)
	if err != nil {
		return RemittanceAdvice{}, nil, err
	}
	advice := RemittanceAdvice{
		AdviceID:     uuid.NewString(),
		SettlementID: settlementID,
		RevenueLine:  revenueLine,
		AsOf:         computation.AsOf,
		AmountMinor:  settlement.AmountMinor,
		Currency:     settlement.Currency,
		Allocations:  computation.Allocations,
		CreatedBy:    actor,
	}
	// Agency comes from the winning rule set.
	if rules, err := store.activeSplitRules(ctx, revenueLine, asOf); err == nil && len(rules) > 0 {
		advice.Agency = rules[0].Agency
	}
	bundle, jws, err := store.signer.Sign(ArtifactRemittanceAdvice, advice.AdviceID, advice, store.now())
	if err != nil {
		return RemittanceAdvice{}, nil, fmt.Errorf("seal remittance advice: %w", err)
	}
	allocationsRaw, err := json.Marshal(advice.Allocations)
	if err != nil {
		return RemittanceAdvice{}, nil, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return RemittanceAdvice{}, nil, fmt.Errorf("begin advice transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO tsa_remittance_advices (advice_id, idempotency_key, request_hash, settlement_id,
		     revenue_line, agency, as_of, amount_minor, currency, allocations, envelope, envelope_jws,
		     created_by, correlation_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		advice.AdviceID, idempotencyKey, requestHash, settlementID, revenueLine, advice.Agency,
		computation.AsOf, advice.AmountMinor, advice.Currency, allocationsRaw, bundle, jws,
		actor, correlationID); err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			if existing, lookupErr := store.adviceByIdempotency(ctx, idempotencyKey); lookupErr == nil {
				if existing.hash != requestHash {
					return RemittanceAdvice{}, nil, ErrIdempotencyConflict
				}
				advice, bundle, err := store.getAdvice(ctx, existing.id)
				return advice, bundle, err
			}
			return RemittanceAdvice{}, nil, ErrIdempotencyConflict
		}
		return RemittanceAdvice{}, nil, fmt.Errorf("insert remittance advice: %w", err)
	}
	if err := insertOutbox(ctx, tx, advice.AdviceID, "revenue.remittance_advice.issued", map[string]any{
		"adviceId": advice.AdviceID, "settlementId": settlementID, "revenueLine": revenueLine,
		"amountMinor": advice.AmountMinor, "currency": advice.Currency, "actor": actor,
	}); err != nil {
		return RemittanceAdvice{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RemittanceAdvice{}, nil, fmt.Errorf("commit advice: %w", err)
	}
	recordMoneyOp(ctx, "remittance_advice.issued")
	return advice, bundle, nil
}

// GetRemittanceAdvice loads one advice with its sealed envelope.
func (store *Store) GetRemittanceAdvice(ctx context.Context, adviceID string) (RemittanceAdvice, []byte, error) {
	return store.getAdvice(ctx, adviceID)
}

func (store *Store) getAdvice(ctx context.Context, adviceID string) (RemittanceAdvice, []byte, error) {
	var advice RemittanceAdvice
	var allocationsRaw, bundle []byte
	err := store.pool.QueryRow(ctx,
		`SELECT advice_id, settlement_id, revenue_line, agency, as_of::text, amount_minor, currency,
		        allocations, envelope, created_by, created_at
		 FROM tsa_remittance_advices WHERE advice_id = $1`, adviceID).
		Scan(&advice.AdviceID, &advice.SettlementID, &advice.RevenueLine, &advice.Agency,
			&advice.AsOf, &advice.AmountMinor, &advice.Currency, &allocationsRaw, &bundle,
			&advice.CreatedBy, &advice.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RemittanceAdvice{}, nil, ErrNotFound
	}
	if err != nil {
		return RemittanceAdvice{}, nil, fmt.Errorf("load remittance advice: %w", err)
	}
	if err := json.Unmarshal(allocationsRaw, &advice.Allocations); err != nil {
		return RemittanceAdvice{}, nil, fmt.Errorf("decode allocations: %w", err)
	}
	return advice, bundle, nil
}

type adviceKeyLookup struct {
	id   string
	hash string
}

func (store *Store) adviceByIdempotency(ctx context.Context, key string) (adviceKeyLookup, error) {
	var lookup adviceKeyLookup
	err := store.pool.QueryRow(ctx,
		`SELECT advice_id, request_hash FROM tsa_remittance_advices WHERE idempotency_key = $1`, key).
		Scan(&lookup.id, &lookup.hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return adviceKeyLookup{}, ErrNotFound
	}
	return lookup, err
}
