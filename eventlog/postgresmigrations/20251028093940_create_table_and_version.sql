-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS chronicle_events (
	global_version BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	log_id         TEXT NOT NULL,
	version        BIGINT NOT NULL,
	event_name     TEXT NOT NULL,
	data           JSONB,
	UNIQUE (log_id, version)
);

CREATE OR REPLACE FUNCTION chronicle_check_event_version()
RETURNS TRIGGER AS $$
DECLARE max_version BIGINT;
BEGIN
	-- Acquire a transaction-level advisory lock based on a hash of the log_id.
	-- This serializes inserts for the same log_id, preventing the race condition
	-- where two transactions simultaneously try to insert the first event for a stream.
	-- The lock is automatically released at the end of the transaction.
	PERFORM pg_advisory_xact_lock(hashtext(NEW.log_id));

	-- Now that we have the lock, we can safely check the current version.
	SELECT version INTO max_version FROM chronicle_events WHERE log_id = NEW.log_id ORDER BY version DESC LIMIT 1;
	
	-- If no row was found (i.e., this is the first event for this log_id),
	-- the 'max_version' will be NULL. COALESCE handles this, setting it to 0.
	IF NOT FOUND THEN
		max_version := 0;
	END IF;

	IF NEW.version != max_version + 1 THEN
		-- Raise an exception with the actual latest version to be parsed by the client.
		RAISE EXCEPTION '_chronicle_version_conflict: %', max_version;
	END IF;

	RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_chronicle_check_event_version
    BEFORE INSERT ON chronicle_events
    FOR EACH ROW
    EXECUTE FUNCTION chronicle_check_event_version();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS trg_chronicle_check_event_version ON chronicle_events;

DROP FUNCTION IF EXISTS chronicle_check_event_version();
DROP TABLE IF EXISTS chronicle_events;
-- +goose StatementEnd
