package glexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
)

// Export types recorded in export_batches.
const (
	ExportTypeCamt053 = "CAMT053_STATEMENT"
	ExportTypePain001 = "PAIN001_CREDIT_TRANSFER"
)

// artifactKind is the envelope artifact code carried on every GL export.
const artifactKind = "gl-export"

// Service runs the GL-export workflows: ISO 20022 feed generation, export
// audit trail (payload hash + signed envelope + exporter identity), period
// close with dual control, and journals-vs-exports reconciliation.
type Service struct {
	store    *Store
	signer   *envelope.Signer
	owner    string // account owner / initiating party name on feeds
	servicer string // servicer BICFI on feeds
	now      func() time.Time
}

// NewService fails closed on any missing dependency. owner and servicer
// identify the platform on the exported feeds and come from configuration
// (env), never from request input.
func NewService(store *Store, signer *envelope.Signer, owner, servicer string) (*Service, error) {
	if store == nil {
		return nil, errors.New("store is required")
	}
	if signer == nil {
		return nil, errors.New("envelope signer is required")
	}
	if owner == "" {
		return nil, errors.New("account owner name is required")
	}
	if servicer == "" {
		return nil, errors.New("servicer BIC is required")
	}
	return &Service{
		store:    store,
		signer:   signer,
		owner:    owner,
		servicer: servicer,
		now:      func() time.Time { return time.Now().UTC() },
	}, nil
}

