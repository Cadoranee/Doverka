# SPEC.md — Transfer Core

Mode: Full  
Status: Approved (Gate 2 — contracts FROZEN)  
Version: 0.1.0  
Baseline: ASSUMPTIONS.md v1

## Overview

Ядро Northwind Transfer: котировка межвалютного перевода, исполнение через внешних провайдеров с резервированием средств, идемпотентностью и устойчивостью к сбоям (Level 1–3). Стек реализации задаётся constitution.md, не этим документом.

## User Stories

1. **Котировка (L1):** Клиент запрашивает цену (SOURCE или TARGET), видит fee, курс, итог и срок действия.
2. **Перевод (L2):** Клиент подтверждает перевод по quote; система резервирует средства, выбирает провайдера, доводит до COMPLETED или FAILED с возвратом резерва.
3. **Надёжность (L3):** При обрывах, дублях webhook и рестарте нет двойных выплат и «зависших» резервов; неясные случаи → NEEDS_REVIEW.

## Boundaries

**Always**

- Соблюдать контракт API §6 PDF и `contracts/`.
- Хранить историю переходов transfer и попыток провайдеров.
- Прогонять `examples/` и обязательные коды ошибок тестами.

**Ask first**

- Изменение замороженных contracts.
- Новые обязательные поля в ответах, ломающие chaos_run.

**Never**

- Float для денег.
- Секреты в репозитории/архиве.
- Списание без успешной выплаты (кроме явной бизнес-политики chargeback — out of scope).

## Out of Scope (WONT)

- Level 4: структурные логи/метрики, конфиг провайдеров без редеплоя, load test, endpoint «почему провайдер».
- UI, KYC, реальные банковские интеграции вне mock-provider.
- Изменение тарифов/курсов в runtime (только чтение data + mock rates).

---

## Acceptance Criteria

### Level 1 — Quotes

**L1-Q-001 [MUST]** Given валидное направление RU→TH BANK и сумма в лимитах, When POST /quotes amount_type SOURCE, Then 201 и поля `quote_id`, `direction_id`, `source_amount`, `fee`, `amount_to_convert`, `rate`, `target_amount`, `expires_at`, `breakdown` (array).

**L1-Q-002 [MUST]** Given те же данные, When amount_type TARGET с суммой из examples, Then 201 и target_amount совпадает с запросом (с допущением ASSUMPTIONS 1.5).

**L1-Q-003 [MUST]** Given фиксированные tariffs/rates из тестового окружения, When POST /quotes для каждого файла в `examples/*.json`, Then status и числовые поля body совпадают с expected (кроме quote_id, expires_at).

**L1-Q-004 [MUST]** Given сумма ниже min направления, When POST /quotes, Then ошибка с code `AMOUNT_BELOW_MIN`.

**L1-Q-005 [MUST]** Given сумма выше max, Then `AMOUNT_ABOVE_MAX`.

**L1-Q-006 [MUST]** Given неподдерживаемый payout_method, Then `UNSUPPORTED_DIRECTION`.

**L1-Q-007 [MUST]** Given невалидный JSON или формат amount, Then `VALIDATION_ERROR`.

**L1-Q-008 [MUST]** Given нет применимого свежего курса, Then `RATE_UNAVAILABLE`.

**L1-Q-009 [MUST]** Given два последовательных POST /quotes с одинаковым телом, Then разные `quote_id`.

**L1-Q-010 [MUST]** Given сохранённая quote, When GET /quotes/{id}, Then тот же snapshot расчёта.

**L1-Q-011 [MUST]** Given SOURCE-расчёт S и TARGET-расчёт с target = forward(S), When оба запроса, Then согласованность source/fee/target в пределах одного forward шага (property test).

**L1-Q-012 [SHOULD]** Given charged_in TARGET, Then fee.currency = target currency и amount_to_convert.currency = source.

---

### Level 2 — Transfers, balance, providers

**L2-T-001 [MUST]** Given валидная непросроченная quote и достаточный available, When POST /transfers с Idempotency-Key, Then 202 и body содержит `transfer_id`, `status` (не terminal или PROCESSING).

**L2-T-002 [MUST]** Given повтор POST /transfers с тем же ключом и телом, Then тот же `transfer_id` и согласованный status (идемпотентность).

**L2-T-003 [MUST]** Given тот же ключ и другой quote_id/body, Then 409 `IDEMPOTENCY_KEY_REUSED`.

