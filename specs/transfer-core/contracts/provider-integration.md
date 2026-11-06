# Provider integration contract

Version: 0.1.0-FROZEN

Base URL: `{MOCK_PROVIDER_URL}/{provider_id}`

## Create payout

`POST /payouts`

Headers:

- `Idempotency-Key`: unique per attempt (we use `{transfer_id}:{attempt}`)
- `Content-Type: application/json`
- Optional `X-Scenario`: ok | decline | error_5xx | slow (dev)

Body:

```json
{
  "reference": "<transfer_id>",
  "amount": "<quote.target_amount.amount>",
  "currency": "<quote.target_amount.currency>",
  "recipient": { },
  "callback_url": "<API_PUBLIC_URL>/webhooks/<provider_id>"
}
```

Responses:

| Status | Meaning |
|--------|---------|
| 201 | `{ "payout_id", "status": "PROCESSING" }` — payout created |
| 422 | Sync decline (e.g. RECIPIENT_ACCOUNT_INVALID) — try next provider |
| 500/503 | Retry same provider or next per policy |
| Connection drop / 504 | Unknown — poll, do not fallback blindly |

## Query payout

- `GET /payouts/{payout_id}`
- `GET /payouts?reference=<transfer_id>`

404 may mean visibility lag (up to ~15s in chaos).

## Rates

`GET {MOCK_PROVIDER_URL}/rates` — same shape as `data/rates.json` entries.

## Reconciliation

`GET /{provider_id}/admin/ledger` — used by chaos_run; entries with `status` COMPLETED are source of truth for paid amounts.

## Webhook inbound (our API)

Body per openapi `WebhookEvent`. Signature: `X-Signature = hex(HMAC-SHA256(raw_body, PROVIDER_WEBHOOK_SECRET))`.
