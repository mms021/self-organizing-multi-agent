# aichatdeck

Go-реализация Core Loop MVP self-organizing multi-agent platform. Полный дизайн — в [docs/README.md](docs/README.md) (12 RFC + 4 ADR).

## Запуск

```bash
docker run -d --rm -p 6379:6379 redis:7-alpine
go run ./cmd/server
```

Проверка: `./scripts/smoke.sh` (нужен `curl`, `jq`, сервер на `localhost:8080`).

## Тесты

```bash
CGO_ENABLED=0 go build ./...
go vet ./...
go test ./...
```