**L2-T-004 [MUST]** Given quote expired, When POST /transfers, Then `QUOTE_EXPIRED`.

**L2-T-005 [MUST]** Given unknown quote_id, Then `QUOTE_NOT_FOUND`.

**L2-T-006 [MUST]** Given available < source_amount, Then `INSUFFICIENT_FUNDS` без создания transfer.

**L2-T-007 [MUST]** Given успешный сценарий провайдера ok (X-Scenario), When transfer завершён, Then status COMPLETED, reserved=0, available уменьшен на source_amount.

**L2-T-008 [MUST]** Given decline сценарий, Then FAILED, резерв возвращён, available восстановлен.

**L2-T-009 [MUST]** Given error_5xx без создания payout, Then FAILED после fallback или исчерпания провайдеров, резерв возвращён.

**L2-T-010 [MUST]** Given slow сценарий, Then eventual COMPLETED; POST /transfers возвращает 202 до завершения (не блокирует 30s+).

**L2-T-011 [MUST]** When GET /transfers/{id}, Then status, history transitions, provider attempts (provider_id, time, result).

**L2-T-012 [MUST]** When GET /clients/{id}/balance, Then по каждой валюте `available` и `reserved` (chaos_run parseable).

**L2-T-013 [MUST]** For any client: start_available + start_reserved = available + reserved + sum(source_amount of COMPLETED) over session.

**L2-T-014 [MUST]** Payout amount/currency к провайдеру = quote.target_amount / target_currency (string equal).

**L2-T-015 [MUST]** Primary provider selection: only enabled, not in maintenance, direction match, margin ≥ min_margin_percent.

**L2-T-016 [MUST]** On RECIPIENT_ACCOUNT_INVALID, attempt next eligible provider before FAILED.

**L2-T-017 [SHOULD]** When quote already used by COMPLETED transfer, Then `QUOTE_ALREADY_USED`.

**L2-T-018 [MUST]** When no eligible provider at submission, Then error before transfer record **or** documented code without orphan id — см. ASSUMPTIONS 4.11 (prefer reject at POST).

---

### Level 3 — Reliability

**L3-R-001 [MUST]** Given parallel double POST /transfers same Idempotency-Key, Then at most one transfer_id.

**L3-R-002 [MUST]** Given two parallel transfers same client depleting balance, Then at most one succeeds; other INSUFFICIENT_FUNDS or DAILY_LIMIT_EXCEEDED.

**L3-R-003 [MUST]** Given duplicate webhook same event_id, Then single state transition; HTTP 200.

**L3-R-004 [MUST]** Given webhook invalid/missing signature, Then no state change; HTTP 200.

**L3-R-005 [MUST]** Given webhook for unknown payout, Then HTTP 200, no panic.

**L3-R-006 [MUST]** Given terminal COMPLETED, When late contradictory webhook, Then status unchanged.

**L3-R-007 [MUST]** Given provider unknown outcome (disconnect after accept), When reconcile/poll resolves, Then no second payout to another provider unless first definitively absent.

**L3-R-008 [MUST]** Given process restart mid-transfer, Then worker resumes to terminal or NEEDS_REVIEW.

**L3-R-009 [MUST]** Background reconciliation for stale PROCESSING beyond threshold.

**L3-R-010 [MUST]** DAILY_LIMIT_EXCEEDED when sum of today's gross source (non-FAILED) + new transfer exceeds client daily_limit (converted if needed).

**L3-R-011 [MUST]** `scripts/chaos_run.py --transfers 300` with MOCK_MODE=chaos: RESULT OK (zero violations).

**L3-R-012 [SHOULD]** Transfers stuck indeterminate → NEEDS_REVIEW with reserved held until human/reconcile policy.

---

## Open Questions

См. ASSUMPTIONS.md §9 — ответы организаторов могут породить `/sdd:amend`.

## AC → Component Map (preview)

| AC group | Component |
|----------|-----------|
| L1-Q-* | calc, rates, quote store, httpapi quotes |
| L2-T-* | transfer SM, ledger, provider select/client, httpapi transfers |
| L3-R-* | webhook, reconcile, idempotency store, worker |

Полная трассировка в TASKS.md.

## Gate 2 Checklist

- [x] Каждый MUST имеет тест в TASKS.md
- [x] DECISIONS + contracts согласованы с chaos_run
- [x] Contracts помечены FROZEN v0.1.0
