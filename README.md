# Reminder Telegram Bot

A Telegram bot for one-off, recurring, and conditional reminders. The project
includes Telegram polling, background reminder processing, and a CLI for
migrations and administrative operations.

SQLite is used by default, so no separate database server is required for a
standard deployment. For a local run the database is stored in
`data/remind.db`; in Docker it lives in `/data/remind.db` inside a persistent
volume.

## Components

- `bot` accepts Telegram commands and recognizes reminder text.
- `worker` evaluates conditions and sends scheduled notifications.
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
| `/rss <url>` | subscribe to a periodic RSS/Atom news digest, delivered daily at 09:00 (top 10) |
| `/rss <url> \| HH:MM` | same, with a custom delivery time |
| `/rss <url> \| HH:MM \| N` | same, with a custom delivery time and item count (1–20) |

IDs for `/cancel`, `/remove`, `/pause`, and `/resume` can be obtained with
`/list`. `/cancel` marks the reminder `done` in the database, while `/remove`
physically deletes it along with its notifications and observation history.

Creating a reminder does not require a dedicated command: just send the bot a
plain-text description. The bot recognizes the parameters, asks a
clarifying question if needed, and offers a confirmation button before
creating it.

For smoother day-to-day use, `/start`, `/help`, and an empty `/list` show a
small persistent Telegram keyboard with the most common commands. During
confirmation the inline buttons are still available, but users can also reply
with plain text such as `yes`, `no`, `да`, or `нет`.

