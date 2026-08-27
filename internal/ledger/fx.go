package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/munisp/blueeconomy-financial-controls/internal/fx"
	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

// DisbursementPairInput describes the FX pass-through ledger entries for one
// CVFF disbursement: an NGN custodial fee pair and a USD-denominated cost pair
// valued with the dual-control-confirmed CBN reference rate.
type DisbursementPairInput struct {
	ApplicationID      string
	FeeNGNMinor        uint64
	CostUSDMinor       uint64
	Rate               fx.Rate
	NGNDebitAccountID  tigerbeetle.Uint128
	NGNCreditAccountID tigerbeetle.Uint128
	USDDebitAccountID  tigerbeetle.Uint128
	USDCreditAccountID tigerbeetle.Uint128
	NGNLedger          uint32
	USDLedger          uint32
	FeeCode            uint16
	CostCode           uint16
}

// DisbursementLegs records both ledger legs plus the FX-adjusted NGN
// equivalent of the USD cost for the dual-ledger report.
type DisbursementLegs struct {
	ApplicationID     string
	RateID            string
	NGNPerUSDMicro    uint64
	FeeTransferID     tigerbeetle.Uint128
	CostTransferID    tigerbeetle.Uint128
	FeeNGNMinor       uint64
	CostUSDMinor      uint64
	CostNGNEquivalent uint64
}

// BuildDisbursementPair constructs the two deterministic TigerBeetle transfers
// without touching the cluster. It fails closed on invalid input or FX
// overflow before any ledger write is attempted.
func BuildDisbursementPair(input DisbursementPairInput) ([]tigerbeetle.Transfer, DisbursementLegs, error) {
	if input.ApplicationID == "" {
		return nil, DisbursementLegs{}, errors.New("application ID is required")
	}
	if input.NGNLedger == 0 || input.USDLedger == 0 || input.FeeCode == 0 || input.CostCode == 0 {
		return nil, DisbursementLegs{}, errors.New("disbursement ledgers and codes must be non-zero")
	}
	equivalent, err := input.Rate.ConvertUSDToNGN(input.CostUSDMinor)
	if err != nil {
		return nil, DisbursementLegs{}, fmt.Errorf("fx-adjust USD cost: %w", err)
	}
	if input.FeeNGNMinor == 0 {
		return nil, DisbursementLegs{}, errors.New("NGN custodial fee must be non-zero")
	}
	feeID := disbursementTransferID("cvff-fee-ngn", input.ApplicationID)
	costID := disbursementTransferID("cvff-cost-usd", input.ApplicationID)
	if err := validateTransfer(feeID, input.NGNDebitAccountID, input.NGNCreditAccountID, input.FeeNGNMinor); err != nil {
		return nil, DisbursementLegs{}, fmt.Errorf("NGN custodial fee pair: %w", err)
	}
	if err := validateTransfer(costID, input.USDDebitAccountID, input.USDCreditAccountID, input.CostUSDMinor); err != nil {
		return nil, DisbursementLegs{}, fmt.Errorf("USD cost pair: %w", err)
	}
	transfers := []tigerbeetle.Transfer{
		{
			ID:              feeID,
			DebitAccountID:  input.NGNDebitAccountID,
			CreditAccountID: input.NGNCreditAccountID,
			Amount:          tigerbeetle.ToUint128(input.FeeNGNMinor),
			Ledger:          input.NGNLedger,
			Code:            input.FeeCode,
		},
		{
			ID:              costID,
			DebitAccountID:  input.USDDebitAccountID,
			CreditAccountID: input.USDCreditAccountID,
			Amount:          tigerbeetle.ToUint128(input.CostUSDMinor),
			Ledger:          input.USDLedger,
			Code:            input.CostCode,
		},
	}
	legs := DisbursementLegs{
		ApplicationID:     input.ApplicationID,
		RateID:            input.Rate.RateID,
		NGNPerUSDMicro:    input.Rate.NGNPerUSDMicro,
		FeeTransferID:     feeID,
		CostTransferID:    costID,
		FeeNGNMinor:       input.FeeNGNMinor,
		CostUSDMinor:      input.CostUSDMinor,
		CostNGNEquivalent: equivalent,
	}
	return transfers, legs, nil
}

// CreateDisbursementPair posts both transfers. Deterministic transfer IDs make
// retries idempotent at the TigerBeetle boundary.
func (service *Service) CreateDisbursementPair(input DisbursementPairInput) (DisbursementLegs, error) {
	transfers, legs, err := BuildDisbursementPair(input)
	if err != nil {
		return DisbursementLegs{}, err
	}
	results, err := service.client.CreateTransfers(transfers)
	if err != nil {
		return DisbursementLegs{}, fmt.Errorf("create TigerBeetle disbursement pair: %w", err)
	}
	if len(results) != len(transfers) {
		return DisbursementLegs{}, fmt.Errorf("create TigerBeetle disbursement pair returned %d results, want %d", len(results), len(transfers))
	}
	for index, result := range results {
		if result.Status != tigerbeetle.TransferCreated {
			return DisbursementLegs{}, fmt.Errorf("create TigerBeetle disbursement transfer %d returned status %v", index, result.Status)
		}
	}
	return legs, nil
}

// disbursementTransferID derives a deterministic idempotent transfer ID.
func disbursementTransferID(namespace, applicationID string) tigerbeetle.Uint128 {
	digest := sha256.Sum256([]byte(namespace + ":" + applicationID))
	id, err := tigerbeetle.HexStringToUint128(hex.EncodeToString(digest[:16]))
	if err != nil {
		panic(fmt.Sprintf("deterministic TigerBeetle ID encoding failed: %v", err))
	}
	return id
}
