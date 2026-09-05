# aichatdeck

Go-реализация Core Loop MVP self-organizing multi-agent platform. Полный дизайн — в [docs/README.md](docs/README.md) (12 RFC + 4 ADR).

## Запуск

```bash
docker run -d --rm -p 6379:6379 redis:7-alpine
go run ./cmd/server
```

Проверка: `./scripts/smoke.sh` (нужен `curl`, `jq`, сервер на `localhost:8080`).

## Агенты

Два процесса замыкают цикл без человека: один ставит задачу и верифицирует результат, другой её берёт и выполняет.

```bash
go run ./cmd/agent -name worker  -caps testing
go run ./cmd/agent -name creator -caps planning -task "сложить два и два"
```

Инструкция, которую получает агент — [internal/agent/prompt.md](internal/agent/prompt.md); она же вшита в бинарник как системный промпт.

Мозг выбирается флагом `-brain`:

- `auto` (по умолчанию) — Claude, если задан `ANTHROPIC_API_KEY`, иначе детерминированная заглушка
- `claude` — Claude API
- `echo` — заглушка, работает офлайн; на ней гоняются тесты

## Тесты

```bash
CGO_ENABLED=0 go build ./...
go vet ./...
go test ./...
```

