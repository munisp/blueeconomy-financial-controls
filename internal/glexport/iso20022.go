// Package glexport implements the feed-based ERP integration layer: it turns
// the platform's authoritative postings (settlement collections and approved
// disbursement instructions, mirrored from the signed ledger) into ISO 20022
// XML feeds that external ERPs (SAP, Dynamics, etc.) consume. Integration is
// strictly feed-based — no ERP component is embedded in this service.
//
// Two message families are produced:
//   - camt.053.001.08  Bank-to-Customer Statement (per GL account, per period)
//   - pain.001.001.09  Customer Credit Transfer Initiation (approved
//     disbursement/refund instructions)
//
// Every export is deterministic (stable ordering, fixed amount rendering) and
// recorded in export_batches with the payload hash, the exporting subject and
// a signed envelope v1.0 (JWS-EdDSA over the JCS payload) so downstream ERPs
// and auditors -- can verify authenticity end to end.
package glexport

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ISO 20022 message namespaces emitted by this package.
const (
	Camt053Namespace = "urn:iso:std:iso:20022:tech:xsd:camt.053.001.08"
	Pain001Namespace = "urn:iso:std:iso:20022:tech:xsd:pain.001.001.09"
)

// Direction of a journal posting leg.
const (
	DirectionDebit  = "DEBIT"
	DirectionCredit = "CREDIT"
)

// JournalEntry is one double-entry leg of the authoritative journal. Entries
// are derived from the platform's settlement and disbursement records; money
// is always minor units with a 2-decimal currency exponent.
type JournalEntry struct {
	EntryID      string    `json:"entry_id"`
	Account      string    `json:"account"`
	Direction    string    `json:"direction"` // DEBIT | CREDIT
	AmountMinor  int64     `json:"amount_minor"`
	Currency     string    `json:"currency"`
	ValueDate    time.Time `json:"value_date"`
	BookingDate  time.Time `json:"booking_date"`
	Reference    string    `json:"reference"` // end-to-end / bank reference
	Counterparty string    `json:"counterparty"`
	Narrative    string    `json:"narrative"`
}

// CreditInstruction is one approved disbursement/refund instruction exported
// as a pain.001 credit-transfer transaction.
type CreditInstruction struct {
	InstructionID string    `json:"instruction_id"`
	AmountMinor   int64     `json:"amount_minor"`
	Currency      string    `json:"currency"`
	CreditorName  string    `json:"creditor_name"`
	CreditorAcct  string    `json:"creditor_account"`
	Narrative     string    `json:"narrative"`
	ExecutionDate time.Time `json:"execution_date"`
}

// Amount renders minor units as the ISO 20022 decimal major-unit string with
// exactly two fraction digits (e.g. 123456 -> "1234.56"). Deterministic.
func Amount(amountMinor int64) string {
	sign := ""
	value := amountMinor
	if value < 0 {
		sign = "-"
		value = -value
	}
	return fmt.Sprintf("%s%d.%02d", sign, value/100, value%100)
}

func xmlDate(t time.Time) string { return t.UTC().Format("2006-01-02") }
func xmlDateTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// ---------------------------------------------------------------------------
// camt.053.001.08 — Bank-to-Customer Statement
// ---------------------------------------------------------------------------

type camtAmount struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type camtDate struct {
	Dt string `xml:"Dt"`
}

type camtBalance struct {
	Tp struct {
		CdOrPrtry struct {
			Cd string `xml:"Cd"`
		} `xml:"CdOrPrtry"`
	} `xml:"Tp"`
	Amt       camtAmount `xml:"Amt"`
	CdtDbtInd string     `xml:"CdtDbtInd"`
	Dt        camtDate   `xml:"Dt"`
}

type camtEntry struct {
	NtryRef   string     `xml:"NtryRef"`
	Amt       camtAmount `xml:"Amt"`
	CdtDbtInd string     `xml:"CdtDbtInd"`
	Sts       struct {
		Cd string `xml:"Cd"`
	} `xml:"Sts"`
	BookgDt     camtDate `xml:"BookgDt"`
	ValDt       camtDate `xml:"ValDt"`
	AcctSvcrRef string   `xml:"AcctSvcrRef"`
	NtryDtls    struct {
		TxDtls struct {
			Refs struct {
				EndToEndId string `xml:"EndToEndId"`
			} `xml:"Refs"`
			RltdPties struct {
				Dbtr struct {
					Nm string `xml:"Nm"`
				} `xml:"Dbtr"`
			} `xml:"RltdPties"`
			RmtInf struct {
				Ustrd string `xml:"Ustrd"`
			} `xml:"RmtInf"`
		} `xml:"TxDtls"`
	} `xml:"NtryDtls"`
}

