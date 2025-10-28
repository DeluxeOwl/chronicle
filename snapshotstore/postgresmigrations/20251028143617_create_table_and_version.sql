-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS chronicle_snapshots (
    log_id  TEXT PRIMARY KEY,
    version BIGINT NOT NULL,
    data    BYTEA NOT NULL
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS chronicle_snapshots;
-- +goose StatementEnd
