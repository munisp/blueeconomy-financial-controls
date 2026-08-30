package revenue

import (
	"fmt"
	"time"
)

// This file holds the pure reconciliation planning logic: the store loads
// the unmatched legs, planRecon decides matches and exceptions
// deterministically, and the store persists the decisions in one
// transaction. Keeping the decision logic pure makes every exception class
// unit-testable and re-runs byte-stable.

// statementLeg is one unmatched CREDIT statement line with its location.
type statementLeg struct {
	StatementID   string
	LineNo        int
	BankReference string
	ValueDate     string
	AmountMinor   int64
	Currency      string
}

// reconInput is the unmatched snapshot for one batch.
type reconInput struct {
	Notes       []DebitNote    // state ISSUED/ACKED/DISPUTED, no match yet
	Settlements []Settlement   // no match yet
	Lines       []statementLeg // CREDIT, no match yet
	AsOf        time.Time
}

type matchDecision struct {
	DebitNoteID     string
	SettlementID    string
	StatementID     string
	StatementLineNo int
	AmountMinor     int64
	Currency        string
}

type exceptionDecision struct {
	Class           string
	DedupeKey       string
	DebitNoteID     string
	SettlementID    string
	StatementID     string
	StatementLineNo int
	ExpectedMinor   *int64
	ActualMinor     *int64
	Currency        string
	Detail          string
}

type reconPlan struct {
	Matches    []matchDecision
	Exceptions []exceptionDecision
	// SettledNotes maps note -> settlement for the SETTLED transition.
	SettledNotes map[string]string
}

// noteAmountIn returns the note's amount in the given currency.
func noteAmountIn(note DebitNote, currency string) int64 {
	if currency == "NGN" {
		return note.AmountNGNMinor
	}
	return note.AmountUSDMinor
}

