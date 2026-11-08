## Запуск

```bash
docker compose up -d --build --wait
```

Проверка: `GET http://localhost:8080/health` отвечает `ok`.

Секрет вебхука по умолчанию `local-dev-webhook-secret`. Свой задаётся переменной `PROVIDER_WEBHOOK_SECRET` до `docker compose up`.

## Тесты

Поднять Postgres и mock, затем:

```bash
docker compose up -d --wait db mock-provider
```

```bash
DATABASE_URL=postgresql://transfer:transfer@localhost:5432/transfer MOCK_PROVIDER_URL=http://localhost:9000 go test ./... -count=1
```

В `docker-compose.yml` порт Postgres наружу не проброшен. Для тестов с хоста добавьте сервису `db` порт `5432:5432`.

## Chaos

```bash
docker compose down -v
MOCK_MODE=chaos MOCK_SEED=1 docker compose up -d --build --wait
python3 scripts/chaos_run.py --transfers 300
```

Успех: последняя строка `RESULT: OK`, код выхода 0.
