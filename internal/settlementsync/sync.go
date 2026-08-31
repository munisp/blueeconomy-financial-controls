// Package settlementsync mirrors posted TigerBeetle collection transfers
// into settlement_records (the materialized, queryable mirror the
// reconciliation batch reads; the TB ledger stays the system of record).
//
// The sync polls the ledger with QueryTransfers per configured currency
// ledger, keyed by a durable cursor that advances inside the same
// transaction as each mirrored row (tb_transfer_id UNIQUE makes a replay a
// no-op). Pending reservations and voided transfers are never mirrored —
// only posted money.
//
// Fail-closed posture: when the TigerBeetle address is not configured the
// sync reports ErrUnconfigured and mirrors NOTHING (no fabricated rows);
// transfers on unconfigured ledgers or amounts exceeding int64 minor units
// are hard errors, never silently skipped or truncated.
package settlementsync

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/munisp/blueeconomy-financial-controls/internal/revenue"
	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

// ErrUnconfigured reports the honest unconfigured state: no TigerBeetle
// address was injected, so there is nothing to mirror from.
var ErrUnconfigured = errors.New("TIGERBEETLE_REPLICA_ADDRESSES is not configured; settlement sync is disabled (fail-closed, no rows fabricated)")

// Querier is the TigerBeetle query surface the sync needs (the real client
// in production; fakes in tests).
type Querier interface {
	QueryTransfers(filter tigerbeetle.QueryFilter) ([]tigerbeetle.Transfer, error)
}

// Config is the validated sync wiring.
type Config struct {
	// Ledgers maps a TigerBeetle ledger number to its ISO currency. Only
	// transfers on these ledgers are mirrored; anything else is an error.
	Ledgers map[uint32]string
	// Code is the collection-transfer code to mirror.
	Code uint16
	// Limit bounds one poll batch per ledger.
	Limit uint32
}

// Validate fails closed on a malformed config.
func (config Config) Validate() error {
	if len(config.Ledgers) == 0 {
		return errors.New("at least one ledger->currency mapping is required")
	}
	for ledger, currency := range config.Ledgers {
		if ledger == 0 {
			return errors.New("ledger numbers must be non-zero")
		}
		if currency != "USD" && currency != "NGN" {
			return fmt.Errorf("ledger %d maps to unsupported currency %q", ledger, currency)
		}
	}
	if config.Code == 0 {
		return errors.New("collection transfer code must be non-zero")
	}
	if config.Limit == 0 || config.Limit > 8189 {
		return errors.New("batch limit must be within the TigerBeetle query width")
	}
	return nil
}

// LedgerCurrenciesEnv maps ledger numbers to currencies:
// "1=USD,2=NGN". Required — the sync never guesses a currency.
const LedgerCurrenciesEnv = "TB_SYNC_LEDGERS"

// ParseLedgerCurrencies parses the LedgerCurrenciesEnv value.
func ParseLedgerCurrencies(raw string) (map[uint32]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%s must be set (ledger=currency pairs, e.g. 1=USD,2=NGN)", LedgerCurrenciesEnv)
	}
	ledgers := map[uint32]string{}
	for _, entry := range strings.Split(raw, ",") {
		number, currency, found := strings.Cut(strings.TrimSpace(entry), "=")
		if !found {
			return nil, fmt.Errorf("%s entry %q is not ledger=currency", LedgerCurrenciesEnv, entry)
		}
		parsed, err := strconv.ParseUint(strings.TrimSpace(number), 10, 32)
		if err != nil || parsed == 0 {
			return nil, fmt.Errorf("%s ledger %q is not a non-zero uint32", LedgerCurrenciesEnv, number)
		}
		currency = strings.TrimSpace(currency)
		if currency != "USD" && currency != "NGN" {
			return nil, fmt.Errorf("%s currency %q is not USD or NGN", LedgerCurrenciesEnv, currency)
		}
		ledgers[uint32(parsed)] = currency
	}
	return ledgers, nil
}

// MirrorStore is the settlement-records persistence surface the sync needs
// (revenue.Store in production; fakes in tests).
type MirrorStore interface {
	SyncCursor(ctx context.Context, syncKey string) (uint64, error)
	AdvanceSyncCursor(ctx context.Context, syncKey string, timestamp uint64) error
	RecordTBSettlement(ctx context.Context, input revenue.TBMirrorInput, syncKey string, observedTimestamp uint64) (bool, error)
}

// Syncer mirrors posted collection transfers into settlement_records.
type Syncer struct {
	querier Querier
	store   MirrorStore
	config  Config
}

