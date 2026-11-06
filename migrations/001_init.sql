CREATE TABLE quotes (
	quote_id text PRIMARY KEY,
	direction_id text NOT NULL,
	source_country text NOT NULL,
	source_currency text NOT NULL,
	target_country text NOT NULL,
	target_currency text NOT NULL,
	payout_method text NOT NULL,
	amount_type text NOT NULL CHECK (amount_type IN ('SOURCE', 'TARGET')),
	source_amount_minor bigint NOT NULL,
	fee_amount_minor bigint NOT NULL,
	fee_currency text NOT NULL,
	amount_to_convert_minor bigint NOT NULL,
	rate numeric NOT NULL,
	target_amount_minor bigint NOT NULL,
	breakdown jsonb NOT NULL,
	expires_at timestamptz NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	CHECK (char_length(source_currency) = 3 AND char_length(target_currency) = 3 AND char_length(fee_currency) = 3)
);

CREATE TABLE transfers (
	transfer_id text PRIMARY KEY,
	quote_id text NOT NULL REFERENCES quotes (quote_id),
	client_id text NOT NULL,
	status text NOT NULL CHECK (status IN ('CREATED', 'RESERVED', 'SUBMITTING', 'PROCESSING', 'COMPLETED', 'FAILED', 'NEEDS_REVIEW')),
	source_amount_minor bigint NOT NULL,
	source_currency text NOT NULL,
	target_amount_minor bigint NOT NULL,
	target_currency text NOT NULL,
	recipient jsonb NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	CHECK (char_length(source_currency) = 3 AND char_length(target_currency) = 3)
);

CREATE TABLE transfer_transitions (
	id bigserial PRIMARY KEY,
	transfer_id text NOT NULL REFERENCES transfers (transfer_id),
	from_status text NOT NULL,
	to_status text NOT NULL,
	occurred_at timestamptz NOT NULL,
	reason text NOT NULL DEFAULT ''
);

CREATE TABLE provider_attempts (
	id bigserial PRIMARY KEY,
	transfer_id text NOT NULL REFERENCES transfers (transfer_id),
	provider_id text NOT NULL,
	idempotency_key text NOT NULL UNIQUE,
	payout_id text,
	result text,
	attempted_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE client_balances (
	client_id text NOT NULL,
	currency text NOT NULL,
	available bigint NOT NULL,
	reserved bigint NOT NULL,
	PRIMARY KEY (client_id, currency),
	CHECK (available >= 0 AND reserved >= 0),
	CHECK (char_length(currency) = 3)
);

CREATE TABLE ledger_entries (
	id bigserial PRIMARY KEY,
	client_id text NOT NULL,
	currency text NOT NULL,
	entry_type text NOT NULL CHECK (entry_type IN ('seed', 'reserve', 'release', 'debit')),
	amount_minor bigint NOT NULL,
	transfer_id text REFERENCES transfers (transfer_id),
	created_at timestamptz NOT NULL DEFAULT now(),
	CHECK (char_length(currency) = 3)
);

CREATE TABLE idempotency_keys (
	idempotency_key text PRIMARY KEY,
	request_hash text NOT NULL,
	response_body jsonb,
	status_code int,
	created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE webhook_events (
	provider_id text NOT NULL,
	event_id text NOT NULL,
	payload jsonb NOT NULL,
	processed_at timestamptz,
	PRIMARY KEY (provider_id, event_id)
);

CREATE TABLE daily_usage (
	client_id text NOT NULL,
	usage_date date NOT NULL,
	currency text NOT NULL,
	amount_minor bigint NOT NULL,
	PRIMARY KEY (client_id, usage_date, currency),
	CHECK (char_length(currency) = 3)
);

CREATE INDEX transfers_quote_id_idx ON transfers (quote_id);
CREATE INDEX transfers_client_id_idx ON transfers (client_id);
CREATE INDEX transfer_transitions_transfer_id_idx ON transfer_transitions (transfer_id);
CREATE INDEX provider_attempts_transfer_id_idx ON provider_attempts (transfer_id);
CREATE INDEX ledger_entries_transfer_id_idx ON ledger_entries (transfer_id);
