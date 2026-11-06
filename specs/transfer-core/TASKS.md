# TASKS.md — Transfer Core

Status: Ready for Implement (Gate 3)  
Version: 0.1.0  
Reads: SPEC.md, DECISIONS.md, contracts/ (FROZEN)

Формат задачи: **ID | описание | ACs | файлы (≤3) | тесты | deps | implementer**

Implementer: `cheap` = кодоген дешёвой моделью; `orchestrator` = wiring, review, refactor.

## Ритм задачи

1. Реализовать задачу (тесты раньше кода).
2. Прогнать тесты из колонки «Тесты».
3. **Только после того, как вы подтвердили, что тесты зелёные**, закоммитить и отправить в [https://github.com/Cadoranee/Doverka](https://github.com/Cadoranee/Doverka). Без вашего подтверждения пуш не делать.

---

## Block 0 — Scaffold

| ID | Описание | ACs | Файлы | Тесты | Deps | Impl |
|----|----------|-----|-------|-------|------|------|
| T0-01 | go.mod, cmd/api/main, конфиг env | — | go.mod, cmd/api/main.go, internal/config/config.go | smoke `go test ./...` | — | orchestrator |
| T0-02 | Dockerfile + compose health для api | L2-T-010 | Dockerfile, docker-compose.yml | manual compose up | T0-01 | cheap |
| T0-03 | Postgres migrations baseline + pool | — | migrations/001_init.sql, internal/store/db.go | integration db ping | T0-01 | cheap |
| T0-04 | Load data/*.json (tariffs, currencies, clients seed) | L1-Q-001 | internal/refdata/loader.go | loader_test.go | T0-03 | cheap |
| T0-05 | Clock interface (UTC, test fake) | L1-Q-006 | internal/clock/clock.go | clock_test.go | T0-01 | cheap |

---

## Block 1 — Level 1 Quotes

| ID | Описание | ACs | Файлы | Тесты | Deps | Impl |
|----|----------|-----|-------|-------|------|------|
| T1-01 | Money + Round HALF_UP | L1-Q-003 | internal/calc/money.go | money_test.go | T0-04 | cheap |
| T1-02 | Fee max(min, pct+fixed) both charged_in | L1-Q-003, L1-Q-012 | internal/calc/fee.go | fee_test.go | T1-01 | cheap |
| T1-03 | Forward SOURCE quote | L1-Q-001, L1-Q-003 | internal/calc/forward.go | forward_test.go | T1-02 | cheap |
| T1-04 | Inverse TARGET search | L1-Q-002, L1-Q-011 | internal/calc/inverse.go | inverse_test.go, property_test.go | T1-03 | orchestrator |
| T1-05 | Rates fetch + stale 6h + convert | L1-Q-008, L1-Q-003 | internal/rates/service.go | rates_test.go | T0-04, T0-05 | cheap |
| T1-06 | Direction resolve + limits validation | L1-Q-004–007 | internal/quote/validate.go | validate_test.go | T0-04 | cheap |
| T1-07 | Quote repo + TTL expires_at | L1-Q-009, L1-Q-010 | internal/quote/repo.go | repo_test.go | T0-03 | cheap |
| T1-08 | POST/GET /quotes handlers | L1-Q-001–010 | internal/httpapi/quotes.go | quotes_handler_test.go | T1-03–07 | orchestrator |
| T1-09 | Examples golden test | L1-Q-003 | test/examples_test.go | examples | T1-08 | cheap |
| T1-10 | Error codes unit tests | L1-Q-004–008 | internal/httpapi/errors_test.go | each PDF code | T1-08 | cheap |

**Checkpoint L1:** все examples зелёные; ASSUMPTIONS округление совпадает.

---

## Block 2 — Level 2 Transfers

| ID | Описание | ACs | Файлы | Тесты | Deps | Impl |
|----|----------|-----|-------|-------|------|------|
| T2-01 | Ledger reserve/release/debit | L2-T-007–009, L2-T-013 | internal/ledger/ledger.go | ledger_test.go | T0-03 | cheap |
| T2-02 | Client balance GET shape | L2-T-012 | internal/httpapi/balance.go | balance_test.go | T2-01 | cheap |
| T2-03 | Idempotency store | L2-T-002, L2-T-003 | internal/idempotency/store.go | idempotency_test.go | T0-03 | cheap |
| T2-04 | Transfer SM + transitions log | L2-T-011 | internal/transfer/sm.go | sm_test.go | T0-03 | orchestrator |
| T2-05 | Provider filter + score | L2-T-015, L2-T-016 | internal/provider/select/select.go | select_test.go | T0-04, T1-05 | orchestrator |
| T2-06 | Provider HTTP client | L2-T-014, L2-T-010 | internal/provider/client/client.go | client_test.go | T0-01 | cheap |
| T2-07 | Execute transfer worker | L2-T-007–010, L2-T-016 | internal/transfer/execute.go | execute_integration_test.go | T2-04–06 | orchestrator |
| T2-08 | POST /transfers 202 + quote checks | L2-T-001, L2-T-004–006 | internal/httpapi/transfers.go | transfers_test.go | T2-01–04, T1-07 | orchestrator |
| T2-09 | GET /transfers/{id} | L2-T-011 | internal/httpapi/transfer_get.go | transfer_get_test.go | T2-04 | cheap |
| T2-10 | Scenario tests ok/decline/5xx/slow | L2-T-007–010 | test/scenarios_test.go | docker | T2-07–09 | orchestrator |
| T2-11 | Balance invariant integration | L2-T-013 | test/balance_invariant_test.go | — | T2-10 | cheap |

**Checkpoint L2:** сценарии mock X-Scenario; idempotency; invariant.

---

## Block 3 — Level 3 Reliability

| ID | Описание | ACs | Файлы | Тесты | Deps | Impl |
|----|----------|-----|-------|-------|------|------|
| T3-01 | Webhook verify + dedup + apply | L3-R-003–006 | internal/webhook/handler.go | webhook_test.go | T2-04 | orchestrator |
| T3-02 | Unknown outcome poll policy | L3-R-007 | internal/provider/client/reconcile.go | unknown_test.go | T2-06 | orchestrator |
| T3-03 | Parallel idempotency stress | L3-R-001 | test/idempotency_parallel_test.go | — | T2-08 | cheap |
| T3-04 | Parallel balance contention | L3-R-002 | test/parallel_balance_test.go | — | T2-08 | cheap |
| T3-05 | Daily limit tracker | L3-R-010 | internal/ledger/daily.go | daily_test.go | T2-01, T1-05 | cheap |
| T3-06 | Startup resume worker | L3-R-008 | internal/reconcile/resume.go | resume_test.go | T2-07 | cheap |
| T3-07 | Background reconcile job | L3-R-009, L3-R-012 | internal/reconcile/job.go | job_test.go | T3-02 | orchestrator |
| T3-08 | chaos_run CI script target | L3-R-011 | Makefile or README section | manual chaos | T3-01–07 | orchestrator |

**Checkpoint L3:** `MOCK_MODE=chaos MOCK_SEED=1 python scripts/chaos_run.py --transfers 300` → RESULT OK.

---

## Block 4 — Docs (submission)

| ID | Описание | ACs | Файлы | Deps |
|----|----------|-----|-------|------|
| T4-01 | README run + test one-liner | PDF §11 | README.md | T0-02 |
| T4-02 | LIMITS.md honest gaps | PDF | LIMITS.md | T3-08 |
| T4-03 | DECISIONS §14 AI usage | PDF | DECISIONS.md | implement |

---

## Traceability matrix (MUST AC → task → test)

| AC | Task(s) | Test |
|----|---------|------|
| L1-Q-001 | T1-08 | quotes_handler_test |
| L1-Q-002 | T1-04, T1-08 | inverse_test |
| L1-Q-003 | T1-09 | examples_test |
| L1-Q-004 | T1-10 | errors_test |
| L1-Q-005 | T1-10 | errors_test |
| L1-Q-006 | T1-10 | errors_test |
| L1-Q-007 | T1-10 | errors_test |
| L1-Q-008 | T1-05, T1-10 | rates_test |
| L1-Q-009 | T1-07 | repo_test |
| L1-Q-010 | T1-08 | quotes_handler_test |
| L1-Q-011 | T1-04 | property_test |
| L2-T-001 | T2-08 | transfers_test |
| L2-T-002 | T2-03, T2-08 | idempotency_test |
| L2-T-003 | T2-03 | idempotency_test |
| L2-T-004–006 | T2-08 | transfers_test |
| L2-T-007–010 | T2-10 | scenarios_test |
| L2-T-011 | T2-09 | transfer_get_test |
| L2-T-012 | T2-02 | balance_test |
| L2-T-013 | T2-11 | balance_invariant_test |
| L2-T-014 | T2-06, T2-07 | execute_integration |
| L2-T-015–016 | T2-05, T2-07 | select_test, scenarios |
| L3-R-001 | T3-03 | idempotency_parallel |
| L3-R-002 | T3-04 | parallel_balance |
| L3-R-003–006 | T3-01 | webhook_test |
| L3-R-007 | T3-02, T3-07 | unknown_test, job |
| L3-R-008 | T3-06 | resume_test |
| L3-R-009 | T3-07 | job_test |
| L3-R-010 | T3-05 | daily_test |
| L3-R-011 | T3-08 | chaos_run |
| L3-R-012 | T3-07 | job_test |

---

## Gate 3 checklist

- [x] Каждый MUST AC имеет ≥1 задачу и тест
- [x] Задачи ≤3 файлов (кроме orchestrator merge)
- [x] Contracts не изменяются в implement tasks
- [ ] Human approval → `/sdd:next-task T0-01`
- [ ] После зелёных тестов каждой задачи — push в https://github.com/Cadoranee/Doverka (только с подтверждением)

**Вердикт Gate 3:** PASS (39 задач, 28 MUST AC покрыты)
