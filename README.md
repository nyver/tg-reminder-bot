# Reminder Telegram Bot

A Telegram bot for one-off, recurring, and conditional reminders. The project
includes Telegram polling, background reminder processing, an HTTP API,
Prometheus metrics, and a CLI for migrations and administrative operations.

SQLite is used by default, so no separate database server is required for a
standard deployment. For a local run the database is stored in
`data/remind.db`; in Docker it lives in `/data/remind.db` inside a persistent
volume.

## Components

- `bot` accepts Telegram commands and recognizes reminder text.
- `worker` evaluates conditions and sends scheduled notifications.
- `api` provides the HTTP API, health checks, and metrics.
- `remindctl` runs migrations and administrative commands.

## Telegram bot commands

| Command | Purpose |
| --- | --- |
| `/start` | register the user and show a short help message |
| `/help` | show the list of commands and reminder examples |
| `/list` | list active and paused reminders with their IDs |
| `/cancel <id>` | cancel a reminder |
| `/remove <id>` | permanently delete a reminder and its related data |
| `/pause <id>` | temporarily pause a reminder |
| `/resume <id>` | resume a paused reminder |
| `/refresh <id>` | fetch the current price right now (price reminders only) |
| `/run <id>` | force-evaluate any reminder right now and send the result immediately (e.g. generate an RSS digest on demand) |
| `/tz` | show the current timezone |
| `/tz <zone>` | set the timezone in IANA format, e.g. `Europe/Moscow` |
| `/tv <program>` | find a program on any channel over the next week |
| `/tv <program> \| <channel>` | find a program on a specific channel over the next week |
| `/rss <url>` | subscribe to a periodic RSS/Atom news digest, delivered daily at 09:00 (top 5) |
| `/rss <url> \| HH:MM` | same, with a custom delivery time |
| `/rss <url> \| HH:MM \| N` | same, with a custom delivery time and item count (1–20) |

IDs for `/cancel`, `/remove`, `/pause`, and `/resume` can be obtained with
`/list`. `/cancel` marks the reminder `done` in the database, while `/remove`
physically deletes it along with its notifications and observation history.

Creating a reminder does not require a dedicated command: just send the bot a
plain-text description. The bot recognizes the parameters, asks a
clarifying question if needed, and offers a confirmation button before
creating it.

