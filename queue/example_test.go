package queue_test

import (
	"context"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/queue"
)

func ExampleNew() {
	q, err := queue.New[string](eventlog.NewMemory(), "emails")
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	if err := q.Enqueue(ctx, queue.Message[string]{ID: "welcome-123", Payload: "Welcome!"}); err != nil {
		panic(err)
	}
	delivery, err := q.Claim(ctx, time.Minute)
	if err != nil {
		panic(err)
	}
	if delivery == nil {
		return
	}
	fmt.Println(delivery.Message.ID, delivery.Message.Payload, delivery.Attempt)
	if err := q.Ack(ctx, delivery.Receipt); err != nil {
		panic(err)
	}
	// Output: welcome-123 Welcome! 1
}
