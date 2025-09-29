package testutils

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func SetupNATS(t testing.TB) (*nats.Conn, error) {
	t.Helper()

	ctx := context.Background()

	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "nats:2.12",
			ExposedPorts: []string{"4222/tcp"},
			Entrypoint: []string{"nats-server", "-js"},
			WaitingFor: wait.ForListeningPort("4222/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	}

	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		t.Fatalf("failed to start NATS container: %v", err)
		return nil, err
	}

	endpoint, err := container.Endpoint(ctx, "")
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("failed to get NATS container endpoint: %v", err)
		return nil, err
	}

	nc, err := nats.Connect("nats://" + endpoint)
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("failed to connect to NATS server: %v", err)
		return nil, err
	}

	t.Cleanup(func() {
		nc.Close()
		if err := container.Terminate(ctx); err != nil {
			t.Fatalf("failed to terminate container: %s", err)
		}
	})

	return nc, nil
}