type camtStatement struct {
	Id           string `xml:"Id"`
	ElctrncSeqNb int    `xml:"ElctrncSeqNb"`
	CreDtTm      string `xml:"CreDtTm"`
	FrToDt       struct {
		FrDtTm string `xml:"FrDtTm"`
		ToDtTm string `xml:"ToDtTm"`
	} `xml:"FrToDt"`
	Acct struct {
		Id struct {
			Othr struct {
				Id string `xml:"Id"`
			} `xml:"Othr"`
		} `xml:"Id"`
		Ccy  string `xml:"Ccy"`
		Ownr struct {
			Nm string `xml:"Nm"`
		} `xml:"Ownr"`
		Svcr struct {
			FinInstnId struct {
				BICFI string `xml:"BICFI"`
			} `xml:"FinInstnId"`
		} `xml:"Svcr"`
	} `xml:"Acct"`
	Bal  []camtBalance `xml:"Bal"`
	Ntry []camtEntry   `xml:"Ntry"`
}

type camtDocument struct {
	XMLName       xml.Name `xml:"Document"`
	Xmlns         string   `xml:"xmlns,attr"`
	BkToCstmrStmt struct {
		GrpHdr struct {
			MsgId   string `xml:"MsgId"`
			CreDtTm string `xml:"CreDtTm"`
		} `xml:"GrpHdr"`
		Stmt []camtStatement `xml:"Stmt"`
	} `xml:"BkToCstmrStmt"`
}

// Camt053Params carries everything needed to render one deterministic
// camt.053.001.08 statement for one GL account over one period.
type Camt053Params struct {
	MessageID    string
	StatementID  string
	Sequence     int
	AccountRef   string // GL account identifier (IBAN/othr id)
	AccountOwner string
	ServicerBIC  string
	Currency     string
	PeriodStart  time.Time
	PeriodEnd    time.Time
	OpeningMinor int64 // signed: positive = credit balance owed to owner
	Entries      []JournalEntry
	GeneratedAt  time.Time
}

// BuildCamt053 renders a camt.053.001.08 statement. Entries must already be
// scoped to the account/period and ordered deterministically by the caller;
// BuildCamt053 preserves that order. The closing balance is derived from the
// opening balance plus the signed entry amounts (fail-closed: entry direction
// must be DEBIT or CREDIT).
func BuildCamt053(params Camt053Params) ([]byte, error) {
	if strings.TrimSpace(params.MessageID) == "" || strings.TrimSpace(params.StatementID) == "" {
		return nil, errors.New("camt053 message and statement ids are required")
	}
	if strings.TrimSpace(params.AccountRef) == "" || strings.TrimSpace(params.Currency) == "" {
		return nil, errors.New("camt053 account and currency are required")
	}
	if params.PeriodEnd.Before(params.PeriodStart) {
		return nil, errors.New("camt053 period end precedes period start")
	}

	document := camtDocument{Xmlns: Camt053Namespace}
	document.BkToCstmrStmt.GrpHdr.MsgId = params.MessageID
	document.BkToCstmrStmt.GrpHdr.CreDtTm = xmlDateTime(params.GeneratedAt)

	statement := camtStatement{Id: params.StatementID, ElctrncSeqNb: params.Sequence}
	statement.CreDtTm = xmlDateTime(params.GeneratedAt)
	statement.FrToDt.FrDtTm = xmlDateTime(params.PeriodStart)
	statement.FrToDt.ToDtTm = xmlDateTime(params.PeriodEnd)
	statement.Acct.Id.Othr.Id = params.AccountRef
	statement.Acct.Ccy = params.Currency
	statement.Acct.Ownr.Nm = params.AccountOwner
	statement.Acct.Svcr.FinInstnId.BICFI = params.ServicerBIC

	closing := params.OpeningMinor
	statement.Ntry = make([]camtEntry, 0, len(params.Entries))
	for _, entry := range params.Entries {
		if entry.AmountMinor <= 0 {
			return nil, fmt.Errorf("camt053 entry %q amount must be positive minor units", entry.EntryID)
		}
		// Statement direction is from the account owner's perspective: the
		// exported accounts are cash assets, so a journal DEBIT to the account
		// is money in (CRDT) and a journal CREDIT is money out (DBIT).
		var creditDebit string
		switch entry.Direction {
		case DirectionDebit:
			creditDebit = "CRDT"
			closing += entry.AmountMinor
		case DirectionCredit:
			creditDebit = "DBIT"
			closing -= entry.AmountMinor
		default:
			return nil, fmt.Errorf("camt053 entry %q has invalid direction %q", entry.EntryID, entry.Direction)
		}
		rendered := camtEntry{
			NtryRef:     entry.EntryID,
			Amt:         camtAmount{Ccy: params.Currency, Value: Amount(entry.AmountMinor)},
			CdtDbtInd:   creditDebit,
			BookgDt:     camtDate{Dt: xmlDate(entry.BookingDate)},
			ValDt:       camtDate{Dt: xmlDate(entry.ValueDate)},
			AcctSvcrRef: entry.Reference,
		}
		rendered.Sts.Cd = "BOOK"
		rendered.NtryDtls.TxDtls.Refs.EndToEndId = entry.Reference
		rendered.NtryDtls.TxDtls.RltdPties.Dbtr.Nm = entry.Counterparty
		rendered.NtryDtls.TxDtls.RmtInf.Ustrd = entry.Narrative
		statement.Ntry = append(statement.Ntry, rendered)
	}

	statement.Bal = []camtBalance{
		balance("OPBD", params.OpeningMinor, params.Currency, params.PeriodStart),
		balance("CLBD", closing, params.Currency, params.PeriodEnd),
	}
	document.BkToCstmrStmt.Stmt = []camtStatement{statement}

	raw, err := xml.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal camt053 document: %w", err)
	}
	return append([]byte(xml.Header), raw...), nil
}