Example messages (the bot's NLU understands Russian):

```text
напомни завтра в 9:00 позвонить маме
каждый понедельник в 8:30 напоминай про совещание
уведоми за 3 часа до КВН на Первом
уведоми при снижении цены: https://example.com/product
каждый день в 9:00 покажи 5 дешёвых билетов из Москвы в Казань на месяц вперёд
каждый день в 18:00 создай дайджест новостей на основе https://lenta.ru/rss
```

The last example creates a periodic RSS news digest — see
[RSS news digest](#rss-news-digest) below for details; the same reminder can
also be created directly with `/rss https://lenta.ru/rss | 18:00`.

TV schedules can also be queried independently of reminders:

```text
/tv КВН
/tv КВН | Первый канал
```

The first form searches all channels; the second narrows the result to the
channel whose name best matches. Times are shown in the user's timezone,
configured via `/tz`.

## Quick start with Docker

Docker and Docker Compose are required.

```bash
cp config.yaml.example config.yaml
cp .env.example .env
```

Set at least the following in `.env`:

```dotenv
TELEGRAM_TOKEN=your-telegram-token
LLM_API_KEY=your-openrouter-api-key
```

Start the application:

```bash
docker compose up --build -d
docker compose ps
docker compose logs -f bot worker api
```

Compose automatically:

- creates the `reminddata` volume;
- stores SQLite in `/data/remind.db`;
- applies migrations before starting the services;
- publishes the API on `http://localhost:8080`.

API checks:

```bash
curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
curl -H "Authorization: Bearer $ADMIN_API_TOKEN" http://localhost:8080/api/notifications?status=failed
```

Stop the application:

```bash
docker compose down
```

To remove SQLite along with the volume:

```bash
docker compose down -v
```

## Configuration

The full example is in `config.yaml.example`. The applications read
`config.yaml` from the current directory, or the file specified in
`CONFIG_FILE`.

Main YAML sections:

- `database` — database driver and DSN;
- `telegram` — Telegram token;
- `nlu` — LLM provider, key, and model;
- `providers` — external source settings;
- `scheduler` — background task intervals;
- `server` — API port, worker ID, and log level.

The TV provider integrates with the
[EPG Service API](https://epgservice.ru/en/docs/). It resolves the channel
name via `/v1/index`, loads the weekly schedule from
`/v1/schedule/{channel_id}`, and looks up the program within the requested
time window.

When displaying a day's schedule, the bot hides programs that have already
ended (relative to the user's timezone from `/tz`).

The Docker image is built on `debian:bookworm-slim` and includes Chromium for
`price.headless: true` mode. With `headless: false` (the default), Chromium
does not start.

The Docker image ships a fallback `/app/config.yaml` with safe defaults.
Docker Compose mounts the local `config.yaml` over it in read-only mode:

```yaml
volumes:
  - reminddata:/data
  - ./config.yaml:/app/config.yaml:ro
```

So create `config.yaml` from the example before the first Compose run.
Secrets can be stored there or passed via `.env`; non-empty environment
variables take priority over YAML.

Secrets and deployment settings can be overridden with environment
variables:

| Variable | Purpose |
| --- | --- |
| `TELEGRAM_TOKEN` | Telegram bot token |
| `LLM_API_KEY` | OpenRouter or Anthropic key |
| `EPG_SERVICE_API_KEY` | EPG Service Bearer token |
| `EPG_SERVICE_BASE_URL` | override for the EPG Service API base URL |
| `ADMIN_API_TOKEN` | Bearer token for `/api/*` and `/metrics`; the admin API is disabled without it |
| `API_BIND` | address the API is published on in Docker Compose, default `127.0.0.1` |
| `API_PORT` | external API port in Docker Compose, default `8080` |
| `IPTVX_EPG_URL` | XMLTV/XMLTV.GZ URL for the primary TV provider |
| `IPTVX_EPG_FILE` | path to the local IPTVX EPG cache |
| `DATABASE_DRIVER` | `sqlite` or `postgres` |
| `DATABASE_DSN` | SQLite path or PostgreSQL DSN |
| `DATABASE_URL` | PostgreSQL URL, highest priority |
| `LOG_LEVEL` | `debug`, `info`, `warn`, or `error` |

## TV schedule

By default the worker downloads the IPTVX XMLTV schedule, caches the raw
file locally, and imports channels and programs into the shared database.
This database is used both for TV reminders and for the `/tv` command in the
bot process. Therefore bot and worker must share the same SQLite database or
the same PostgreSQL instance.

Primary provider settings:

```yaml
providers:
  iptvx:
    url: https://iptvx.one/epg/epg.xml.gz
    file_path: ./data/iptvx_epg.xml.gz
    update_interval: 168h
    timeout: 120s
```

When running in Docker, use the `/data/iptvx_epg.xml.gz` path: the `/data`
directory is mounted on the persistent volume. The first import can take a
while; until it finishes, `/tv` returns an empty result.

Fields of the `providers.iptvx` section:

| Field | Description | Default |
| --- | --- | --- |
| `url` | XMLTV or XMLTV.GZ address; an empty value enables the EPG Service fallback | `https://iptvx.one/epg/epg.xml.gz` |
| `file_path` | path to the downloaded schedule cache | `./data/iptvx_epg.xml.gz` |
| `update_interval` | EPG check-and-refresh period | `168h` |
| `timeout` | EPG file download timeout | `120s` |

`IPTVX_EPG_URL` and `IPTVX_EPG_FILE` override the corresponding YAML values.
The current `docker-compose.yml` does not pass these variables through
automatically, so for Compose it's more convenient to set IPTVX parameters
directly in `config.yaml`.

If `providers.iptvx.url` is empty, the worker switches to the EPG Service.
This mode requires a Bearer token and supports TV reminders, but does not
import the schedule into the local database, so the `/tv` command gets no
data from it:

```yaml
providers:
  iptvx:
    url: ""
  tv:
    base_url: https://api.epgservice.ru
    api_key: "${EPG_SERVICE_API_KEY}"
    timeout: 15s
```

`EPG_SERVICE_API_KEY` overrides the key from YAML, and
`EPG_SERVICE_BASE_URL` overrides `providers.tv.base_url`. The EPG Service
caches the channel index for one hour and requests the schedule separately
for each week within the reminder's time window.

For a TV reminder, the NLU produces `channel` and program-title parameters:

```json
{
  "type": "tv_program",
  "title": "КВН",
  "params": {"channel": "Первый канал"}
}
```

If the channel ID is already known, `params.channel_id` can be passed
instead of searching by name. `params.channel` is used to look up the
channel by name. A TV reminder needs at least one of these fields; the `/tv`
command can also search a program across all channels at once.

## Price monitoring

The bot tracks a price drop for a product on an online store's page. To
create a reminder, just send a link and a keyword:

```text
уведоми при снижении цены https://example.com/product
подешевеет ли https://www.ozon.ru/product/... — напомни
уведоми при снижении цены https://example.com/product каждые 4 часа
уведоми при снижении цены https://example.com/product каждые 30 минут
```

The NLU automatically extracts the URL and builds a conditional reminder.
The worker periodically checks the page and sends a notification when the
price drops.

### Poll interval

The check interval can be specified directly in the reminder text:

| Phrase | Schedule |
| --- | --- |
| `каждый час` / `раз в час` | every hour |
| `каждые 2 часа` | every 2 hours |
| `каждые 30 минут` | every 30 minutes |
| _(not specified)_ | `providers.price.poll_cron` from the config |

If the user does not specify an interval, `poll_cron` from the config is
used (`"0 * * * *"` — hourly — by default). The confirmation dialog always
shows the resulting interval before creating the reminder.

### Price fetch error notification

If the bot gets an HTTP error while polling the page, the user receives a
notification with the response code:

```
⚠️ Не удалось получить текущую цену
https://example.com/product

HTTP статус: 403

Бот продолжит проверять и уведомит при снижении цены.
```

This notification is sent at most once a day per reminder. On a network
error (no response from the server) the status is omitted.

### Provider settings

```yaml
providers:
  price:
    user_agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/125.0 Safari/537.36
    timeout: 15s
    headless: false
    proxy_url: ""
    poll_cron: "0 9 * * *"
```

| Field | Description | Default |
| --- | --- | --- |
| `user_agent` | User-Agent for HTTP requests to store pages | Chrome/Windows string |
| `timeout` | timeout for fetching the page (HTTP or headless) | `15s` |
| `headless` | use Chromium to bypass WAF/TLS fingerprinting | `false` |
| `proxy_url` | proxy for the headless browser (HTTP, HTTPS, or SOCKS5) | `""` |
| `poll_cron` | default poll schedule (5-field cron, UTC) | `"0 9 * * *"` |

Some stores (e.g. DNS-shop) block plain HTTP requests by TLS fingerprint.
Enable `headless: true` to use a real browser stack. The Docker image
includes Chromium; the first request in this mode takes 1–2 seconds to
launch the browser, subsequent ones are much faster thanks to the shared
Chrome process.

## RSS news digest

The bot can turn any RSS 2.0 or Atom feed into a periodic "important news"
digest, delivered once a day at a chosen time. The simplest way is the
dedicated command:

```text
/rss https://lenta.ru/rss                       → daily digest at 09:00, top 5
/rss https://lenta.ru/rss | 08:30                → custom delivery time
/rss https://lenta.ru/rss | 08:30 | 10           → custom time and top-10 items
```

The same reminder can also be created from a plain-text message, without
using the command — the NLU recognizes phrases that combine a feed link with
a digest request and a time:

```text
каждый день в 18:00 создай дайджест новостей на основе https://lenta.ru/rss
дайджест новостей топ 10 по ленте https://lenta.ru/rss в 8 утра
```

If no time is given, the digest defaults to 09:00; if no item count is
given, it defaults to top 5. `/list`, `/pause`, `/resume`, `/cancel`, and
`/remove` manage an RSS digest reminder the same way as any other
conditional reminder.

To generate a digest immediately instead of waiting for its scheduled time,
use `/run <id>` with the ID from `/list`:

```text
/run 3fa85f64-5717-4562-b3fc-2c963f66afa6
```

`/run` re-fetches the feed and sends the digest right away. Unlike the
scheduled delivery, it is never skipped because "today's digest already went
out" — it always sends a fresh result (or an explanatory message if the feed
has no items). This works the same way for other reminder types too: `/run`
re-checks a price-drop or TV-anchor reminder on demand.

### Importance ranking

By default, importance is a fixed heuristic: a curated keyword match (e.g.
"срочно", "экстренно", "погибли", "взрыв", "кризис", "breaking", "urgent")
adds to the score, and recency adds up to a few more points, decaying
linearly to zero over a 7-day window. Items are deduplicated by link and
sorted by score, descending.

Optionally, the LLM configured under `nlu:` can take over ranking and
summarization instead: enable `providers.rss.llm_digest: true` and, on each
digest tick, the heuristic's top candidates (up to 3× the digest's item
count) are handed to the LLM, which picks the genuinely most important ones
and writes a fresh 2–3 sentence summary for each. This is off by default
because it adds one LLM call per digest tick; if that call fails or is
unavailable, the digest silently falls back to the heuristic ranking instead
of failing outright.

```yaml
providers:
  rss:
    llm_digest: true
```

### SSRF protection

A feed URL is user-supplied input the server makes an outbound request to —
the same risk class as the price-monitoring URL. `internal/netsafe`
provides a hardened HTTP client shared by the RSS provider: it rejects
unsupported URL schemes, `localhost`, private/loopback/link-local
addresses, and re-validates the resolved IP at connect time to close the
DNS-rebinding window.

### Provider settings

```yaml
providers:
  rss:
    timeout: 15s
    llm_digest: false
    proxy_url: ""
```

| Field | Description | Default |
| --- | --- | --- |
| `timeout` | timeout for fetching and parsing one RSS/Atom feed | `15s` |
| `llm_digest` | use the `nlu:` LLM to rank and summarize digest items instead of the heuristic | `false` |
| `proxy_url` | HTTP, HTTPS, or SOCKS5 proxy for fetching feeds | `""` (direct) |

Some feeds block requests from datacenter/VPS IP ranges outright — a feed
that hangs and times out on every attempt from your server, but works fine
from a browser, is a sign of this. `proxy_url` routes all RSS/Atom fetches
through a proxy instead:

```yaml
providers:
  rss:
    proxy_url: http://user:pass@proxy.example.com:3128
    # or: socks5://user:pass@proxy.example.com:1080
```

Unlike the direct-fetch path, a proxied request is not subject to this
provider's own SSRF dial guard (see [SSRF protection](#ssrf-protection)
above) — the proxy, not this process, resolves and connects to the
destination. The operator who configures `proxy_url` is trusted not to point
it at an SSRF pivot, the same trust boundary `providers.price.proxy_url`
already relies on.

## LLM provider

OpenRouter is used by default:

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

On an HTTP 429 from the primary model, OpenRouter tries the models in
`fallback_models` in order. `timeout` bounds the whole LLM call, and
`max_tokens` bounds the response size.

For a direct connection to Anthropic:

```yaml
nlu:
  provider: claude
  api_key: "${LLM_API_KEY}"
  claude:
    model: claude-haiku-4-5-20251001
```

## PostgreSQL

PostgreSQL is not part of Compose and does not start by default. To connect
to an existing server, set the URL:

```dotenv
DATABASE_URL=postgres://user:password@host:5432/remind?sslmode=disable
```

`DATABASE_URL` automatically selects the `postgres` driver. The migrator
applies the PostgreSQL schema version on the next run.

## Local development

Go 1.26 or newer is required.

```bash
cp config.yaml.example config.yaml
go mod download
go run ./cmd/remindctl migrate up
```

Services run as separate processes:

```bash
go run ./cmd/bot
go run ./cmd/worker
go run ./cmd/api
```

Tests, formatting, and static analysis:

```bash
go test ./... -race -count=1
go vet ./...
gofmt -s -w .
golangci-lint run ./...
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
- `GET /metrics` — Prometheus metrics, requires `Authorization: Bearer <ADMIN_API_TOKEN>`.
- `GET /api/users/{id}/reminders` — a user's reminders.
- `GET /api/reminders/{id}` — a reminder by ID.
- `GET /api/reminders/{id}/observations` — observation history.
- `POST /api/reminders/{id}/cancel` — cancel a reminder.
- `GET /api/notifications` — notifications.
- `POST /api/notifications/{id}/retry` — resend a notification.

All `/api/*` endpoints require `Authorization: Bearer <ADMIN_API_TOKEN>`.