Example messages (the bot's NLU understands Russian):

```text
напомни завтра в 9:00 позвонить маме
каждый понедельник в 8:30 напоминай про совещание
уведоми за 3 часа до КВН на Первом
уведоми при снижении цены: https://example.com/product
каждый день в 18:00 создай дайджест новостей на основе https://lenta.ru/rss
предупреди завтра утром, если будет дождь
уведоми, если ночью ожидается ниже -10
каждое утро присылай прогноз погоды на день
пришли прогноз погоды на сегодня
пришли прогноз погоды на завтра
```

The RSS example creates a periodic news digest — see
[RSS news digest](#rss-news-digest) below for details; the same reminder can
also be created directly with `/rss https://lenta.ru/rss | 18:00`.

Relative dates and times in free-form messages are interpreted in the IANA
timezone configured for that user with `/tz`. The current user timezone is
passed to both the fast parser and the LLM prompt for every request.

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
docker compose logs -f bot worker
```

Compose automatically:

- creates the `reminddata` volume;
- stores SQLite in `/data/remind.db`;
- applies migrations before starting the services;
- starts the Telegram bot and background worker after migrations succeed.

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
- `server` — worker ID and log level.

The `providers.travel` fields are reserved for a future live ticket
integration. Travel reminders are currently rejected; the bot never returns
sample or fabricated ticket offers.

At `server.log_level: info`, LLM calls are logged with the component
(`nlu_parser` or `news_ranker`), provider, selected model, and whether the
request is using a fallback model. Prompts, responses, and API keys are not
written to logs.

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
| `COINGECKO_API_KEY` | optional CoinGecko Demo API key |
| `EPG_SERVICE_API_KEY` | EPG Service Bearer token |
| `EPG_SERVICE_BASE_URL` | override for the EPG Service API base URL |
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

## Generic metric conditions

Conditional metric reminders use a provider-independent condition model. The
reminder spec stores it next to `trigger: threshold`:

```json
{
  "trigger": "threshold",
  "condition": {
    "operator": "lt",
    "target": 5000000,
    "edge_triggered": true
  },
  "currency": "RUB",
  "event": {
    "type": "price",
    "title": "Example product",
    "params": {"url": "https://example.com/product"}
  },
  "message": "The price is below 50,000 RUB"
}
```

`target` uses the metric provider's integer unit. The built-in price provider
uses minor currency units, so 50,000 RUB is `5000000` kopecks. Providers for
temperatures, exchange rates, availability, or content hashes can use their
own documented integer representation.

| Operator | Matches when |
| --- | --- |
| `lt` | value is lower than the target |
| `lte` | value is lower than or equal to the target |
| `gt` | value is greater than the target |
| `gte` | value is greater than or equal to the target |
| `changed` | value differs from the previous sample |
| `changed_pct` | absolute change from the previous sample is at least `change_percent` |

For `lt`, `lte`, `gt`, and `gte`, omitting `target` compares the current value
with the previous sample. This is how "notify on a price decrease" is
represented. `changed` and `changed_pct` also require a previous sample; the
first poll only establishes their baseline.

Two delivery modes are supported:

- `edge_triggered: true` sends only when a target comparison changes from
  false to true. The first sample establishes the baseline. Change operators
  and targetless previous-sample comparisons emit one event for every matching
  change. An optional cooldown can throttle rapid edge events.
- `edge_triggered: false` sends immediately while the condition is true and
  repeats while it remains true after `cooldown`. A positive cooldown is
  required for this mode to prevent an alert on every poll.

Every successful sample stores the condition state and last trigger time in
the existing observation history, so restarts preserve edge and cooldown
behavior. Legacy specs with top-level `target` and `direction` remain readable;
`below` maps to `lt`, `above` maps to `gt`, and an omitted direction retains
the historical lower-than-previous behavior.

The repository includes price, weather, and exchange-rate metric providers.
Other examples such as stock availability and page hashes need a corresponding
`MetricProvider`, but use the same evaluator without scheduler changes.

## Fiat and cryptocurrency rate monitoring

The `exchange_rate` metric provider supports fiat cross-rates and
cryptocurrency prices. It also exposes CoinGecko's rolling 24-hour percentage
change as a scalar metric, so directional daily-change alerts use the same
generic threshold evaluator as prices and temperatures.

Supported free-text scenarios include:

```text
уведоми, когда евро станет выше 100 рублей
сообщи, когда биткоин станет ниже 5000000 рублей
сообщи, если биткоин упадёт на 5% за день
```

The first request creates an edge-triggered `gt` condition for `EUR/RUB`.
Exchange rates are stored with six decimal places (`100 RUB = 100000000`).
The second request monitors Bitcoin's current RUB price. The third evaluates
Bitcoin's rolling RUB-denominated 24-hour change and creates an edge-triggered
`lte` condition at `-5%`; percentage metrics are
stored with two decimal places (`-5% = -500`). The first successful sample
establishes the edge baseline, and the notification is sent when the metric
subsequently crosses the threshold.

Fiat rates come from the official [Bank of Russia daily XML feed](https://www.cbr.ru/development/SXML/). The provider
can derive cross-rates between currencies published in that feed, with RUB as
the common reference. Cryptocurrency prices and 24-hour changes come from the
CoinGecko [Simple Price API](https://docs.coingecko.com/reference/simple-price). CoinGecko's keyless public endpoint works without
a key; `COINGECKO_API_KEY` or `coingecko_api_key` can supply an optional Demo
API key when higher public limits are needed.

```yaml
providers:
  exchange_rate:
    cbr_url: https://www.cbr.ru/scripts/XML_daily.asp
    coingecko_url: https://api.coingecko.com/api/v3/simple/price
    coingecko_api_key: "${COINGECKO_API_KEY}"
    timeout: 10s
    poll_cron: "0 * * * *"
```

| Field | Description | Default |
| --- | --- | --- |
| `cbr_url` | Bank of Russia-compatible daily fiat-rate XML endpoint | official Bank of Russia endpoint |
| `coingecko_url` | CoinGecko-compatible Simple Price endpoint | keyless public CoinGecko endpoint |
| `coingecko_api_key` | optional CoinGecko Demo API key | `""` |
| `timeout` | timeout for one upstream request | `10s` |
| `poll_cron` | default alert schedule in the user's timezone | `"0 * * * *"` |

## Weather forecasts and alerts

The weather provider uses [Open-Meteo's forecast API](https://open-meteo.com/en/docs)
and [geocoding API](https://open-meteo.com/en/docs/geocoding-api). The public
non-commercial endpoints do not require an API key. A location named in a
reminder is geocoded and cached; when no city is present, the provider uses
`providers.weather.default_location`.

Supported free-text scenarios include:

```text
предупреди завтра утром, если будет дождь
уведоми, если ночью ожидается ниже -10
каждое утро присылай прогноз погоды на день
пришли прогноз погоды на сегодня
пришли прогноз погоды на завтра
```

Forecast requests are implemented as weather events. Requests for today or
tomorrow run once immediately; `каждое утро` creates a daily event schedule
(08:00 by default), and a rain warning evaluates once at the requested morning
time and sends nothing when rain is not forecast. Daily messages include the
WMO weather condition, actual and apparent temperature range, precipitation
probability and amount, and maximum wind speed.

Temperature alerts use the generic threshold evaluator. The provider samples
the minimum forecast temperature for the upcoming night (00:00–05:59 local
time), stores it internally in tenths of a degree, and compares it with the
user's Celsius threshold. Level-triggered alerts use a 24-hour cooldown, so an
already-matching forecast can notify immediately without sending on every
poll.

The timezone set with `/tz` controls forecast dates, local-day aggregates, and
cron schedules. The timezone returned by geocoding is used only when a caller
does not supply a timezone explicitly.

### Weather provider settings

```yaml
providers:
  weather:
    forecast_url: https://api.open-meteo.com/v1/forecast
    geocoding_url: https://geocoding-api.open-meteo.com/v1/search
    default_location: Moscow
    timeout: 10s
    poll_cron: "0 * * * *"
```

| Field | Description | Default |
| --- | --- | --- |
| `forecast_url` | Open-Meteo-compatible forecast endpoint | public Open-Meteo endpoint |
| `geocoding_url` | Open-Meteo-compatible location search endpoint | public Open-Meteo endpoint |
| `default_location` | city used when reminder text omits a location | `Moscow` |
| `timeout` | timeout for each geocoding or forecast request | `10s` |
| `poll_cron` | default temperature-alert polling schedule in the user's timezone | `"0 * * * *"` |

## Price monitoring

The bot tracks a price drop for a product on an online store's page. To
create a reminder, just send a link and a keyword:

```text
уведоми при снижении цены https://example.com/product
подешевеет ли https://www.ozon.ru/product/... — напомни
уведоми при снижении цены https://example.com/product каждые 4 часа
уведоми при снижении цены https://example.com/product каждые 30 минут
```

The NLU automatically extracts the URL and builds an edge-triggered `lt`
condition without a target. The worker periodically checks the page and sends
a notification for each observed price decrease. Requests with an explicit
price target or percentage change are mapped to `lt` or `changed_pct`.

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
    poll_cron: "0 * * * *"
```

| Field | Description | Default |
| --- | --- | --- |
| `user_agent` | User-Agent for HTTP requests to store pages | Chrome/Windows string |
| `timeout` | timeout for fetching the page (HTTP or headless) | `15s` |
| `headless` | use Chromium to bypass WAF/TLS fingerprinting | `false` |
| `proxy_url` | proxy for the headless browser (HTTP, HTTPS, or SOCKS5) | `""` |
| `poll_cron` | default poll schedule (5-field cron, interpreted in the user's timezone) | `"0 * * * *"` |

Some stores (e.g. DNS-shop) block plain HTTP requests by TLS fingerprint.
Enable `headless: true` to use a real browser stack. The Docker image
includes Chromium; the first request in this mode takes 1–2 seconds to
launch the browser, subsequent ones are much faster thanks to the shared
Chrome process.

Each headless check runs in an isolated browser context, so cookies and site
storage cannot leak between users. Price URLs are logged without credentials,
query parameters, or fragments. Both price and RSS proxy URLs are validated at
startup and accept only HTTP, HTTPS, SOCKS5, and SOCKS5H schemes.

## RSS news digest

The bot can turn any RSS 2.0 or Atom feed into a periodic "important news"
digest, delivered once a day at a chosen time. The simplest way is the
dedicated command:

```text
/rss https://lenta.ru/rss                       → daily digest at 09:00, top 10
/rss https://lenta.ru/rss | 08:30                → custom delivery time
/rss https://lenta.ru/rss | 08:30 | 10           → custom time and top-10 items
```

A single reminder can also combine several feeds into one digest — separate
the URLs with a comma:

```text
/rss https://lenta.ru/rss,https://habr.com/rss | 08:30 | 10
```

Items from every feed are merged, deduplicated, and ranked together into one
shared top-N — not a separate top-N per feed. A digest can combine up to 10
feeds; feeds are fetched concurrently, and if one of them fails or times out
the others still make it into the digest (only when *every* feed fails does
the tick produce no digest, retried on the next one).

The same reminder can also be created from a plain-text message, without
using the command — the NLU recognizes phrases that combine one or more
feed links with a digest request and a time:

```text
каждый день в 18:00 создай дайджест новостей на основе https://lenta.ru/rss
дайджест новостей топ 10 по ленте https://lenta.ru/rss в 8 утра
дайджест по лентам https://lenta.ru/rss и https://habr.com/rss в 8 утра
```

If no time is given, the digest defaults to 09:00; if no item count is
given, it defaults to top 10. `/list`, `/pause`, `/resume`, `/cancel`, and
`/remove` manage an RSS digest reminder the same way as any other
conditional reminder.

Digest times are interpreted in the user's timezone configured with `/tz`
(`Europe/Moscow` by default). The VPS, container, and database timezone do
not affect delivery time.

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
count) are handed to the LLM, which picks the genuinely most important ones,
translates each selected title into Russian (feeds are often in other
languages, e.g. English-language tech news), and writes a fresh 2–3 sentence
Russian summary. This is off by default because it adds one LLM call per
digest tick; if that call fails or is unavailable, the digest silently falls
back to the heuristic ranking (with titles/summaries in the feed's original
language) instead of failing outright.

```yaml
providers:
  rss:
    llm_digest: true
```

### Formatting

Each digest item's title is a clickable link to the article (MarkdownV2),
with the publish date in italics next to it, instead of showing the raw URL
on its own line:

```
1. [Title of the article](https://example.com/article) · 16.07 10:00
   Summary in two or three sentences.
```

All feed-controlled text (title, summary) is escaped before being embedded
in the MarkdownV2 message, so untrusted content from the feed can never
break the message's formatting or inject unintended links/entities. Every
other notification kind (price drops, TV reminders, plain reminders) is
still sent as plain text, unaffected by this — only digest messages opt
into MarkdownV2 rendering.

Telegram rejects any single message over ~4096 characters, which a
multi-feed or top-10+ digest can exceed. When that happens the digest is
split into several messages at item boundaries (never mid-item), each
headed `(часть X из Y)`, instead of failing to send outright.

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
    timeout: 30s
    max_tokens: 1024
```

When a model is rate-limited, unavailable, returns an empty response, or
exceeds `timeout`, OpenRouter tries the models in `fallback_models` in
order. `timeout` is the per-model attempt timeout before fallback, and
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
remindctl version
```