// planRecon pairs the three legs. A clean three-way match requires:
// settlement identifies the note (debit_note_id, or bank_reference equal to
// the note's document number), the amounts agree in the settlement's
// currency, and a statement line with the same bank reference and amount
// exists. Settlements whose statement leg has not arrived stay pending (no
// exception — the next run completes them); every other failure mode is an
// explicit exception class.
func planRecon(input reconInput) reconPlan {
	plan := reconPlan{SettledNotes: map[string]string{}}
	notesByID := map[string]DebitNote{}
	notesByDoc := map[string]DebitNote{}
	for _, note := range input.Notes {
		notesByID[note.DebitNoteID] = note
		if note.DocumentNumber != "" {
			notesByDoc[note.DocumentNumber] = note
		}
	}
	linesByRef := map[string]statementLeg{}
	for _, line := range input.Lines {
		// First line wins on a shared reference; the store raises
		// DUPLICATE_BANK_REF for conflicting duplicates at ingest.
		if _, exists := linesByRef[line.BankReference]; !exists {
			linesByRef[line.BankReference] = line
		}
	}
	matchedNotes := map[string]bool{}
	matchedLines := map[string]bool{}
	matchedSettlements := map[string]bool{}

	// Duplicate bank references across settlements: the later settlement is
	// an exception, never silently matched.
	settlementsByRef := map[string][]Settlement{}
	for _, settlement := range input.Settlements {
		settlementsByRef[settlement.BankReference] = append(settlementsByRef[settlement.BankReference], settlement)
	}

	for _, settlement := range input.Settlements {
		if siblings := settlementsByRef[settlement.BankReference]; len(siblings) > 1 {
			// Fail closed on a shared bank reference: EVERY sibling is
			// frozen as a duplicate-suspect and none is matched — one of
			// them may be fraudulent, and matching either silently would
			// settle a note on possibly-duplicated money.
			plan.Exceptions = append(plan.Exceptions, exceptionDecision{
				Class:        ExceptionDuplicateBankRef,
				DedupeKey:    ExceptionDuplicateBankRef + "|settlement:" + settlement.SettlementID,
				SettlementID: settlement.SettlementID,
				Currency:     settlement.Currency,
				ActualMinor:  int64Ptr(settlement.AmountMinor),
				Detail: fmt.Sprintf("bank reference %q is shared with settlement %s",
					settlement.BankReference, siblings[0].SettlementID),
			})
			matchedSettlements[settlement.SettlementID] = true // excluded from further matching
			continue
		}
		note, identified := notesByID[settlement.DebitNoteID]
		if !identified && settlement.DebitNoteID == "" {
			note, identified = notesByDoc[settlement.BankReference]
		}
		if !identified {
			plan.Exceptions = append(plan.Exceptions, exceptionDecision{
				Class:        ExceptionUnmatchedSettlement,
				DedupeKey:    ExceptionUnmatchedSettlement + "|settlement:" + settlement.SettlementID,
				SettlementID: settlement.SettlementID,
				ActualMinor:  int64Ptr(settlement.AmountMinor),
				Currency:     settlement.Currency,
				Detail:       "settlement does not identify any open debit note",
			})
			continue
		}
		expected := noteAmountIn(note, settlement.Currency)
		if expected != settlement.AmountMinor {
			plan.Exceptions = append(plan.Exceptions, exceptionDecision{
				Class:         ExceptionAmountMismatch,
				DedupeKey:     ExceptionAmountMismatch + "|settlement:" + settlement.SettlementID,
				DebitNoteID:   note.DebitNoteID,
				SettlementID:  settlement.SettlementID,
				ExpectedMinor: int64Ptr(expected),
				ActualMinor:   int64Ptr(settlement.AmountMinor),
				Currency:      settlement.Currency,
				Detail:        "settlement amount differs from the debit-note amount",
			})
			continue
		}
		line, found := linesByRef[settlement.BankReference]
		if !found {
			// Statement leg pending: leave both legs unmatched for the next
			// run. No exception — late statements are normal operations.
			continue
		}
		if line.AmountMinor != settlement.AmountMinor || line.Currency != settlement.Currency {
			plan.Exceptions = append(plan.Exceptions, exceptionDecision{
				Class:           ExceptionAmountMismatch,
				DedupeKey:       ExceptionAmountMismatch + "|line:" + line.StatementID + ":" + fmt.Sprint(line.LineNo),
				DebitNoteID:     note.DebitNoteID,
				SettlementID:    settlement.SettlementID,
				StatementID:     line.StatementID,
				StatementLineNo: line.LineNo,
				ExpectedMinor:   int64Ptr(settlement.AmountMinor),
				ActualMinor:     int64Ptr(line.AmountMinor),
				Currency:        settlement.Currency,
				Detail:          "statement line amount differs from the settlement amount",
			})
			continue
		}
		plan.Matches = append(plan.Matches, matchDecision{
			DebitNoteID:     note.DebitNoteID,
			SettlementID:    settlement.SettlementID,
			StatementID:     line.StatementID,
			StatementLineNo: line.LineNo,
			AmountMinor:     settlement.AmountMinor,
			Currency:        settlement.Currency,
		})
		plan.SettledNotes[note.DebitNoteID] = settlement.SettlementID
		matchedNotes[note.DebitNoteID] = true
		matchedLines[line.StatementID+":"+fmt.Sprint(line.LineNo)] = true
		matchedSettlements[settlement.SettlementID] = true
	}

	// Statement lines with no settlement leg at all.
	for _, line := range input.Lines {
		if matchedLines[line.StatementID+":"+fmt.Sprint(line.LineNo)] {
			continue
		}
		if _, hasSettlement := settlementsByRef[line.BankReference]; hasSettlement {
			continue // settlement exists but failed its own matching — its exception carries it
		}
		plan.Exceptions = append(plan.Exceptions, exceptionDecision{
			Class:           ExceptionUnmatchedStatement,
			DedupeKey:       ExceptionUnmatchedStatement + "|line:" + line.StatementID + ":" + fmt.Sprint(line.LineNo),
			StatementID:     line.StatementID,
			StatementLineNo: line.LineNo,
			ActualMinor:     int64Ptr(line.AmountMinor),
			Currency:        line.Currency,
			Detail:          "statement line has no corresponding settlement record",
		})
	}

	// Open notes past due with no settlement leg.
	asOfDate := input.AsOf.UTC().Format("2006-01-02")
	for _, note := range input.Notes {
		if matchedNotes[note.DebitNoteID] {
			continue
		}
		if note.DueDate >= asOfDate {
			continue // not yet due
		}
		hasSettlement := false
		for _, settlement := range input.Settlements {
			if settlement.DebitNoteID == note.DebitNoteID || settlement.BankReference == note.DocumentNumber {
				hasSettlement = true
				break
			}
		}
		if hasSettlement {
			continue // a settlement exists; its own exception (if any) carries it
		}
		plan.Exceptions = append(plan.Exceptions, exceptionDecision{
			Class:         ExceptionUnmatchedAssessment,
			DedupeKey:     ExceptionUnmatchedAssessment + "|note:" + note.DebitNoteID,
			DebitNoteID:   note.DebitNoteID,
			ExpectedMinor: int64Ptr(note.AmountUSDMinor),
			Currency:      "USD",
			Detail:        "debit note is past due with no settlement record",
		})
	}
	return plan
}

func int64Ptr(value int64) *int64 { return &value }
