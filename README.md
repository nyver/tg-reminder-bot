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
| `/tv <программа>` | найти программу на всех каналах на ближайшую неделю |
| `/tv <программа> \| <канал>` | найти программу на указанном канале на ближайшую неделю |

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

Расписание можно запросить независимо от напоминаний:

```text
/tv КВН
/tv КВН | Первый канал
```

Первый вариант ищет выпуски на всех каналах, второй ограничивает результат наиболее
подходящим по названию каналом. Время показывается в часовом поясе пользователя из `/tz`.

## Быстрый запуск в Docker

Требуются Docker и Docker Compose.

```bash
cp config.yaml.example config.yaml
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

TV-провайдер интегрирован с [EPG Service API](https://epgservice.ru/en/docs/).
Он разрешает название канала через `/v1/index`, загружает недельное расписание
из `/v1/schedule/{channel_id}` и ищет передачу в заданном временном окне.

Docker-образ содержит резервный `/app/config.yaml` с безопасными значениями по
умолчанию. Docker Compose монтирует локальный `config.yaml` поверх него в режиме
read-only:

```yaml
volumes:
  - reminddata:/data
  - ./config.yaml:/app/config.yaml:ro
```

Поэтому перед первым запуском Compose создайте `config.yaml` из примера. Секреты
можно хранить в нём или передавать через `.env`; непустые переменные окружения
имеют приоритет над YAML.

Секреты и deployment-настройки можно переопределять переменными окружения:

| Переменная | Назначение |
| --- | --- |
| `TELEGRAM_TOKEN` | токен Telegram-бота |
| `LLM_API_KEY` | ключ OpenRouter или Anthropic |
| `EPG_SERVICE_API_KEY` | Bearer-токен EPG Service |
| `EPG_SERVICE_BASE_URL` | переопределение корневого URL EPG Service API |
| `IPTVX_EPG_URL` | URL XMLTV/XMLTV.GZ для основного TV-провайдера |
| `IPTVX_EPG_FILE` | путь к локальному кешу IPTVX EPG |
| `DATABASE_DRIVER` | `sqlite` или `postgres` |
| `DATABASE_DSN` | путь SQLite или PostgreSQL DSN |
| `DATABASE_URL` | PostgreSQL URL с наивысшим приоритетом |
| `LOG_LEVEL` | `debug`, `info`, `warn` или `error` |

## TV-расписание

По умолчанию worker скачивает XMLTV-расписание IPTVX, сохраняет исходный файл в локальный
кеш и импортирует каналы и программы в общую базу данных. Эта база используется как для
TV-напоминаний, так и командой `/tv` в процессе bot. Поэтому bot и worker должны работать
с одной SQLite-базой или с одним экземпляром PostgreSQL.

Настройки основного провайдера:

```yaml
providers:
  iptvx:
    url: https://iptvx.one/epg/epg.xml.gz
    file_path: ./data/iptvx_epg.xml.gz
    update_interval: 168h
    timeout: 120s
```

При запуске в Docker используйте путь `/data/iptvx_epg.xml.gz`: каталог `/data` подключён
к persistent volume. Первый импорт может занять некоторое время; до его завершения `/tv`
вернёт пустой результат.

Поля секции `providers.iptvx`:

| Поле | Описание | Значение по умолчанию |
| --- | --- | --- |
| `url` | адрес XMLTV или XMLTV.GZ; пустое значение включает резервный EPG Service | `https://iptvx.one/epg/epg.xml.gz` |
| `file_path` | путь к кешу скачанного расписания | `./data/iptvx_epg.xml.gz` |
| `update_interval` | период проверки и обновления EPG | `168h` |
| `timeout` | таймаут скачивания EPG-файла | `120s` |

`IPTVX_EPG_URL` и `IPTVX_EPG_FILE` переопределяют соответствующие значения YAML.
В текущем `docker-compose.yml` эти переменные не пробрасываются автоматически, поэтому
для Compose удобнее задавать параметры IPTVX непосредственно в `config.yaml`.

Если `providers.iptvx.url` пуст, worker переключается на EPG Service. Этот режим требует
Bearer-токен и поддерживает TV-напоминания, но не импортирует расписание в локальную базу,
поэтому команда `/tv` не получает из него данные:

```yaml
providers:
  iptvx:
    url: ""
  tv:
    base_url: https://api.epgservice.ru
    api_key: "${EPG_SERVICE_API_KEY}"
    timeout: 15s
```

`EPG_SERVICE_API_KEY` переопределяет ключ из YAML, а `EPG_SERVICE_BASE_URL` —
`providers.tv.base_url`. EPG Service кеширует индекс каналов на один час и запрашивает
расписание отдельно для каждой недели во временном окне напоминания.

Для TV-напоминания NLU формирует параметры `channel` и название передачи:

```json
{
  "type": "tv_program",
  "title": "КВН",
  "params": {"channel": "Первый канал"}
}
```

Если ID канала уже известен, вместо поиска по названию можно передать
`params.channel_id`. Поле `params.channel` используется для поиска канала по
названию. Для TV-напоминания необходимо указать хотя бы одно из этих полей;
команда `/tv` также умеет искать программу сразу по всем каналам.

## LLM-провайдер

OpenRouter используется по умолчанию:

```yaml
nlu:
  provider: openrouter
  api_key: "${LLM_API_KEY}"
  openrouter:
    base_url: https://openrouter.ai/api/v1
    model: anthropic/claude-haiku-4.5
    fallback_models:
      - mistralai/mistral-7b-instruct:free
      - meta-llama/llama-3.2-3b-instruct:free
    timeout: 60s
    max_tokens: 1024
```

При ответе HTTP 429 от основной модели OpenRouter последовательно пробуются модели из
`fallback_models`. `timeout` ограничивает всё обращение к LLM, а `max_tokens` — размер ответа.

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