// deterministicID derives a stable UUID from a natural key, so replays of the
// same logical artifact collide on the primary key instead of duplicating.
func deterministicID(parts ...string) string {
	key := ""
	for index, part := range parts {
		if index > 0 {
			key += "|"
		}
		key += part
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// signExport seals the export audit payload into an envelope v1.0 bundle.
func (service *Service) signExport(batch ExportBatch) (json.RawMessage, string, error) {
	payload := map[string]any{
		"batch_id":           batch.BatchID,
		"export_type":        batch.ExportType,
		"account_ref":        batch.AccountRef,
		"period_start":       batch.PeriodStart.UTC().Format("2006-01-02"),
		"period_end":         batch.PeriodEnd.UTC().Format("2006-01-02"),
		"currency":           batch.Currency,
		"entry_count":        batch.EntryCount,
		"total_debit_minor":  batch.TotalDebitMinor,
		"total_credit_minor": batch.TotalCreditMinor,
		"payload_sha256":     batch.PayloadSHA256,
		"exported_by":        batch.ExportedBy,
	}
	bundle, jws, err := service.signer.Sign(artifactKind, batch.BatchID, payload, service.now())
	if err != nil {
		return nil, "", fmt.Errorf("sign export envelope: %w", err)
	}
	return json.RawMessage(bundle), jws, nil
}

// ExportStatement renders, signs and records one camt.053.001.08 statement
// for one GL account over one period. A byte-identical re-export fails with
// ErrExportReplay (409 upstream) — replays fetch the stored payload instead.
func (service *Service) ExportStatement(ctx context.Context, account, currency string, start, end time.Time, actor string) (ExportBatch, error) {
	start, end = dateOnly(start), dateOnly(end)
	if account == "" || currency == "" {
		return ExportBatch{}, errors.New("account and currency are required")
	}
	if end.Before(start) {
		return ExportBatch{}, errors.New("period end precedes period start")
	}

	entries, err := service.store.JournalEntries(ctx, start, end)
	if err != nil {
		return ExportBatch{}, err
	}
	scoped := make([]JournalEntry, 0, len(entries))
	var totalDebit, totalCredit int64
	for _, entry := range entries {
		if entry.Account != account || entry.Currency != currency {
			continue
		}
		scoped = append(scoped, entry)
		if entry.Direction == DirectionDebit {
			totalDebit += entry.AmountMinor
		} else {
			totalCredit += entry.AmountMinor
		}
	}
	opening, err := service.store.OpeningBalance(ctx, account, start)
	if err != nil {
		return ExportBatch{}, err
	}

	statementID := deterministicID("camt053", account, currency, start.Format("2006-01-02"), end.Format("2006-01-02"))
	xmlPayload, err := BuildCamt053(Camt053Params{
		MessageID:    "MSG-" + statementID,
		StatementID:  statementID,
		Sequence:     1,
		AccountRef:   account,
		AccountOwner: service.owner,
		ServicerBIC:  service.servicer,
		Currency:     currency,
		PeriodStart:  start,
		PeriodEnd:    end,
		OpeningMinor: opening,
		Entries:      scoped,
		GeneratedAt:  service.now(),
	})
	if err != nil {
		return ExportBatch{}, err
	}

	digest := sha256.Sum256(xmlPayload)
	batch := ExportBatch{
		BatchID:          statementID,
		ExportType:       ExportTypeCamt053,
		AccountRef:       account,
		PeriodStart:      start,
		PeriodEnd:        end,
		Currency:         currency,
		EntryCount:       len(scoped),
		TotalDebitMinor:  totalDebit,
		TotalCreditMinor: totalCredit,
		PayloadSHA256:    hex.EncodeToString(digest[:]),
		Payload:          string(xmlPayload),
		SignerKeyID:      service.signer.KeyID(),
		ExportedBy:       actor,
	}
	bundle, jws, err := service.signExport(batch)
	if err != nil {
		return ExportBatch{}, err
	}
	batch.Envelope = bundle
	batch.EnvelopeJWS = jws
	if err := service.store.InsertExportBatch(ctx, batch); err != nil {
		return ExportBatch{}, err
	}
	return batch, nil
}

// ExportCreditTransfers renders, signs and records one pain.001.001.09
// initiation message for every approved disbursement/refund instruction in
// one currency over one period.
func (service *Service) ExportCreditTransfers(ctx context.Context, currency string, start, end time.Time, actor string) (ExportBatch, error) {
	start, end = dateOnly(start), dateOnly(end)
	if currency == "" {
		return ExportBatch{}, errors.New("currency is required")
	}
	if end.Before(start) {
		return ExportBatch{}, errors.New("period end precedes period start")
	}

	all, err := service.store.DisbursementInstructions(ctx, start, end)
	if err != nil {
		return ExportBatch{}, err
	}
	var scoped []CreditInstruction
	var total int64
	for _, instruction := range all {
		if instruction.Currency != currency {
			continue
		}
		scoped = append(scoped, instruction)
		total += instruction.AmountMinor
	}
	if len(scoped) == 0 {
		return ExportBatch{}, errors.New("no approved disbursement instructions in scope")
	}

	batchID := deterministicID("pain001", currency, start.Format("2006-01-02"), end.Format("2006-01-02"))
	xmlPayload, err := BuildPain001(Pain001Params{
		MessageID:     "MSG-" + batchID,
		PaymentInfoID: "PMT-" + batchID,
		DebtorName:    service.owner,
		DebtorAcct:    "TSA:" + currency,
		DebtorBIC:     service.servicer,
		Instructions:  scoped,
		GeneratedAt:   service.now(),
	})
	if err != nil {
		return ExportBatch{}, err
	}

	digest := sha256.Sum256(xmlPayload)
	batch := ExportBatch{
		BatchID:          batchID,
		ExportType:       ExportTypePain001,
		AccountRef:       "TSA:" + currency,
		PeriodStart:      start,
		PeriodEnd:        end,
		Currency:         currency,
		EntryCount:       len(scoped),
		TotalDebitMinor:  0,
		TotalCreditMinor: total,
		PayloadSHA256:    hex.EncodeToString(digest[:]),
		Payload:          string(xmlPayload),
		SignerKeyID:      service.signer.KeyID(),
		ExportedBy:       actor,
	}
	bundle, jws, err := service.signExport(batch)
	if err != nil {
		return ExportBatch{}, err
	}
	batch.Envelope = bundle
	batch.EnvelopeJWS = jws
	if err := service.store.InsertExportBatch(ctx, batch); err != nil {
		return ExportBatch{}, err
	}
	return batch, nil
}

// RequestPeriodClose performs the maker step of a period close.
func (service *Service) RequestPeriodClose(ctx context.Context, start, end time.Time, actor string) (PeriodClose, error) {
	start, end = dateOnly(start), dateOnly(end)
	if end.Before(start) {
		return PeriodClose{}, errors.New("period end precedes period start")
	}
	close := PeriodClose{
		PeriodCloseID: deterministicID("period-close", start.Format("2006-01-02"), end.Format("2006-01-02")),
		PeriodStart:   start,
		PeriodEnd:     end,
		Status:        "PENDING_APPROVAL",
		RequestedBy:   actor,
	}
	if err := service.store.CreatePeriodClose(ctx, close); err != nil {
		return PeriodClose{}, err
	}
	return service.store.GetPeriodClose(ctx, close.PeriodCloseID)
}

// ApprovePeriodClose performs the checker step: a different officer than the
// maker, freezing the current trial balance as close evidence.
func (service *Service) ApprovePeriodClose(ctx context.Context, periodCloseID, actor string) (PeriodClose, error) {
	close, err := service.store.GetPeriodClose(ctx, periodCloseID)
	if err != nil {
		return PeriodClose{}, err
	}
	balance, err := service.store.TrialBalance(ctx, close.PeriodStart, close.PeriodEnd)
	if err != nil {
		return PeriodClose{}, err
	}
	frozen, err := json.Marshal(balance)
	if err != nil {
		return PeriodClose{}, fmt.Errorf("marshal trial balance: %w", err)
	}
	if err := service.store.ApprovePeriodClose(ctx, periodCloseID, actor, frozen, service.now()); err != nil {
		return PeriodClose{}, err
	}
	return service.store.GetPeriodClose(ctx, periodCloseID)
}

// AccountDifference is one account's drift between the journal and the
// recorded exports for the same period.
type AccountDifference struct {
	Account             string `json:"account"`
	Currency            string `json:"currency"`
	JournalDebitMinor   int64  `json:"journal_debit_minor"`
	JournalCreditMinor  int64  `json:"journal_credit_minor"`
	ExportedDebitMinor  int64  `json:"exported_debit_minor"`
	ExportedCreditMinor int64  `json:"exported_credit_minor"`
	Reason              string `json:"reason"`
}

// RunReconciliation compares the authoritative journal against the recorded
// camt.053 exports account-by-account for one period and persists the run as
// replayable evidence.
func (service *Service) RunReconciliation(ctx context.Context, start, end time.Time, actor string) (ReconciliationRun, error) {
	start, end = dateOnly(start), dateOnly(end)
	if end.Before(start) {
		return ReconciliationRun{}, errors.New("period end precedes period start")
	}
	journal, err := service.store.TrialBalance(ctx, start, end)
	if err != nil {
		return ReconciliationRun{}, err
	}
	batches, err := service.store.ExportBatches(ctx, "", start, end)
	if err != nil {
		return ReconciliationRun{}, err
	}

	exported := map[string]*AccountDifference{}
	for _, batch := range batches {
		if batch.ExportType != ExportTypeCamt053 {
			continue
		}
		diff, ok := exported[batch.AccountRef]
		if !ok {
			diff = &AccountDifference{Account: batch.AccountRef, Currency: batch.Currency}
			exported[batch.AccountRef] = diff
		}
		diff.ExportedDebitMinor += batch.TotalDebitMinor
		diff.ExportedCreditMinor += batch.TotalCreditMinor
	}

	var journalCount, exportedCount int
	var journalTotal, exportedTotal int64
	var differences []AccountDifference
	for _, row := range journal {
		journalCount += row.EntryCount
		journalTotal += row.DebitTotalMinor
		diff, ok := exported[row.Account]
		if !ok {
			differences = append(differences, AccountDifference{
				Account:            row.Account,
				Currency:           row.Currency,
				JournalDebitMinor:  row.DebitTotalMinor,
				JournalCreditMinor: row.CreditTotalMinor,
				Reason:             "account has no recorded export for the period",
			})
			continue
		}
		diff.JournalDebitMinor = row.DebitTotalMinor
		diff.JournalCreditMinor = row.CreditTotalMinor
		if diff.ExportedDebitMinor != row.DebitTotalMinor || diff.ExportedCreditMinor != row.CreditTotalMinor {
			diff.Reason = "exported totals diverge from journal totals"
			differences = append(differences, *diff)
		}
	}
	for account, diff := range exported {
		found := false
		for _, row := range journal {
			if row.Account == account {
				found = true
				break
			}
		}
		if !found {
			diff.Reason = "export recorded for an account with no journal movement"
			differences = append(differences, *diff)
		}
	}
	for _, batch := range batches {
		if batch.ExportType == ExportTypeCamt053 {
			exportedCount += batch.EntryCount
			exportedTotal += batch.TotalDebitMinor + batch.TotalCreditMinor
		}
	}
	sort.Slice(differences, func(i, j int) bool { return differences[i].Account < differences[j].Account })
	differencesJSON, err := json.Marshal(differences)
	if err != nil {
		return ReconciliationRun{}, fmt.Errorf("marshal reconciliation differences: %w", err)
	}

	run := ReconciliationRun{
		RunID:              deterministicID("recon", start.Format("2006-01-02"), end.Format("2006-01-02"), service.now().UTC().Format(time.RFC3339Nano)),
		PeriodStart:        start,
		PeriodEnd:          end,
		JournalEntryCount:  journalCount,
		JournalTotalMinor:  journalTotal,
		ExportedEntryCount: exportedCount,
		ExportedTotalMinor: exportedTotal,
		Balanced:           len(differences) == 0,
		Differences:        differencesJSON,
		RunBy:              actor,
	}
	if err := service.store.InsertReconciliationRun(ctx, run); err != nil {
		return ReconciliationRun{}, err
	}
	runs, err := service.store.ListReconciliationRuns(ctx, start, end, 1)
	if err != nil {
		return ReconciliationRun{}, err
	}
	if len(runs) == 0 {
		return ReconciliationRun{}, errors.New("reconciliation run was not persisted")
	}
	return runs[0], nil
}
