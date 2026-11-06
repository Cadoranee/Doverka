# ASSUMPTIONS.md — Transfer Core

Документ фиксирует неоднозначности условия тестового задания и принятые допущения до написания SPEC/DECISIONS. Статус: `[CONFIRMED]` — проверено по данным стартера, `examples/` или коду mock-provider; `[VERIFY]` — обоснованный вывод, может уточняться в SPEC.

---

## 1. Деньги и округление

| # | Неоднозначность | Допущение | Статус | Обоснование |
|---|-----------------|-----------|--------|-------------|
| 1.1 | Режим округления fee, `target_amount`, обратный source в TARGET | **ROUND_HALF_UP** до `minor_units` валюты результата; промежуточные значения не округляются до финального шага | `[VERIFY]` | `examples/01–10` сходятся с HALF_UP; JPY (0 decimals) в `08_zero_decimal_target.json` |
| 1.2 | Формула комиссии | `fee = max(min, amount × percent/100 + fixed)` в валюте `charged_in`; для SOURCE — от gross source, для TARGET — от gross target до вычета fee | `[CONFIRMED]` | `07_percent_plus_fixed.json`: 2500×2%+30=80=min; `10_fee_charged_in_target.json`: 2.1M×1.2%+5000=30200 |
| 1.3 | `amount_to_convert` | SOURCE: `source − fee` (fee в source). TARGET+fee in TARGET: `source` без изменения, fee с gross target | `[CONFIRMED]` | PDF §6 и example 10 |
| 1.4 | Конвертация | `target = round(amount_to_convert × rate, target.minor)`; rate — клиентский, без наценки | `[CONFIRMED]` | PDF §3, examples |
| 1.5 | TARGET-режим (обратный расчёт) | Подбираем минимальный `source_amount` (шаг 1 minor unit source), при котором forward-расчёт даёт `target_amount ≥ запрошенного` (или точное совпадение, если достижимо); лимиты проверяем по итоговому source | `[VERIFY]` | Example 03: T=124068.63 → S=299900; exact forward даёт T=124068.63 |
| 1.6 | Неделимость курса (KWD→THB 113.75) | Допускаем target с дробными minor units (5573.75 THB в example 09) | `[CONFIRMED]` | `09_three_decimal_source.json` |
| 1.7 | Представление в коде | Суммы в API — строки; внутри — decimal/rational, **без float** | `[CONFIRMED]` | PDF §1 «Недопустимо: float для денег» |
| 1.8 | Поле `rate` в ответе | Строка с 4 знаками после запятой, если в данных курса 4 знака; иначе как в источнике | `[VERIFY]` | PDF пример «0.4200» |
| 1.9 | Inverse-курс | Если пары `(A,B)` нет, используем `1/rate(B,A)` с той же точностью, что хранится | `[VERIFY]` | В `rates.json` нет RUB→KWD, есть KWD→RUB; mock `reference_data.convert()` поддерживает inverse и cross |

**Единая точка округления:** одна функция `Round(amount, currencyCode)` — описывается в DECISIONS, поведение — здесь (1.1).

---

## 2. Котировка (quotes)

