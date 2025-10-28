-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS chronicle_events (
	global_version 	INTEGER PRIMARY KEY AUTOINCREMENT,
	log_id          TEXT    NOT NULL,
	version        	INTEGER NOT NULL,
	event_name     	TEXT    NOT NULL,
	data           	BLOB,
	UNIQUE (log_id, version)
);
CREATE TRIGGER IF NOT EXISTS check_event_version
	BEFORE INSERT ON chronicle_events
	FOR EACH ROW
	BEGIN
		-- This custom message is key for our driver-agnostic error check. Must be "_chronicle_version_conflict: "
		SELECT RAISE(ABORT, '_chronicle_version_conflict: ' || (SELECT COALESCE(MAX(version), 0) FROM chronicle_events WHERE log_id = NEW.log_id))
		WHERE NEW.version != (
			SELECT COALESCE(MAX(version), 0) + 1
			FROM chronicle_events
			WHERE log_id = NEW.log_id
		);
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS check_event_version;
DROP TABLE IF EXISTS chronicle_events;
-- +goose StatementEnd
