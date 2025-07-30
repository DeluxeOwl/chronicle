package main

import (
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
)

type TransferID string

func (t TransferID) String() string { return string(t) }

type TransferStatus string

const (
	TransferStatusStarted      TransferStatus = "started"
	TransferStatusWithdrawing  TransferStatus = "withdrawing"
	TransferStatusDepositing   TransferStatus = "depositing"
	TransferStatusCompensating TransferStatus = "compensating"
	TransferStatusCompleted    TransferStatus = "completed"
	TransferStatusFailed       TransferStatus = "failed"
)

type Transfer struct {
	aggregate.Base
	id            TransferID
	fromAccountID AccountID
	toAccountID   AccountID
	amount        int
	status        TransferStatus
	failureReason string
}

//sumtype:decl
type TransferEvent interface {
	event.Any
	isTransferEvent()
}

type TransferStarted struct {
	ID            TransferID `json:"id"`
	FromAccountID AccountID  `json:"fromAccountID"`
	ToAccountID   AccountID  `json:"toAccountID"`
	Amount        int        `json:"amount"`
}

func (*TransferStarted) EventName() string { return "transfer/started" }
func (*TransferStarted) isTransferEvent()  {}

type WithdrawalConfirmed struct{}

func (*WithdrawalConfirmed) EventName() string { return "transfer/withdrawal-confirmed" }
func (*WithdrawalConfirmed) isTransferEvent()  {}

type DepositConfirmed struct{}

func (*DepositConfirmed) EventName() string { return "transfer/deposit-confirmed" }
func (*DepositConfirmed) isTransferEvent()  {}

type TransferFailed struct {
	Reason string `json:"reason"`
}

func (*TransferFailed) EventName() string { return "transfer/failed" }
func (*TransferFailed) isTransferEvent()  {}

type CompensationStarted struct{}

func (*CompensationStarted) EventName() string { return "transfer/compensation-started" }
func (*CompensationStarted) isTransferEvent()  {}

func (t *Transfer) EventFuncs() event.FuncsFor[TransferEvent] {
	return event.FuncsFor[TransferEvent]{
		func() TransferEvent { return new(TransferStarted) },
		func() TransferEvent { return new(WithdrawalConfirmed) },
		func() TransferEvent { return new(DepositConfirmed) },
		func() TransferEvent { return new(TransferFailed) },
		func() TransferEvent { return new(CompensationStarted) },
	}
}

func NewEmptyTransfer() *Transfer { return new(Transfer) }

func (t *Transfer) ID() TransferID { return t.id }

func (t *Transfer) Apply(evt TransferEvent) error {
	switch e := evt.(type) {
	case *TransferStarted:
		t.id = e.ID
		t.fromAccountID = e.FromAccountID
		t.toAccountID = e.ToAccountID
		t.amount = e.Amount
		t.status = TransferStatusStarted
	case *WithdrawalConfirmed:
		t.status = TransferStatusDepositing
	case *DepositConfirmed:
		t.status = TransferStatusCompleted
	case *TransferFailed:
		t.status = TransferStatusFailed
		t.failureReason = e.Reason
	case *CompensationStarted:
		t.status = TransferStatusCompensating
	default:
		return fmt.Errorf("unexpected event type: %T", evt)
	}
	return nil
}

func (t *Transfer) recordThat(event TransferEvent) error {
	return aggregate.RecordEvent(t, event)
}

func Start(id TransferID, from, to AccountID, amount int) (*Transfer, error) {
	if amount <= 0 {
		return nil, errors.New("amount must be positive")
	}
	if from == to {
		return nil, errors.New("from and to accounts cannot be the same")
	}

	t := NewEmptyTransfer()
	_ = t.recordThat(&TransferStarted{
		ID: id, FromAccountID: from, ToAccountID: to, Amount: amount,
	})
	return t, nil
}

func (t *Transfer) ConfirmWithdrawal() error {
	if t.status != TransferStatusStarted {
		return fmt.Errorf("cannot confirm withdrawal in state %s", t.status)
	}
	return t.recordThat(&WithdrawalConfirmed{})
}

func (t *Transfer) ConfirmDeposit() error {
	if t.status != TransferStatusDepositing {
		return fmt.Errorf("cannot confirm deposit in state %s", t.status)
	}
	return t.recordThat(&DepositConfirmed{})
}

func (t *Transfer) Fail(reason string) error {
	if t.status == TransferStatusFailed || t.status == TransferStatusCompleted {
		return fmt.Errorf("cannot fail a transfer in terminal state %s", t.status)
	}
	return t.recordThat(&TransferFailed{Reason: reason})
}

func (t *Transfer) StartCompensation() error {
	if t.status != TransferStatusDepositing {
		return fmt.Errorf("cannot start compensation in state %s", t.status)
	}
	return t.recordThat(&CompensationStarted{})
}
