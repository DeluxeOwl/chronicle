package main

import (
	"context"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/examples/internal/account"
)

func main() {
	memoryEventLog := eventlog.NewMemory()

	// A 256-bit key (32 bytes)
	encryptionKey := []byte("a-very-secret-32-byte-key-123456")
	cryptoTransformer := account.NewCryptoTransformer(encryptionKey)

	accountRepo, err := chronicle.NewEventSourcedRepository(
		memoryEventLog,
		account.NewEmpty,
		[]event.Transformer[account.AccountEvent]{cryptoTransformer},
	)
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	accID := account.AccountID("crypto-123")

	acc, err := account.Open(accID, time.Now(), "John Smith")
	if err != nil {
		panic(err)
	}

	_ = acc.DepositMoney(100)
	_, _, err = accountRepo.Save(ctx, acc)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Account for '%s' saved successfully.\n", acc.HolderName())

	// 4. Load the account again to verify decryption
	reloadedAcc, err := accountRepo.Get(ctx, accID)
	if err != nil {
		panic(err)
	}
	fmt.Printf(
		"Account reloaded. Holder name is correctly decrypted: '%s'\n\n",
		reloadedAcc.HolderName(),
	)
}