// NewSyncer fails closed on nil dependencies or an invalid config.
func NewSyncer(querier Querier, store MirrorStore, config Config) (*Syncer, error) {
	if querier == nil {
		return nil, errors.New("TigerBeetle querier is required")
	}
	if store == nil {
		return nil, errors.New("revenue store is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Syncer{querier: querier, store: store, config: config}, nil
}

// SyncOnce polls every configured ledger once and mirrors newly posted
// collection transfers. It returns the number of NEWLY mirrored rows
// (replays advance the cursor but are not counted).
func (syncer *Syncer) SyncOnce(ctx context.Context) (int, error) {
	ledgers := make([]uint32, 0, len(syncer.config.Ledgers))
	for ledger := range syncer.config.Ledgers {
		ledgers = append(ledgers, ledger)
	}
	sort.Slice(ledgers, func(i, j int) bool { return ledgers[i] < ledgers[j] })
	mirrored := 0
	for _, ledger := range ledgers {
		count, err := syncer.syncLedger(ctx, ledger, syncer.config.Ledgers[ledger])
		if err != nil {
			return mirrored, err
		}
		mirrored += count
	}
	return mirrored, nil
}

func (syncer *Syncer) syncLedger(ctx context.Context, ledger uint32, currency string) (int, error) {
	syncKey := fmt.Sprintf("ledger:%d", ledger)
	ctx, span := telemetry.Default().StartSpan(ctx, "settlement.sync.batch", trace.SpanKindInternal,
		attribute.Int("tigerbeetle.ledger", int(ledger)),
		attribute.String("settlement.currency", currency))
	defer span.End()

	cursor, err := syncer.store.SyncCursor(ctx, syncKey)
	if err != nil {
		return 0, err
	}
	transfers, err := syncer.querier.QueryTransfers(tigerbeetle.QueryFilter{
		Ledger:       ledger,
		Code:         syncer.config.Code,
		TimestampMin: cursor + 1,
		Limit:        syncer.config.Limit,
	})
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("query TigerBeetle ledger %d: %w", ledger, err)
	}
	// Ascending timestamp order keeps the cursor monotone.
	sort.Slice(transfers, func(i, j int) bool { return transfers[i].Timestamp < transfers[j].Timestamp })
	mirrored := 0
	for _, transfer := range transfers {
		if transfer.Timestamp <= cursor {
			continue // defensive: the filter already bounds, never re-mirror
		}
		mirror, skip, err := mapTransfer(transfer, currency)
		if err != nil {
			span.RecordError(err)
			return mirrored, err
		}
		if skip {
			// Pending reservations and voids are not posted money; the
			// cursor still advances past them via the next mirrored row or
			// the trailing cursor bump below.
			if err := syncer.bumpCursor(ctx, syncKey, transfer.Timestamp); err != nil {
				return mirrored, err
			}
			continue
		}
		created, err := syncer.store.RecordTBSettlement(ctx, mirror, syncKey, transfer.Timestamp)
		if err != nil {
			span.RecordError(err)
			return mirrored, err
		}
		if created {
			mirrored++
		}
	}
	span.SetAttributes(attribute.Int("settlement.mirrored", mirrored))
	return mirrored, nil
}

// bumpCursor advances the cursor past non-money transfers (pendings/voids)
// without inserting a settlement row.
func (syncer *Syncer) bumpCursor(ctx context.Context, syncKey string, timestamp uint64) error {
	// RecordTBSettlement owns the atomic cursor+row pair; for skips there is
	// no row, so the cursor advances alone — a crash here simply re-reads
	// the skipped transfer, which is again a skip (deterministic).
	return syncer.store.AdvanceSyncCursor(ctx, syncKey, timestamp)
}

// mapTransfer converts one posted collection transfer into its mirror
// record. skip is true for transfers that do not represent posted money
// (pending reservations, voids). References are derived deterministically
// from the transfer identity — the ledger carries no bank-reference
// convention, and the sync never fabricates one.
func mapTransfer(transfer tigerbeetle.Transfer, currency string) (revenue.TBMirrorInput, bool, error) {
	flags := transfer.TransferFlags()
	if flags.Pending || flags.VoidPendingTransfer {
		return revenue.TBMirrorInput{}, true, nil
	}
	low, high := transfer.Amount.Uint64()
	if high != 0 || low > math.MaxInt64 {
		return revenue.TBMirrorInput{}, false, fmt.Errorf(
			"transfer %s amount exceeds int64 minor units — refusing to truncate money", transfer.ID.String())
	}
	if low == 0 {
		return revenue.TBMirrorInput{}, false, fmt.Errorf("transfer %s has a zero amount", transfer.ID.String())
	}
	transferID := transfer.ID.String()
	return revenue.TBMirrorInput{
		TBTransferID:  transferID,
		AmountMinor:   int64(low),
		Currency:      currency,
		BankReference: "tb:" + transferID,
		PayerRef:      "tb-debit:" + transfer.DebitAccountID.String(),
		ValueDate:     time.Unix(0, int64(transfer.Timestamp)).UTC().Format("2006-01-02"),
	}, false, nil
}
