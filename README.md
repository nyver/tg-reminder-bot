# Reminder Telegram Bot

Telegram-бот для обычных, периодических и условных напоминаний. Проект включает
Telegram polling, фоновую обработку напоминаний, HTTP API, метрики Prometheus и
CLI для миграций и административных операций.

SQLite используется по умолчанию. Отдельный сервер базы данных для стандартного
запуска не требуется. При локальном запуске база хранится в `data/remind.db`,
в Docker — в `/data/remind.db` внутри persistent volume.

## Компоненты

- `bot` принимает команды Telegram и распознаёт текст напоминания.
- `worker` проверяет условия и отправляет запланированные уведомления.
- `api` предоставляет HTTP API, health checks и метрики.
- `remindctl` запускает миграции и административные команды.

## Команды Telegram-бота

| Команда | Назначение |
| --- | --- |
| `/start` | зарегистрировать пользователя и показать краткую справку |
| `/help` | показать список команд и примеры напоминаний |
| `/list` | вывести активные и приостановленные напоминания с их ID |
| `/cancel <id>` | отменить напоминание |
| `/remove <id>` | безвозвратно удалить напоминание и связанные данные |
| `/pause <id>` | временно приостановить напоминание |
| `/resume <id>` | возобновить приостановленное напоминание |
| `/tz` | показать текущий часовой пояс |
| `/tz <зона>` | установить часовой пояс в формате IANA, например `Europe/Moscow` |

ID для команд `/cancel`, `/remove`, `/pause` и `/resume` можно получить командой
`/list`. Команда `/cancel` сохраняет напоминание в базе со статусом `done`, а
`/remove` физически удаляет его вместе с уведомлениями и историей наблюдений.

Для создания напоминания отдельная команда не нужна: отправьте боту описание
обычным текстом. Бот распознает параметры, при необходимости задаст уточняющий
вопрос и предложит подтвердить создание кнопкой.

Примеры сообщений:

```text
напомни завтра в 9:00 позвонить маме
каждый понедельник в 8:30 напоминай про совещание
уведоми за 3 часа до КВН на Первом
уведоми при снижении цены: https://example.com/product
каждый день в 9:00 покажи 5 дешёвых билетов из Москвы в Казань на месяц вперёд
```

## Быстрый запуск в Docker

Требуются Docker и Docker Compose.

```bash
cp .env.example .env
```

Укажите в `.env` как минимум:

```dotenv
TELEGRAM_TOKEN=your-telegram-token
LLM_API_KEY=your-openrouter-api-key
```

Запустите приложение:

```bash
docker compose up --build -d
docker compose ps
docker compose logs -f bot worker api
```

Compose автоматически:

- создаёт volume `reminddata`;
- хранит SQLite в `/data/remind.db`;
- применяет миграции перед запуском сервисов;
- публикует API на `http://localhost:8080`.

Проверка API:

```bash
curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
```

Остановка приложения:

```bash
docker compose down
```

Для удаления SQLite вместе с volume:

```bash
docker compose down -v
```

## Конфигурация

Полный пример находится в `config.yaml.example`. Приложения читают
`config.yaml` из текущего каталога или файл, указанный в `CONFIG_FILE`.

Основные разделы YAML:

- `database` — драйвер и DSN базы данных;
- `telegram` — токен Telegram;
- `nlu` — LLM-провайдер, ключ и модель;
- `providers` — настройки внешних источников;
- `scheduler` — интервалы фоновых задач;
- `server` — порт API, worker ID и уровень логирования.

Docker-образ содержит рабочий `/app/config.yaml` с безопасными значениями по
умолчанию. Для собственного YAML добавьте read-only mount в `x-app.volumes`:

```yaml
volumes:
  - reminddata:/data
  - ./config.yaml:/app/config.yaml:ro
```

Секреты и deployment-настройки можно переопределять переменными окружения:

| Переменная | Назначение |
| --- | --- |
| `TELEGRAM_TOKEN` | токен Telegram-бота |
| `LLM_API_KEY` | ключ OpenRouter или Anthropic |
| `DATABASE_DRIVER` | `sqlite` или `postgres` |
| `DATABASE_DSN` | путь SQLite или PostgreSQL DSN |
| `DATABASE_URL` | PostgreSQL URL с наивысшим приоритетом |
| `LOG_LEVEL` | `debug`, `info`, `warn` или `error` |

## LLM-провайдер

OpenRouter используется по умолчанию:

```yaml
nlu:
  provider: openrouter
  api_key: "${LLM_API_KEY}"
  openrouter:
    base_url: https://openrouter.ai/api/v1
    model: anthropic/claude-haiku-4.5
```

Для прямого подключения к Anthropic:

```yaml
nlu:
  provider: claude
  api_key: "${LLM_API_KEY}"
  claude:
    model: claude-haiku-4-5-20251001
```

## PostgreSQL

PostgreSQL не входит в Compose и не запускается по умолчанию. Для подключения к
существующему серверу задайте URL:

```dotenv
DATABASE_URL=postgres://user:password@host:5432/remind?sslmode=disable
```

`DATABASE_URL` автоматически выбирает драйвер `postgres`. Мигратор применит
PostgreSQL-версию схемы при следующем запуске.

## Локальная разработка

Требуется Go 1.25 или новее.

```bash
cp config.yaml.example config.yaml
go mod download
go run ./cmd/remindctl migrate up
```

Сервисы запускаются отдельными процессами:

```bash
go run ./cmd/bot
go run ./cmd/worker
go run ./cmd/api
```

Тесты и статический анализ:

```bash
go test ./...
go vet ./...
```

## CLI

```text
remindctl migrate up|down|status
remindctl reminders list --user <telegram-user-id>
remindctl notifications retry <notification-id>
remindctl provider travel --from <city> --to <city>
remindctl version
```

## HTTP endpoints

- `GET /healthz` — liveness check.
- `GET /readyz` — readiness check.
- `GET /metrics` — метрики Prometheus.
- `GET /api/users/{id}/reminders` — напоминания пользователя.
- `GET /api/reminders/{id}` — напоминание по ID.
- `GET /api/reminders/{id}/observations` — история наблюдений.
- `POST /api/reminders/{id}/cancel` — отмена напоминания.
- `GET /api/notifications` — уведомления.
- `POST /api/notifications/{id}/retry` — повторная отправка.
