# Error catalog (API)

Version: 0.1.0-FROZEN

| code | HTTP | When |
|------|------|------|
| VALIDATION_ERROR | 400 | Invalid JSON, amount format, missing Idempotency-Key |
| UNSUPPORTED_DIRECTION | 422 | No tariff for country/currency/method |
| AMOUNT_BELOW_MIN | 422 | Below direction min |
| AMOUNT_ABOVE_MAX | 422 | Above direction max |
| RATE_UNAVAILABLE | 422 | No fresh rate |
| QUOTE_NOT_FOUND | 404 | Unknown quote_id |
| QUOTE_EXPIRED | 422 | quote past expires_at on transfer |
| QUOTE_ALREADY_USED | 422 | Second transfer on same quote |
| INSUFFICIENT_FUNDS | 422 | available < source_amount |
| DAILY_LIMIT_EXCEEDED | 422 | Daily cap exceeded |
| IDEMPOTENCY_KEY_REUSED | 409 | Same key, different body |
| NO_PROVIDER_AVAILABLE | 422 | No provider can execute |
| CLIENT_NOT_FOUND | 404 | Unknown client_id |

Provider errors (outbound, not returned to client as-is): see `provider-integration.md`.