| # | Неоднозначность | Допущение | Статус | Обоснование |
|---|-----------------|-----------|--------|-------------|
| 2.1 | Формат суммы | Строка, ровно `minor_units` знаков после точки (или без точки при minor=0); иначе `VALIDATION_ERROR` | `[CONFIRMED]` | PDF §6; mock `amount_is_valid()` |
| 2.2 | Отрицательный / нулевой amount | `VALIDATION_ERROR` | `[VERIFY]` | Разумная валидация |
| 2.3 | Несуществующее направление | `UNSUPPORTED_DIRECTION` (страна, валюта, payout_method не совпадают с тарифом) | `[CONFIRMED]` | `06_unsupported_method.json` |
| 2.4 | Лимиты направления | Границы **включительно**; валюта лимита — `amount_limits.currency` (source) | `[CONFIRMED]` | Examples 04/05 |
| 2.5 | Повторный POST /quotes с тем же телом | **Новый** `quote_id` и новый `expires_at` | `[VERIFY]` | TTL привязан к котировке; идемпотентность не требуется для quotes |
| 2.6 | Истечение TTL | Котировка недействительна при `now >= expires_at` (UTC) | `[VERIFY]` | PDF «ограниченный срок» |
| 2.7 | GET /quotes/{id} после истечения | **200** с полем `expires_at` в прошлом; при POST /transfers — `QUOTE_EXPIRED` | `[VERIFY]` | GET — справочный; блокировка на исполнении |
| 2.8 | Нет курса / протухший курс | `RATE_UNAVAILABLE` | `[CONFIRMED]` | PDF §6; в data RUB→PHP/AED старше 6 ч относительно «сейчас»; mock `STALE_AFTER = 6h` |
| 2.9 | Cross-rate | Разрешён через одну промежуточную валюту, как в mock | `[CONFIRMED]` | `mock-provider/app/reference_data.py:69–74` |
| 2.10 | fee ≥ source (SOURCE) | `VALIDATION_ERROR` или `AMOUNT_BELOW_MIN` после расчёта — трактуем как невозможность удовлетворить лимиты | `[VERIFY]` | Edge case |
| 2.11 | `breakdown` | Массив произвольных шагов (человекочитаемых), без жёсткой схемы | `[CONFIRMED]` | PDF «в свободной форме» |

---

## 3. Курсы (источник для Level 1+)

| # | Неоднозначность | Допущение | Статус | Обоснование |
|---|-----------------|-----------|--------|-------------|
| 3.1 | Источник в runtime | `GET {MOCK_PROVIDER_URL}/rates` (актуальный снимок); `data/rates.json` — только для офлайн-тестов и seed | `[VERIFY]` | PDF §5 |
| 3.2 | Свежесть | Курс применим, если `now - updated_at ≤ 6 hours` | `[CONFIRMED]` | `reference_data.py:6–7`, `STALE_AFTER` |
| 3.3 | Недоступность /rates | POST /quotes для затронутых пар → `RATE_UNAVAILABLE`; сервис остаётся живым | `[VERIFY]` | Отказоустойчивость Level 1 |

---

## 4. Перевод, баланс, идемпотентность

| # | Неоднозначность | Допущение | Статус | Обоснование |
|---|-----------------|-----------|--------|-------------|
| 4.1 | Область Idempotency-Key | **Глобально** по сервису (не per client) | `[VERIFY]` | PDF не уточняет; mock-provider глобально по ключу |
| 4.2 | Нет заголовка Idempotency-Key | **400** + код `VALIDATION_ERROR` (или `IDEMPOTENCY_KEY_REQUIRED` в README) | `[CONFIRMED]` | mock-provider:400 `IDEMPOTENCY_KEY_REQUIRED` |
| 4.3 | Тот же ключ, другое тело | **409** + `IDEMPOTENCY_KEY_REUSED` | `[CONFIRMED]` | mock `main.py:133–134`; зеркалим для API |
| 4.4 | Порядок проверок при повторе | Сначала lookup по ключу → вернуть сохранённый ответ (в т.ч. ошибку), затем валидация quote/баланса | `[VERIFY]` | Level 3 + типичная идемпотентность |
| 4.5 | Одноразовость quote | Одна успешная оплата на quote; повтор с другим ключом → `QUOTE_ALREADY_USED` (свой код) | `[VERIFY]` | PDF «по конкретной котировке» |
| 4.6 | Неизвестный client_id | **404** или **422** + `VALIDATION_ERROR` | `[VERIFY]` | Нет кода в PDF |
| 4.7 | Нет баланса в валюте source | `INSUFFICIENT_FUNDS` (available=0) | `[VERIFY]` | |
| 4.8 | Резерв | Блокируем **полный** `source_amount` (gross, с комиссией клиента) | `[VERIFY]` | PDF «деньги клиента резервируются»; chaos_run debited = source_amount |
| 4.9 | Успех | Списание резерва (debit), available уменьшается на source_amount | `[VERIFY]` | chaos_run invariant |
| 4.10 | FAILED | Резерв возвращается в available | `[CONFIRMED]` | PDF §2 |
| 4.11 | Направление без провайдеров (RU-AE-BANK, RU-PH-CASH, KZ-TH-BANK) | **422/503 до создания transfer** с `UNSUPPORTED_DIRECTION` или `NO_PROVIDER_AVAILABLE` — **не** создаём transfer без id | `[VERIFY]` | `chaos_run.py` шлёт violation `api_errors`, если нет `transfer_id` |
| 4.12 | POST /transfers HTTP | **202 Accepted** + `{transfer_id, status}`; клиент опрашивает GET | `[VERIFY]` | PDF «решите сами»; slow 30s — см. DECISIONS |
| 4.13 | Цена перевода | Строго суммы из quote (не пересчитываем) | `[CONFIRMED]` | PDF Level 2 |