func balance(code string, amountMinor int64, currency string, date time.Time) camtBalance {
	rendered := camtBalance{
		Amt: camtAmount{Ccy: currency, Value: Amount(abs64(amountMinor))},
		Dt:  camtDate{Dt: xmlDate(date)},
	}
	rendered.Tp.CdOrPrtry.Cd = code
	if amountMinor >= 0 {
		rendered.CdtDbtInd = "CRDT"
	} else {
		rendered.CdtDbtInd = "DBIT"
	}
	return rendered
}

func abs64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// ---------------------------------------------------------------------------
// pain.001.001.09 — Customer Credit Transfer Initiation
// ---------------------------------------------------------------------------

type painParty struct {
	Nm string `xml:"Nm"`
}

type painAccount struct {
	Id struct {
		Othr struct {
			Id string `xml:"Id"`
		} `xml:"Othr"`
	} `xml:"Id"`
}

type painAgent struct {
	FinInstnId struct {
		BICFI string `xml:"BICFI"`
	} `xml:"FinInstnId"`
}

type painTransaction struct {
	PmtId struct {
		InstrId    string `xml:"InstrId"`
		EndToEndId string `xml:"EndToEndId"`
	} `xml:"PmtId"`
	Amt struct {
		InstdAmt camtAmount `xml:"InstdAmt"`
	} `xml:"Amt"`
	Cdtr     painParty   `xml:"Cdtr"`
	CdtrAcct painAccount `xml:"CdtrAcct"`
	RmtInf   struct {
		Ustrd string `xml:"Ustrd"`
	} `xml:"RmtInf"`
}

type painPaymentInfo struct {
	PmtInfId    string            `xml:"PmtInfId"`
	PmtMtd      string            `xml:"PmtMtd"`
	BtchBookg   bool              `xml:"BtchBookg"`
	NbOfTxs     int               `xml:"NbOfTxs"`
	CtrlSum     string            `xml:"CtrlSum"`
	ReqdExctnDt camtDate          `xml:"ReqdExctnDt"`
	Dbtr        painParty         `xml:"Dbtr"`
	DbtrAcct    painAccount       `xml:"DbtrAcct"`
	DbtrAgt     painAgent         `xml:"DbtrAgt"`
	CdtTrfTxInf []painTransaction `xml:"CdtTrfTxInf"`
}

type painDocument struct {
	XMLName          xml.Name `xml:"Document"`
	Xmlns            string   `xml:"xmlns,attr"`
	CstmrCdtTrfInitn struct {
		GrpHdr struct {
			MsgId    string    `xml:"MsgId"`
			CreDtTm  string    `xml:"CreDtTm"`
			NbOfTxs  int       `xml:"NbOfTxs"`
			CtrlSum  string    `xml:"CtrlSum"`
			InitgPty painParty `xml:"InitgPty"`
		} `xml:"GrpHdr"`
		PmtInf []painPaymentInfo `xml:"PmtInf"`
	} `xml:"CstmrCdtTrfInitn"`
}

