package glexport

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestAmountRendersMinorUnitsDeterministically(t *testing.T) {
	cases := map[int64]string{
		0:        "0.00",
		1:        "0.01",
		99:       "0.99",
		100:      "1.00",
		123456:   "1234.56",
		-123456:  "-1234.56",
		99999999: "999999.99",
	}
	for minor, want := range cases {
		if got := Amount(minor); got != want {
			t.Fatalf("Amount(%d) = %q, want %q", minor, got, want)
		}
	}
}

// camt053Probe mirrors the ISO 20022 structure for round-trip validation:
// the emitted XML must parse back into the BkToCstmrStmt hierarchy with the
// expected header, balances and entries.
type camt053Probe struct {
	XMLName xml.Name `xml:"Document"`
	Xmlns   string   `xml:"xmlns,attr"`
	Stmt    struct {
		GrpHdr struct {
			MsgId string `xml:"MsgId"`
		} `xml:"GrpHdr"`
		Stmt struct {
			Id   string `xml:"Id"`
			Acct struct {
				Ccy string `xml:"Ccy"`
			} `xml:"Acct"`
			Bal []struct {
				Code      string `xml:"Tp>CdOrPrtry>Cd"`
				Amount    string `xml:"Amt"`
				CreditInd string `xml:"CdtDbtInd"`
			} `xml:"Bal"`
			Ntry []struct {
				Ref       string `xml:"NtryRef"`
				Amount    string `xml:"Amt"`
				CreditInd string `xml:"CdtDbtInd"`
				Status    string `xml:"Sts>Cd"`
				EndToEnd  string `xml:"NtryDtls>TxDtls>Refs>EndToEndId"`
			} `xml:"Ntry"`
		} `xml:"Stmt"`
	} `xml:"BkToCstmrStmt"`
}

func statementParams() Camt053Params {
	return Camt053Params{
		MessageID:    "MSG-TEST-1",
		StatementID:  "STMT-TEST-1",
		Sequence:     7,
		AccountRef:   "TSA:NGN",
		AccountOwner: "Federation Treasury Single Account",
		ServicerBIC:  "NIBONGNGLAG",
		Currency:     "NGN",
		PeriodStart:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:    time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
		OpeningMinor: 500000,
		Entries: []JournalEntry{
			{
				EntryID: "SETL-1-D", Account: "TSA:NGN", Direction: DirectionDebit,
				AmountMinor: 250000, Currency: "NGN",
				ValueDate:   time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC),
				BookingDate: time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC),
				Reference:   "BNK-REF-1", Counterparty: "PAYER-A", Narrative: "collection",
			},
			{
				EntryID: "CVFF-1-C", Account: "TSA:NGN", Direction: DirectionCredit,
				AmountMinor: 100000, Currency: "NGN",
				ValueDate:   time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC),
				BookingDate: time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC),
				Reference:   "CVFF-EXT-1", Counterparty: "BENEF-A", Narrative: "disbursement",
			},
		},
		GeneratedAt: time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC),
	}
}