---

## 5. Суточный лимит (Level 2–3)

| # | Неоднозначность | Допущение | Статус | Обоснование |
|---|-----------------|-----------|--------|-------------|
| 5.1 | Окно | Календарные **UTC-сутки** `[00:00, 24:00)` | `[VERIFY]` | PDF «суточные лимиты» |
| 5.2 | Что учитываем | Сумма **gross source_amount** переводов в статусах не-FAILED: RESERVED/PROCESSING/COMPLETED/NEEDS_REVIEW; FAILED не считаем | `[VERIFY]` | Level 3 вопрос |
| 5.3 | Валюта лимита ≠ валюта списания | Конвертируем source_amount в валюту `daily_limit.currency` по **тому же курсу, что в quote** (или актуальному на момент резерва) | `[VERIFY]` | `clients.json` c_003: RUB balance, USD limit |
| 5.4 | Приоритет ошибок | Сначала `DAILY_LIMIT_EXCEEDED`, затем `INSUFFICIENT_FUNDS` (если оба) | `[VERIFY]` | Лимит — регуляторное ограничение |

---

## 6. Провайдеры, маржа, fallback

| # | Неоднозначность | Допущение | Статус | Обоснование |
|---|-----------------|-----------|--------|-------------|
| 6.1 | Сумма выплаты провайдеру | `quote.target_amount` / `target_currency` (строки 1:1) | `[CONFIRMED]` | PDF §7; `chaos_run` amount_mismatch |
| 6.2 | Комиссия провайдера для маржи | `provider_fee = round(payout_in_provider_currency × pct/100 + fixed, provider.currency)`; payout конвертируем из target в `provider.currency` | `[VERIFY]` | PDF §5 «комиссия в валюте провайдера» |
| 6.3 | Маржа | `(client_fee_in_source − provider_fee_in_source) / source_amount × 100 ≥ min_margin_percent` после приведения provider_fee к source | `[VERIFY]` | PDF §3 «маржа» |
| 6.4 | Fallback ниже min_margin | **Запрещён** для первичного выбора; **разрешён** только если все подходящие провайдеры исчерпаны (decline/5xx/лимиты) и иначе transfer FAILED | `[VERIFY]` | PDF «Можно ли брать запасным… решите» |
| 6.5 | Фильтр | direction, enabled, не в maintenance, лимиты провайдера (как mock ±10%), маржа (6.3) для primary | `[CONFIRMED]` | PDF Level 2; mock limits |
| 6.6 | Скoring | Взвешенная сумма: низкая стоимость (40%), success_rate (35%), низкая p95_latency (25%); детали в DECISIONS | `[VERIFY]` | PDF «ваше решение» |
| 6.7 | `RECIPIENT_ACCOUNT_INVALID` (422 sync) | Считаем отказом получателя → **следующий** провайдер | `[CONFIRMED]` | PDF Level 2 |
| 6.8 | `CURRENCY_NOT_SUPPORTED`, `AMOUNT_OUT_OF_LIMITS` | Провайдер не подходит → skip, не terminal для transfer | `[CONFIRMED]` | mock 422 коды |
| 6.9 | 503 maintenance/disabled | Skip провайдера | `[CONFIRMED]` | mock |
| 6.10 | Idempotency к провайдеру | Один `reference` (= наш transfer_id) и один Idempotency-Key на попытку; при unknown outcome — **retry тому же** провайдеру с тем же ключом | `[CONFIRMED]` | mock `main.py:148–151` |

