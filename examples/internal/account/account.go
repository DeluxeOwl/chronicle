package account

import (
	"errors"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
)

type AccountID string

func (a AccountID) String() string { return string(a) }

type Account struct {
	aggregate.Base

	id AccountID

	openedAt   time.Time
	balance    int // we need to know how much money an account has
	holderName string
}

func (a *Account) ID() AccountID {
	return a.id
}

//sumtype:decl
type AccountEvent interface {
	event.Any
	isAccountEvent()
}

// We say an account is "opened", not "created"
type accountOpened struct {
	ID         AccountID `json:"id"`
	OpenedAt   time.Time `json:"openedAt"`
	HolderName string    `json:"holderName"`
}

func (*accountOpened) EventName() string { return "account/opened" }
func (*accountOpened) isAccountEvent()   {}

type moneyDeposited struct {
	Amount int `json:"amount"` // Note: In a real-world application, you would use a dedicated money type instead of an int to avoid precision issues.
}

// ⚠️ Note: the event name is unique
func (*moneyDeposited) EventName() string { return "account/money_deposited" }
func (*moneyDeposited) isAccountEvent()   {}

type moneyWithdrawn struct {
	Amount int `json:"amount"`
}

// ⚠️ Note: the event name is unique
func (*moneyWithdrawn) EventName() string { return "account/money_withdrawn" }
func (*moneyWithdrawn) isAccountEvent()   {}

func (a *Account) EventFuncs() event.FuncsFor[AccountEvent] {
	return event.FuncsFor[AccountEvent]{
		func() AccountEvent { return new(accountOpened) },
		func() AccountEvent { return new(moneyDeposited) },
		func() AccountEvent { return new(moneyWithdrawn) },
	}
}

func (a *Account) Apply(evt AccountEvent) error {
	switch event := evt.(type) {
	case *accountOpened:
		a.id = event.ID
		a.openedAt = event.OpenedAt
		a.holderName = event.HolderName
	case *moneyWithdrawn:
		a.balance -= event.Amount
	case *moneyDeposited:
		a.balance += event.Amount
	default:
		return fmt.Errorf("unexpected event kind: %T", event)
	}

	return nil
}

func NewEmpty() *Account {
	return new(Account)
}

func Open(id AccountID, currentTime time.Time, holderName string) (*Account, error) {
	if currentTime.Weekday() == time.Sunday {
		return nil, errors.New("sorry, you can't open an account on Sunday ¯\\_(ツ)_/¯")
	}

	a := NewEmpty()

	// Note: this is type safe, you'll get autocomplete for the events
	if err := a.recordThat(&accountOpened{
		ID:         id,
		OpenedAt:   currentTime,
		HolderName: holderName,
	}); err != nil {
		return nil, fmt.Errorf("open account: %w", err)
	}

	return a, nil
}

func (a *Account) recordThat(event AccountEvent) error {
	return aggregate.RecordEvent(a, event)
}

func (a *Account) DepositMoney(amount int) error {
	if amount <= 0 {
		return errors.New("amount must be greater than 0")
	}

	return a.recordThat(&moneyDeposited{
		Amount: amount,
	})
}

// Returns the amount withdrawn and an error if any
func (a *Account) WithdrawMoney(amount int) (int, error) {
	if a.balance < amount {
		return 0, fmt.Errorf("insufficient money, balance left: %d", a.balance)
	}

	err := a.recordThat(&moneyWithdrawn{
		Amount: amount,
	})
	if err != nil {
		return 0, fmt.Errorf("error during withdrawal: %w", err)
	}

	return amount, nil
}

func (a *Account) Balance() int {
	return a.balance
}

func (a *Account) HolderName() string {
	return a.holderName
}