func TestBuildCamt053RoundTripsThroughISO20022Structure(t *testing.T) {
	raw, err := BuildCamt053(statementParams())
	if err != nil {
		t.Fatalf("BuildCamt053: %v", err)
	}
	var probe camt053Probe
	if err := xml.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("statement does not round-trip through the camt.053 structure: %v", err)
	}
	if probe.Xmlns != Camt053Namespace {
		t.Fatalf("namespace = %q, want %q", probe.Xmlns, Camt053Namespace)
	}
	if probe.Stmt.GrpHdr.MsgId != "MSG-TEST-1" {
		t.Fatalf("MsgId = %q", probe.Stmt.GrpHdr.MsgId)
	}
	if probe.Stmt.Stmt.Acct.Ccy != "NGN" {
		t.Fatalf("account currency = %q", probe.Stmt.Stmt.Acct.Ccy)
	}
	if len(probe.Stmt.Stmt.Bal) != 2 {
		t.Fatalf("balances = %d, want 2 (OPBD/CLBD)", len(probe.Stmt.Stmt.Bal))
	}
	opening, closing := probe.Stmt.Stmt.Bal[0], probe.Stmt.Stmt.Bal[1]
	if opening.Code != "OPBD" || opening.Amount != "5000.00" || opening.CreditInd != "CRDT" {
		t.Fatalf("unexpected opening balance: %+v", opening)
	}
	// closing = 500000 + 250000 - 100000 = 650000 minor = 6500.00
	if closing.Code != "CLBD" || closing.Amount != "6500.00" || closing.CreditInd != "CRDT" {
		t.Fatalf("unexpected closing balance: %+v", closing)
	}
	if len(probe.Stmt.Stmt.Ntry) != 2 {
		t.Fatalf("entries = %d, want 2", len(probe.Stmt.Stmt.Ntry))
	}
	first := probe.Stmt.Stmt.Ntry[0]
	if first.Ref != "SETL-1-D" || first.CreditInd != "CRDT" || first.Amount != "2500.00" || first.Status != "BOOK" {
		t.Fatalf("unexpected first entry: %+v", first)
	}
	if first.EndToEnd != "BNK-REF-1" {
		t.Fatalf("end-to-end id = %q", first.EndToEnd)
	}
	second := probe.Stmt.Stmt.Ntry[1]
	if second.CreditInd != "DBIT" || second.Amount != "1000.00" {
		t.Fatalf("unexpected second entry: %+v", second)
	}
}

func TestBuildCamt053IsDeterministic(t *testing.T) {
	first, err := BuildCamt053(statementParams())
	if err != nil {
		t.Fatalf("BuildCamt053: %v", err)
	}
	second, err := BuildCamt053(statementParams())
	if err != nil {
		t.Fatalf("BuildCamt053: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("two builds of identical input diverged")
	}
	if !strings.HasPrefix(string(first), xml.Header) {
		t.Fatal("statement missing XML declaration")
	}
}

func TestBuildCamt053FailsClosed(t *testing.T) {
	params := statementParams()
	params.MessageID = ""
	if _, err := BuildCamt053(params); err == nil {
		t.Fatal("missing message id must fail")
	}
	params = statementParams()
	params.PeriodEnd = params.PeriodStart.AddDate(0, 0, -1)
	if _, err := BuildCamt053(params); err == nil {
		t.Fatal("inverted period must fail")
	}
	params = statementParams()
	params.Entries[0].Direction = "SIDEWAYS"
	if _, err := BuildCamt053(params); err == nil {
		t.Fatal("invalid direction must fail")
	}
	params = statementParams()
	params.Entries[0].AmountMinor = 0
	if _, err := BuildCamt053(params); err == nil {
		t.Fatal("zero amount must fail")
	}
}

// pain001Probe mirrors the CstmrCdtTrfInitn hierarchy for round-trip checks.
type pain001Probe struct {
	XMLName xml.Name `xml:"Document"`
	Xmlns   string   `xml:"xmlns,attr"`
	Initn   struct {
		GrpHdr struct {
			MsgId   string `xml:"MsgId"`
			NbOfTxs int    `xml:"NbOfTxs"`
			CtrlSum string `xml:"CtrlSum"`
		} `xml:"GrpHdr"`
		PmtInf struct {
			PmtInfId    string `xml:"PmtInfId"`
			PmtMtd      string `xml:"PmtMtd"`
			ReqdExctnDt string `xml:"ReqdExctnDt>Dt"`
			DbtrAcct    string `xml:"DbtrAcct>Id>Othr>Id"`
			Tx          []struct {
				InstrId  string `xml:"PmtId>InstrId"`
				InstdAmt struct {
					Value    string `xml:",chardata"`
					Currency string `xml:"Ccy,attr"`
				} `xml:"Amt>InstdAmt"`
				Creditor  string `xml:"Cdtr>Nm"`
				CredAcct  string `xml:"CdtrAcct>Id>Othr>Id"`
				Narrative string `xml:"RmtInf>Ustrd"`
			} `xml:"CdtTrfTxInf"`
		} `xml:"PmtInf"`
	} `xml:"CstmrCdtTrfInitn"`
}

func pain001Params() Pain001Params {
	return Pain001Params{
		MessageID:     "MSG-PAIN-1",
		PaymentInfoID: "PMT-PAIN-1",
		DebtorName:    "Federation Treasury Single Account",
		DebtorAcct:    "TSA:NGN",
		DebtorBIC:     "NIBONGNGLAG",
		Instructions: []CreditInstruction{
			{
				InstructionID: "CVFF-APP-1", AmountMinor: 150000, Currency: "NGN",
				CreditorName: "BENEF-1", CreditorAcct: "BANK-PRINCIPAL-1",
				Narrative:     "CVFF disbursement EXT-1",
				ExecutionDate: time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
			},
			{
				InstructionID: "CVFF-APP-2", AmountMinor: 50000, Currency: "NGN",
				CreditorName: "BENEF-2", CreditorAcct: "BANK-PRINCIPAL-2",
				Narrative:     "CVFF disbursement EXT-2",
				ExecutionDate: time.Date(2026, 1, 22, 0, 0, 0, 0, time.UTC),
			},
		},
		GeneratedAt: time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC),
	}
}

