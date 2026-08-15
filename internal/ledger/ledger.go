package ledger

import (
	"errors"
	"fmt"

	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

type client interface {
	CreateAccounts([]tigerbeetle.Account) ([]tigerbeetle.CreateAccountResult, error)
	CreateTransfers([]tigerbeetle.Transfer) ([]tigerbeetle.CreateTransferResult, error)
	LookupTransfers([]tigerbeetle.Uint128) ([]tigerbeetle.Transfer, error)
}

type Service struct {
	client client
	ledger uint32
	code   uint16
}

func New(client client, ledger uint32, code uint16) (*Service, error) {
	if client == nil {
		return nil, errors.New("TigerBeetle client is required")
	}
	if ledger == 0 {
		return nil, errors.New("ledger must be non-zero")
	}
	if code == 0 {
		return nil, errors.New("transfer/account code must be non-zero")
	}
	return &Service{client: client, ledger: ledger, code: code}, nil
}

func (service *Service) CreateAccount(id tigerbeetle.Uint128, history bool) error {
	if zero(id) {
		return errors.New("account ID must be non-zero")
	}
	flags := tigerbeetle.AccountFlags{History: history}.ToUint16()
	results, err := service.client.CreateAccounts([]tigerbeetle.Account{{
		ID:     id,
		Ledger: service.ledger,
		Code:   service.code,
		Flags:  flags,
	}})
	if err != nil {
		return fmt.Errorf("create TigerBeetle account: %w", err)
	}
	if len(results) != 1 {
		return fmt.Errorf("create TigerBeetle account returned %d results, want 1", len(results))
	}
	if results[0].Status != tigerbeetle.AccountCreated && results[0].Status != tigerbeetle.AccountExists {
		return fmt.Errorf("create TigerBeetle account returned status %v", results[0].Status)
	}
	return nil
}

func (service *Service) Reserve(transferID, debitAccountID, creditAccountID tigerbeetle.Uint128, amount uint64, timeout uint32) error {
	if err := validateTransfer(transferID, debitAccountID, creditAccountID, amount); err != nil {
		return err
	}
	if timeout == 0 {
		return errors.New("pending transfer timeout must be non-zero")
	}
	return service.createTransfer(tigerbeetle.Transfer{
		ID:              transferID,
		DebitAccountID:  debitAccountID,
		CreditAccountID: creditAccountID,
		Amount:          tigerbeetle.ToUint128(amount),
		Timeout:         timeout,
		Ledger:          service.ledger,
		Code:            service.code,
		Flags:           tigerbeetle.TransferFlags{Pending: true}.ToUint16(),
	})
}

func (service *Service) Post(postTransferID, pendingTransferID tigerbeetle.Uint128) error {
	if zero(postTransferID) || zero(pendingTransferID) {
		return errors.New("post and pending transfer IDs must be non-zero")
	}
	return service.createTransfer(tigerbeetle.Transfer{
		ID:        postTransferID,
		PendingID: pendingTransferID,
		Ledger:    service.ledger,
		Code:      service.code,
		Flags:     tigerbeetle.TransferFlags{PostPendingTransfer: true}.ToUint16(),
	})
}

func (service *Service) Void(voidTransferID, pendingTransferID tigerbeetle.Uint128) error {
	if zero(voidTransferID) || zero(pendingTransferID) {
		return errors.New("void and pending transfer IDs must be non-zero")
	}
	return service.createTransfer(tigerbeetle.Transfer{
		ID:        voidTransferID,
		PendingID: pendingTransferID,
		Ledger:    service.ledger,
		Code:      service.code,
		Flags:     tigerbeetle.TransferFlags{VoidPendingTransfer: true}.ToUint16(),
	})
}

func (service *Service) LookupTransfer(id tigerbeetle.Uint128) (tigerbeetle.Transfer, bool, error) {
	if zero(id) {
		return tigerbeetle.Transfer{}, false, errors.New("transfer ID must be non-zero")
	}
	transfers, err := service.client.LookupTransfers([]tigerbeetle.Uint128{id})
	if err != nil {
		return tigerbeetle.Transfer{}, false, fmt.Errorf("lookup TigerBeetle transfer: %w", err)
	}
	if len(transfers) == 0 {
		return tigerbeetle.Transfer{}, false, nil
	}
	if len(transfers) != 1 {
		return tigerbeetle.Transfer{}, false, fmt.Errorf("lookup TigerBeetle transfer returned %d records, want at most 1", len(transfers))
	}
	return transfers[0], true, nil
}

func (service *Service) createTransfer(transfer tigerbeetle.Transfer) error {
	results, err := service.client.CreateTransfers([]tigerbeetle.Transfer{transfer})
	if err != nil {
		return fmt.Errorf("create TigerBeetle transfer: %w", err)
	}
	if len(results) != 1 {
		return fmt.Errorf("create TigerBeetle transfer returned %d results, want 1", len(results))
	}
	if results[0].Status != tigerbeetle.TransferCreated {
		return fmt.Errorf("create TigerBeetle transfer returned status %v", results[0].Status)
	}
	return nil
}

func validateTransfer(transferID, debitAccountID, creditAccountID tigerbeetle.Uint128, amount uint64) error {
	if zero(transferID) || zero(debitAccountID) || zero(creditAccountID) {
		return errors.New("transfer and account IDs must be non-zero")
	}
	if debitAccountID == creditAccountID {
		return errors.New("debit and credit accounts must differ")
	}
	if amount == 0 {
		return errors.New("transfer amount must be non-zero")
	}
	return nil
}

func zero(id tigerbeetle.Uint128) bool {
	return id == (tigerbeetle.Uint128{})
}
