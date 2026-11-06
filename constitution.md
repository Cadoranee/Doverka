# Project Constitution — Transfer Core

Version: 1.0.0  
Last updated: 2026-11-06

## Architecture Principles

- Расчёт котировки, выбор провайдера и HTTP-клиент провайдера — **разные пакеты**, без «божественных» handler-ов.
- Контракты API и провайдера из `specs/transfer-core/contracts/` **заморожены после Gate 2**; изменения только через `/sdd:amend`.
- Источник истины по деньгам — ledger в БД; баланс `available`/`reserved` — производное от проводок.
- Идемпотентность и state machine — на уровне домена, не только HTTP.

## Technology Stack

| Layer | Choice | Notes |
|-------|--------|-------|
| Language | Go 1.22+ | |
| HTTP | std `net/http` или chi | |
| Database | PostgreSQL 16 | SQLite не используем (compose уже с PG) |
| Migrations | goose или embed SQL | Применяются при старте |
| Money | `shopspring/decimal` или целые minor units в `int64` | **Запрещён float** |
| Tests | `testing` + testify | Интеграционные — docker compose |
| Config | env + `data/*.json` read-only mount |

## Security Constraints

- Секреты только из env (`PROVIDER_WEBHOOK_SECRET`); не в ZIP и не в git.
- Webhook: проверка HMAC-SHA256 обязательна в production-path.
- Не логировать полные PAN/счета — допустимо последние 4 символа.

## Naming Conventions

- Go packages: lowercase single word (`calc`, `ledger`)
- DB: snake_case
- JSON API: snake_case как в PDF
- Env: SCREAMING_SNAKE_CASE

## Banned Patterns

- float32/float64 для денег или курсов
- Пересчёт суммы перевода при исполнении (только quote)
- Fallback на другого провайдера при unknown payout без доказательства «не создано»
- Правка `contracts/` в задачах implement без amend

## File Structure Rules

```
cmd/api/
internal/calc/
internal/rates/
internal/quote/
internal/transfer/
internal/ledger/
internal/provider/select/
internal/provider/client/
internal/webhook/
internal/reconcile/
internal/httpapi/
migrations/
specs/transfer-core/
```

## Open Questions / Deferred Decisions

- Метрики и hot-reload конфига провайдеров — Level 4, вне scope.
