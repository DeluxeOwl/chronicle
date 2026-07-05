-- +goose Up
-- +goose StatementBegin

-- Move the per-row version check from a PL/pgSQL trigger into application code.
-- Previously, every INSERT fired chronicle_check_event_version() which acquired
-- an advisory lock and ran a SELECT per row. For a batch of N events this meant
-- N lock acquisitions + N index lookups, all redundant after the first.
-- The same check now runs once per batch in Go (AppendInTx), keeping identical
-- safety guarantees with significantly less overhead.
DROP TRIGGER IF EXISTS trg_chronicle_check_event_version ON chronicle_events;
DROP FUNCTION IF EXISTS chronicle_check_event_version();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Restore the per-row trigger for environments that haven't upgraded the Go code.
CREATE OR REPLACE FUNCTION chronicle_check_event_version()
RETURNS TRIGGER AS $$
DECLARE max_version BIGINT;
BEGIN
	PERFORM pg_advisory_xact_lock(hashtext(NEW.log_id));

	SELECT version INTO max_version FROM chronicle_events WHERE log_id = NEW.log_id ORDER BY version DESC LIMIT 1;

	IF NOT FOUND THEN
		max_version := 0;
	END IF;

	IF NEW.version != max_version + 1 THEN
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