---

## 7. Неизвестный исход, webhook, Level 3

| # | Неоднозначность | Допущение | Статус | Обоснование |
|---|-----------------|-----------|--------|-------------|
| 7.1 | Классы исхода POST payout | **Definite fail:** 422 decline, 5xx без создания (нет replay 201). **Definite ok:** 201 + payout_id. **Unknown:** обрыв, 504, hang, drop_created | `[CONFIRMED]` | `behavior.py`, `main.py` |
| 7.2 | Unknown | Poll `GET …/payouts?reference=`; 404 **не** означает «нет выплаты» (visibility_lag до ~15s) | `[CONFIRMED]` | `behavior.py:83–84`, `store.py:41–42` |
| 7.3 | Fallback после unknown | Только если установлено «выплата не создана»; иначе **NEEDS_REVIEW**, резерв удерживаем | `[VERIFY]` | Level 3, anti double payout |
| 7.4 | Webhook X-Signature | **Обязательна** проверка HMAC-SHA256; без/неверная подпись — игнор | `[VERIFY]` | PDF «плюс, но не обязательна»; chaos forged webhooks |
| 7.5 | Дубликат webhook | Идемпотентно по `(provider_id, event_id)`; **200** | `[CONFIRMED]` | Level 3 |
| 7.6 | Неизвестная выплата | **200**, без изменения state | `[VERIFY]` | Level 3 |
| 7.7 | Out-of-order / contradiction | Терминальный статус transfer **не откатываем**; поздний PROCESSING игнорируем; противоречие → reconcile / NEEDS_REVIEW | `[VERIFY]` | mock `deliver_final` contradiction |
| 7.8 | stale_read провайдера | GET может показывать PROCESSING после COMPLETED 1–3s — опираемся на webhook+reconcile | `[CONFIRMED]` | `store.py:36–38` |
| 7.9 | NEEDS_REVIEW | Явный терминальный(или quasi-terminal) статус для chaos_run `--review-state` | `[VERIFY]` | `chaos_run.py:17–27` |
| 7.10 | Рестарт процесса | Незавершённые transfer возобновляет фоновый worker из БД | `[VERIFY]` | Level 3 |

---

## 8. Инфраструктура и API

| # | Неоднозначность | Допущение | Статус | Обоснование |
|---|-----------------|-----------|--------|-------------|
| 8.1 | БД | PostgreSQL в docker-compose; миграции при старте | `[CONFIRMED]` | `docker-compose.yml` |
| 8.2 | Балансы клиентов | Инициализация из `data/clients.json` **один раз**; дальше только ledger; рестарт не сбрасывает | `[VERIFY]` | PDF + persistence |
| 8.3 | callback_url | Конфиг `API_PUBLIC_URL` → `http://api:8080/webhooks/{provider_id}` | `[VERIFY]` | docker network |
| 8.4 | Dockerfile api | Добавляем в реализации (в стартере отсутствует) | `[CONFIRMED]` | compose `build: .` |
| 8.5 | HTTP-коды ошибок | 400 validation; 404 quote/transfer not found; 409 idempotency; 422 business (amount, funds); 503 rate/provider | `[VERIFY]` | PDF не фиксирует; согласуем в contracts |
| 8.6 | GET /clients/{id}/balance | JSON с полями **`available`** и **`reserved`** по каждой валюте (формат — contracts) | `[CONFIRMED]` | `chaos_run.py:28–29` |
| 8.7 | transfer statuses | `PROCESSING`, `COMPLETED`, `FAILED`, `NEEDS_REVIEW` (+ опционально `CANCELLED` не используем) | `[VERIFY]` | chaos_run SUCCESS/FAILURE sets |

## Gate 1 (самопроверка полноты)

- Покрыты разделы плана: деньги, quotes, курсы, transfer, лимит, провайдеры, L3, infra.
- Каждый `[VERIFY]` имеет кандидата в SPEC (AC) или DECISIONS (алгоритм).
- Арифметика сверена с 10 файлами `examples/`.

**Вердикт:** готово к утверждению SPEC/DECISIONS (ожидается подтверждение исполнителя).
