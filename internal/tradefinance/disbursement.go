package tradefinance

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"

	"github.com/munisp/blueeconomy-financial-controls/internal/ledger"
)

// DisbursementLedger posts the TigerBeetle double-entry of the trade-finance
// disbursement lifecycle on top of the shared ledger.Service: a pending
// reservation when the application is APPROVED, the post when the treasury
// officer disburses, and the settlement post when the trader repays.
// Transfer and account IDs are deterministic functions of the application
// id so every create is idempotent and replay-safe.
type DisbursementLedger struct {
	service *ledger.Service
	// ledgerTimeout bounds the pending reservation.
	ledgerTimeout uint32
}

// NewDisbursementLedger fails closed without a ledger service or timeout.
func NewDisbursementLedger(service *ledger.Service, pendingTimeoutSeconds uint32) (*DisbursementLedger, error) {
	if service == nil {
		return nil, errors.New("TigerBeetle ledger service is required")
	}
	if pendingTimeoutSeconds == 0 {
		return nil, errors.New("pending transfer timeout must be non-zero")
	}
	return &DisbursementLedger{service: service, ledgerTimeout: pendingTimeoutSeconds}, nil
}

// deterministicID derives a non-zero Uint128 from domain parts.
func deterministicID(parts ...string) (tigerbeetle.Uint128, error) {
	for _, part := range parts {
		if part == "" {
			return tigerbeetle.Uint128{}, errors.New("deterministic id parts must be non-empty")
		}
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s", parts)))
	if binary.LittleEndian.Uint64(digest[0:8]) == 0 && binary.LittleEndian.Uint64(digest[8:16]) == 0 {
		return tigerbeetle.Uint128{}, errors.New("deterministic id collision with zero")
	}
	var raw [16]byte
	copy(raw[:], digest[0:16])
	return tigerbeetle.BytesToUint128(raw), nil
}

// AccountID derives the deterministic ledger account id for one role account.
func AccountID(applicationID, kind string) (tigerbeetle.Uint128, error) {
	return deterministicID("tf-account", applicationID, kind)
}

// ReserveTransferID derives the deterministic pending reservation id.
func ReserveTransferID(applicationID string) (tigerbeetle.Uint128, error) {
	return deterministicID("tf-reserve", applicationID)
}

// DisburseTransferID derives the deterministic disbursement post id.
func DisburseTransferID(applicationID string) (tigerbeetle.Uint128, error) {
	return deterministicID("tf-disburse", applicationID)
}

// SettleTransferID derives the deterministic settlement post id.
func SettleTransferID(applicationID string) (tigerbeetle.Uint128, error) {
	return deterministicID("tf-settle", applicationID)
}

// EnsureAccounts creates the facility (bank) and settlement (trader)
// accounts with history. Existing identical accounts are idempotent success.
func (disbursement *DisbursementLedger) EnsureAccounts(facilityAccountID, settlementAccountID tigerbeetle.Uint128) error {
	if err := disbursement.service.CreateAccount(facilityAccountID, true); err != nil {
		return fmt.Errorf("create facility account: %w", err)
	}
	if err := disbursement.service.CreateAccount(settlementAccountID, true); err != nil {
		return fmt.Errorf("create settlement account: %w", err)
	}
	return nil
}

// Reserve places the pending double-entry facility reservation
// (debit facility account, credit settlement account).
func (disbursement *DisbursementLedger) Reserve(applicationID string, facilityAccountID, settlementAccountID tigerbeetle.Uint128, amount uint64) (tigerbeetle.Uint128, error) {
	transferID, err := ReserveTransferID(applicationID)
	if err != nil {
		return tigerbeetle.Uint128{}, err
	}
	if err := disbursement.service.Reserve(transferID, facilityAccountID, settlementAccountID, amount, disbursement.ledgerTimeout); err != nil {
		return tigerbeetle.Uint128{}, fmt.Errorf("reserve facility: %w", err)
	}
	return transferID, nil
}

// Disburse posts the reservation: the funds move to the trader settlement
// account. Returns the deterministic post transfer id.
func (disbursement *DisbursementLedger) Disburse(applicationID string) (tigerbeetle.Uint128, error) {
	reserveID, err := ReserveTransferID(applicationID)
	if err != nil {
		return tigerbeetle.Uint128{}, err
	}
	postID, err := DisburseTransferID(applicationID)
	if err != nil {
		return tigerbeetle.Uint128{}, err
	}
	if err := disbursement.service.Post(postID, reserveID); err != nil {
		return tigerbeetle.Uint128{}, fmt.Errorf("post disbursement: %w", err)
	}
	return postID, nil
}

// Settle posts the repayment double-entry (debit settlement account, credit
// facility account) as a fresh non-pending transfer.
func (disbursement *DisbursementLedger) Settle(applicationID string, settlementAccountID, facilityAccountID tigerbeetle.Uint128, amount uint64) (tigerbeetle.Uint128, error) {
	settleID, err := SettleTransferID(applicationID)
	if err != nil {
		return tigerbeetle.Uint128{}, err
	}
	if err := disbursement.service.Reserve(settleID, settlementAccountID, facilityAccountID, amount, disbursement.ledgerTimeout); err != nil {
		return tigerbeetle.Uint128{}, fmt.Errorf("reserve settlement: %w", err)
	}
	postID, err := deterministicID("tf-settle-post", applicationID)
	if err != nil {
		return tigerbeetle.Uint128{}, err
	}
	if err := disbursement.service.Post(postID, settleID); err != nil {
		return tigerbeetle.Uint128{}, fmt.Errorf("post settlement: %w", err)
	}
	return settleID, nil
}

// Void releases the reservation when the application is DECLINED after
// approval (fail-closed release: the funds return to the facility account).
func (disbursement *DisbursementLedger) Void(applicationID string) error {
	reserveID, err := ReserveTransferID(applicationID)
	if err != nil {
		return err
	}
	voidID, err := deterministicID("tf-void", applicationID)
	if err != nil {
		return err
	}
	if err := disbursement.service.Void(voidID, reserveID); err != nil {
		return fmt.Errorf("void reservation: %w", err)
	}
	return nil
}
