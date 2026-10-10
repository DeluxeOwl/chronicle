package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/queue"
	_ "github.com/mattn/go-sqlite3"
)

type Email struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
}

func main() {
	path := "queue.db"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	log, err := eventlog.NewSqlite(db)
	if err != nil {
		panic(err)
	}
	q, err := queue.New[Email](log, "emails")
	if err != nil {
		panic(err)
	}
	ctx := context.Background()

	// The stable ID makes repeated runs safe, including after acknowledgment.
	if err := q.Enqueue(ctx, queue.Message[Email]{
		ID: "welcome/alice", Payload: Email{To: "alice@example.com", Subject: "Welcome!"},
	}); err != nil {
		panic(err)
	}

	delivery, err := q.Claim(ctx, time.Minute)
	if err != nil {
		panic(err)
	}
	if delivery == nil {
		fmt.Println("No ready emails. This message may already be acknowledged.")
		return
	}

	fmt.Printf(
		"Message %s, attempt %d: send %q to %s\n",
		delivery.Message.ID,
		delivery.Attempt,
		delivery.Message.Payload.Subject,
		delivery.Message.Payload.To,
	)
	// A real email provider must deduplicate by delivery.Message.ID. A crash
	// after sending but before Ack can cause the same message to run again.
	if err := q.Ack(ctx, delivery.Receipt); err != nil {
		panic(err)
	}
	fmt.Println("Acknowledged.")
}
