package revenue

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Reporting primitives the ministry dashboards consume. All are real SQL
// over the real tables — no derived caches, so the numbers always reflect
// the ledger state at query time.

// AgencyRevenue is one agency's billed/settled position over a window.
type AgencyRevenue struct {
	Agency          string `json:"agency"`
	NotesIssued     int    `json:"notesIssued"`
	BilledUSDMinor  int64  `json:"billedUsdMinor"`
	BilledNGNMinor  int64  `json:"billedNgnMinor"`
	SettledNotes    int    `json:"settledNotes"`
	SettledUSDMinor int64  `json:"settledUsdMinor"`
	SettledNGNMinor int64  `json:"settledNgnMinor"`
	OutstandingUSD  int64  `json:"outstandingUsdMinor"`
	OutstandingNGN  int64  `json:"outstandingNgnMinor"`
}

// RevenueByAgency aggregates issued debit notes per agency over
// [from, to] on effective_date.
func (store *Store) RevenueByAgency(ctx context.Context, from, to time.Time) ([]AgencyRevenue, error) {
	rows, err := store.pool.Query(ctx,
		`SELECT agency,
		        COUNT(*) FILTER (WHERE state <> 'DRAFT'),
		        COALESCE(SUM(amount_usd_minor) FILTER (WHERE state <> 'DRAFT'), 0),
		        COALESCE(SUM(amount_ngn_minor) FILTER (WHERE state <> 'DRAFT'), 0),
		        COUNT(*) FILTER (WHERE state = 'SETTLED'),
		        COALESCE(SUM(amount_usd_minor) FILTER (WHERE state = 'SETTLED'), 0),
		        COALESCE(SUM(amount_ngn_minor) FILTER (WHERE state = 'SETTLED'), 0),
		        COALESCE(SUM(amount_usd_minor) FILTER (WHERE state IN ('ISSUED', 'ACKED', 'DISPUTED')), 0),
		        COALESCE(SUM(amount_ngn_minor) FILTER (WHERE state IN ('ISSUED', 'ACKED', 'DISPUTED')), 0)
		 FROM revenue_debit_notes
		 WHERE effective_date BETWEEN $1 AND $2
		 GROUP BY agency ORDER BY agency`, from, to)
	if err != nil {
		return nil, fmt.Errorf("revenue by agency: %w", err)
	}
	defer rows.Close()
	report := []AgencyRevenue{}
	for rows.Next() {
		var row AgencyRevenue
		if err := rows.Scan(&row.Agency, &row.NotesIssued, &row.BilledUSDMinor, &row.BilledNGNMinor,
			&row.SettledNotes, &row.SettledUSDMinor, &row.SettledNGNMinor,
			&row.OutstandingUSD, &row.OutstandingNGN); err != nil {
			return nil, fmt.Errorf("scan agency revenue: %w", err)
		}
		report = append(report, row)
	}
	return report, rows.Err()
}

// LineRevenue is one revenue line's billed/settled position.
type LineRevenue struct {
	Instrument       string `json:"instrument"`
	Agency           string `json:"agency"`
	Currency         string `json:"currency"`
	BilledMinor      int64  `json:"billedMinor"`
	SettledMinor     int64  `json:"settledMinor"`
	OutstandingMinor int64  `json:"outstandingMinor"`
}