// Pain001Params carries everything needed to render one deterministic
// pain.001.001.09 credit-transfer initiation message.
type Pain001Params struct {
	MessageID     string
	PaymentInfoID string
	DebtorName    string
	DebtorAcct    string
	DebtorBIC     string
	Instructions  []CreditInstruction
	GeneratedAt   time.Time
}

// BuildPain001 renders a pain.001.001.09 message. Instructions must share one
// currency (one PmtInf per message keeps the control sum unambiguous) and be
// pre-ordered deterministically by the caller.
func BuildPain001(params Pain001Params) ([]byte, error) {
	if strings.TrimSpace(params.MessageID) == "" || strings.TrimSpace(params.PaymentInfoID) == "" {
		return nil, errors.New("pain001 message and payment-info ids are required")
	}
	if len(params.Instructions) == 0 {
		return nil, errors.New("pain001 requires at least one credit instruction")
	}
	currency := params.Instructions[0].Currency
	if strings.TrimSpace(currency) == "" {
		return nil, errors.New("pain001 instruction currency is required")
	}

	document := painDocument{Xmlns: Pain001Namespace}
	document.CstmrCdtTrfInitn.GrpHdr.MsgId = params.MessageID
	document.CstmrCdtTrfInitn.GrpHdr.CreDtTm = xmlDateTime(params.GeneratedAt)
	document.CstmrCdtTrfInitn.GrpHdr.NbOfTxs = len(params.Instructions)
	document.CstmrCdtTrfInitn.GrpHdr.InitgPty = painParty{Nm: params.DebtorName}

	payment := painPaymentInfo{
		PmtInfId:  params.PaymentInfoID,
		PmtMtd:    "TRF",
		BtchBookg: true,
		NbOfTxs:   len(params.Instructions),
		Dbtr:      painParty{Nm: params.DebtorName},
		DbtrAgt:   painAgent{},
	}
	payment.DbtrAcct.Id.Othr.Id = params.DebtorAcct
	payment.DbtrAgt.FinInstnId.BICFI = params.DebtorBIC

	var totalMinor int64
	var latestExecution time.Time
	payment.CdtTrfTxInf = make([]painTransaction, 0, len(params.Instructions))
	for _, instruction := range params.Instructions {
		if instruction.AmountMinor <= 0 {
			return nil, fmt.Errorf("pain001 instruction %q amount must be positive minor units", instruction.InstructionID)
		}
		if instruction.Currency != currency {
			return nil, fmt.Errorf("pain001 instruction %q currency %q differs from batch currency %q", instruction.InstructionID, instruction.Currency, currency)
		}
		if strings.TrimSpace(instruction.CreditorName) == "" || strings.TrimSpace(instruction.CreditorAcct) == "" {
			return nil, fmt.Errorf("pain001 instruction %q creditor name/account is required", instruction.InstructionID)
		}
		totalMinor += instruction.AmountMinor
		if instruction.ExecutionDate.After(latestExecution) {
			latestExecution = instruction.ExecutionDate
		}
		transaction := painTransaction{
			Cdtr:     painParty{Nm: instruction.CreditorName},
			CdtrAcct: painAccount{},
		}
		transaction.PmtId.InstrId = instruction.InstructionID
		transaction.PmtId.EndToEndId = instruction.InstructionID
		transaction.Amt.InstdAmt = camtAmount{Ccy: currency, Value: Amount(instruction.AmountMinor)}
		transaction.CdtrAcct.Id.Othr.Id = instruction.CreditorAcct
		transaction.RmtInf.Ustrd = instruction.Narrative
		payment.CdtTrfTxInf = append(payment.CdtTrfTxInf, transaction)
	}
	controlSum := Amount(totalMinor)
	payment.CtrlSum = controlSum
	payment.ReqdExctnDt = camtDate{Dt: xmlDate(latestExecution)}
	document.CstmrCdtTrfInitn.GrpHdr.CtrlSum = controlSum
	document.CstmrCdtTrfInitn.PmtInf = []painPaymentInfo{payment}

	raw, err := xml.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal pain001 document: %w", err)
	}
	return append([]byte(xml.Header), raw...), nil
}
