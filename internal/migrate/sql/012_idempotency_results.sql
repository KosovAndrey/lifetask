-- Legacy claims have no recoverable result; keep them to prevent replaying old writes.
ALTER TABLE idempotency_keys ADD COLUMN request_hash text;
ALTER TABLE idempotency_keys ADD COLUMN status_code integer;
ALTER TABLE idempotency_keys ADD COLUMN response_headers jsonb;
ALTER TABLE idempotency_keys ADD COLUMN response_body bytea;
