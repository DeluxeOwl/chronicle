package main

import (
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
)

type AccountID string

func (a AccountID) String() string { return string(a) }

type Account struct {
	aggregate.Base

	id      AccountID
	balance int
	opened  bool
}

func (a *Account) ID() AccountID {
	return a.id
}

func NewEmptyAccount() *Account {
	return new(Account)
}

func OpenAccount(id string, initialBalance int) (*Account, error) {
	if initialBalance < 0 {
		return nil, errors.New("initial balance cannot be negative")
	}
	acc := NewEmptyAccount()
	err := acc.recordThat(&AccountOpened{
		ID:             AccountID(id),
		InitialBalance: initialBalance,
	})
	if err != nil {
		return nil, fmt.Errorf("could not open account: %w", err)
	}
	return acc, nil
}

func (a *Account) Apply(evt AccountEvent) error {
	switch e := evt.(type) {
	case *AccountOpened:
		a.id = e.ID
		a.balance = e.InitialBalance
		a.opened = true
	case *MoneyWithdrawn:
		a.balance -= e.Amount
	case *MoneyDeposited:
		a.balance += e.Amount
	default:
		return fmt.Errorf("unexpected event type: %T", evt)
	}
	return nil
}

func (a *Account) Withdraw(amount int) error {
	if !a.opened {
		return errors.New("cannot withdraw from an unopened account")
	}
	if amount <= 0 {
		return errors.New("withdrawal amount must be positive")
	}
	if a.balance < amount {
		return errors.New("insufficient funds")
	}

	return a.recordThat(&MoneyWithdrawn{Amount: amount})
}

// Deposit contains the business logic for depositing money.
func (a *Account) Deposit(amount int) error {
	if !a.opened {
		return errors.New("cannot deposit to an unopened account")
	}
	if amount <= 0 {
		return errors.New("deposit amount must be positive")
	}
	return a.recordThat(&MoneyDeposited{Amount: amount})
}

func (a *Account) recordThat(event AccountEvent) error {
	return aggregate.RecordEvent(a, event)
}

// The account events:
func (a *Account) EventFuncs() event.FuncsFor[AccountEvent] {
	return event.FuncsFor[AccountEvent]{
		func() AccountEvent { return new(AccountOpened) },
		func() AccountEvent { return new(MoneyWithdrawn) },
		func() AccountEvent { return new(MoneyDeposited) },
	}
}

//sumtype:decl
type AccountEvent interface {
	event.Any
	isAccountEvent()
}
type AccountOpened struct {
	ID             AccountID `json:"id"`
	InitialBalance int       `json:"initialBalance"`
}

func (*AccountOpened) EventName() string { return "account/opened" }
func (*AccountOpened) isAccountEvent()   {}

type MoneyWithdrawn struct {
	Amount int `json:"amount"`
}

func (*MoneyWithdrawn) EventName() string { return "account/withdrawn" }
func (*MoneyWithdrawn) isAccountEvent()   {}

type MoneyDeposited struct {
	Amount int `json:"amount"`
}

func (*MoneyDeposited) EventName() string { return "account/deposited" }
func (*MoneyDeposited) isAccountEvent()   {}
