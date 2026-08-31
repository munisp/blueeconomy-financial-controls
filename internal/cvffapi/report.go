package cvffapi

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvff"
)

// maxReportWindow bounds the auditor report range so one request can never
// trigger an unbounded scan of the disbursement legs.
const maxReportWindow = 366 * 24 * time.Hour

// reportWindow parses and validates the mandatory `from`/`to` query
// parameters: both RFC 3339, from strictly before to, span bounded.
func reportWindow(request *http.Request) (time.Time, time.Time, error) {
	fromText := request.URL.Query().Get("from")
	toText := request.URL.Query().Get("to")
	if fromText == "" || toText == "" {
		return time.Time{}, time.Time{}, errors.New("from and to query parameters are required")
	}
	from, err := time.Parse(time.RFC3339, fromText)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("from is not RFC 3339: %q", fromText)
	}
	to, err := time.Parse(time.RFC3339, toText)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("to is not RFC 3339: %q", toText)
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, errors.New("from must be strictly before to")
	}
	if to.Sub(from) > maxReportWindow {
		return time.Time{}, time.Time{}, fmt.Errorf("report window exceeds the approved %d days", int(maxReportWindow.Hours()/24))
	}
	return from.UTC(), to.UTC(), nil
}

var dualLedgerCSVHeader = []string{
	"application_id", "beneficiary_id", "state",
	"fee_ngn_minor", "cost_usd_minor", "cost_ngn_equivalent", "ngn_per_usd_micro",
	"fee_transfer_id", "cost_transfer_id", "disbursed_at",
}

// dualLedgerReport serves the auditor-facing NGN/USD dual-ledger disbursement
// report. Access is gated on the auditor realm role by the route mount.
func (handler *Handler) dualLedgerReport(writer http.ResponseWriter, request *http.Request) {
	from, to, err := reportWindow(request)
	if err != nil {
		writeProblem(writer, http.StatusUnprocessableEntity, "The report window is invalid: "+err.Error(), nil)
		return
	}
	format := request.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "csv" {
		writeProblem(writer, http.StatusUnprocessableEntity, "The format must be json or csv.", nil)
		return
	}
	report, err := handler.store.DualLedgerReport(request.Context(), from, to)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "The dual-ledger report could not be generated.", nil)
		return
	}
	if format == "csv" {
		writer.Header().Set("Content-Type", "text/csv")
		writer.WriteHeader(http.StatusOK)
		encoder := csv.NewWriter(writer)
		_ = encoder.Write(dualLedgerCSVHeader)
		for _, row := range report {
			_ = encoder.Write(dualLedgerCSVRow(row))
		}
		encoder.Flush()
		return
	}
	writeJSON(writer, http.StatusOK, report)
}

func dualLedgerCSVRow(row cvff.DualLedgerReport) []string {
	return []string{
		row.ApplicationID,
		row.BeneficiaryID,
		string(row.State),
		strconv.FormatUint(row.FeeNGNMinor, 10),
		strconv.FormatUint(row.CostUSDMinor, 10),
		strconv.FormatUint(row.CostNGNEquivalent, 10),
		strconv.FormatUint(row.NGNPerUSDMicro, 10),
		row.FeeTransferID,
		row.CostTransferID,
		row.DisbursedAt.UTC().Format(time.RFC3339),
	}
}