func TestBuildPain001RoundTripsThroughISO20022Structure(t *testing.T) {
	raw, err := BuildPain001(pain001Params())
	if err != nil {
		t.Fatalf("BuildPain001: %v", err)
	}
	var probe pain001Probe
	if err := xml.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("payment initiation does not round-trip through the pain.001 structure: %v", err)
	}
	if probe.Xmlns != Pain001Namespace {
		t.Fatalf("namespace = %q, want %q", probe.Xmlns, Pain001Namespace)
	}
	if probe.Initn.GrpHdr.NbOfTxs != 2 || probe.Initn.GrpHdr.CtrlSum != "2000.00" {
		t.Fatalf("group header = %+v", probe.Initn.GrpHdr)
	}
	payment := probe.Initn.PmtInf
	if payment.PmtMtd != "TRF" || payment.DbtrAcct != "TSA:NGN" {
		t.Fatalf("payment info = %+v", payment)
	}
	if payment.ReqdExctnDt != "2026-01-22" {
		t.Fatalf("requested execution date = %q, want latest instruction date", payment.ReqdExctnDt)
	}
	if len(payment.Tx) != 2 {
		t.Fatalf("transactions = %d, want 2", len(payment.Tx))
	}
	if payment.Tx[0].InstrId != "CVFF-APP-1" || payment.Tx[0].InstdAmt.Value != "1500.00" || payment.Tx[0].InstdAmt.Currency != "NGN" {
		t.Fatalf("unexpected first transaction: %+v", payment.Tx[0])
	}
	if payment.Tx[1].CredAcct != "BANK-PRINCIPAL-2" {
		t.Fatalf("unexpected second transaction: %+v", payment.Tx[1])
	}
}

func TestBuildPain001IsDeterministic(t *testing.T) {
	first, err := BuildPain001(pain001Params())
	if err != nil {
		t.Fatalf("BuildPain001: %v", err)
	}
	second, err := BuildPain001(pain001Params())
	if err != nil {
		t.Fatalf("BuildPain001: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("two builds of identical input diverged")
	}
}

func TestBuildPain001FailsClosed(t *testing.T) {
	params := pain001Params()
	params.Instructions = nil
	if _, err := BuildPain001(params); err == nil {
		t.Fatal("empty instruction set must fail")
	}
	params = pain001Params()
	params.Instructions[1].Currency = "USD"
	if _, err := BuildPain001(params); err == nil {
		t.Fatal("mixed-currency batch must fail")
	}
	params = pain001Params()
	params.Instructions[0].CreditorAcct = ""
	if _, err := BuildPain001(params); err == nil {
		t.Fatal("missing creditor account must fail")
	}
}