// RevenueByLine aggregates debit-note lines per instrument over [from, to].
func (store *Store) RevenueByLine(ctx context.Context, from, to time.Time) ([]LineRevenue, error) {
	rows, err := store.pool.Query(ctx,
		`SELECT l.instrument, l.agency, l.currency,
		        COALESCE(SUM(l.amount_minor) FILTER (WHERE n.state <> 'DRAFT'), 0),
		        COALESCE(SUM(l.amount_minor) FILTER (WHERE n.state = 'SETTLED'), 0),
		        COALESCE(SUM(l.amount_minor) FILTER (WHERE n.state IN ('ISSUED', 'ACKED', 'DISPUTED')), 0)
		 FROM revenue_debit_note_lines l
		 JOIN revenue_debit_notes n ON n.debit_note_id = l.debit_note_id
		 WHERE n.effective_date BETWEEN $1 AND $2
		 GROUP BY l.instrument, l.agency, l.currency
		 ORDER BY l.instrument, l.agency, l.currency`, from, to)
	if err != nil {
		return nil, fmt.Errorf("revenue by line: %w", err)
	}
	defer rows.Close()
	report := []LineRevenue{}
	for rows.Next() {
		var row LineRevenue
		if err := rows.Scan(&row.Instrument, &row.Agency, &row.Currency,
			&row.BilledMinor, &row.SettledMinor, &row.OutstandingMinor); err != nil {
			return nil, fmt.Errorf("scan line revenue: %w", err)
		}
		report = append(report, row)
	}
	return report, rows.Err()
}

// AgingBucket is one aging band of unsettled debit notes.
type AgingBucket struct {
	Bucket         string `json:"bucket"` // 0-30 | 31-60 | 61-90 | 90+
	Agency         string `json:"agency"`
	NoteCount      int    `json:"noteCount"`
	AmountUSDMinor int64  `json:"amountUsdMinor"`
	AmountNGNMinor int64  `json:"amountNgnMinor"`
}

// AgingUnsettled buckets unsettled (ISSUED/ACKED/DISPUTED) notes by age at
// asOf — the revenue-assurance watch list.
func (store *Store) AgingUnsettled(ctx context.Context, asOf time.Time) ([]AgingBucket, error) {
	rows, err := store.pool.Query(ctx,
		`SELECT CASE
		         WHEN $1::date - effective_date <= 30 THEN '0-30'
		         WHEN $1::date - effective_date <= 60 THEN '31-60'
		         WHEN $1::date - effective_date <= 90 THEN '61-90'
		         ELSE '90+' END AS bucket,
		        agency, COUNT(*),
		        COALESCE(SUM(amount_usd_minor), 0), COALESCE(SUM(amount_ngn_minor), 0)
		 FROM revenue_debit_notes
		 WHERE state IN ('ISSUED', 'ACKED', 'DISPUTED')
		 GROUP BY bucket, agency
		 ORDER BY bucket, agency`, asOf.UTC().Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("aging unsettled: %w", err)
	}
	defer rows.Close()
	report := []AgingBucket{}
	for rows.Next() {
		var row AgingBucket
		if err := rows.Scan(&row.Bucket, &row.Agency, &row.NoteCount, &row.AmountUSDMinor, &row.AmountNGNMinor); err != nil {
			return nil, fmt.Errorf("scan aging bucket: %w", err)
		}
		report = append(report, row)
	}
	return report, rows.Err()
}

// ExceptionCount is one class/state cell of the exception queue.
type ExceptionCount struct {
	Class string `json:"class"`
	State string `json:"state"`
	Count int    `json:"count"`
}

// ExceptionCounts summarizes the exception queue by class and state.
func (store *Store) ExceptionCounts(ctx context.Context) ([]ExceptionCount, error) {
	rows, err := store.pool.Query(ctx,
		`SELECT class, state, COUNT(*) FROM recon_exceptions GROUP BY class, state ORDER BY class, state`)
	if err != nil {
		return nil, fmt.Errorf("exception counts: %w", err)
	}
	defer rows.Close()
	report := []ExceptionCount{}
	for rows.Next() {
		var row ExceptionCount
		if err := rows.Scan(&row.Class, &row.State, &row.Count); err != nil {
			return nil, fmt.Errorf("scan exception count: %w", err)
		}
		report = append(report, row)
	}
	return report, rows.Err()
}

var errWindowRequired = errors.New("report window (from/to) is required")

// parseWindow validates a reporting window.
func parseWindow(from, to time.Time) error {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return errWindowRequired
	}
	return nil
}
