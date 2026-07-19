package nlu

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/nyver2k/remindertgbot/internal/provider/exchangerate"
)

var (
	// rePollEveryN matches "каждые N часов/минут".
	rePollEveryN = regexp.MustCompile(`(?i)каждые?\s+(\d+)\s+(час(?:а|ов)?|минут(?:у|ы)?)`)
	// rePollOnceIn matches "раз в N часов/минут".
	rePollOnceIn = regexp.MustCompile(`(?i)раз\s+в\s+(\d+)\s+(час(?:а|ов)?|минут(?:у|ы)?)`)
	// rePollEveryHour matches "каждый час" / "раз в час".
	rePollEveryHour = regexp.MustCompile(`(?i)(?:каждый\s+час|раз\s+в\s+час\b)`)
)

// FastPath recognizes simple absolute and recurring reminders using regex.
// Covers: «напомни [дата] в [время] [текст]» and «каждый [день/будний] в [время] [текст]».
type FastPath struct{}

func NewFastPath() *FastPath { return &FastPath{} }

var (
	reTVAnchor = regexp.MustCompile(
		`(?i)(?:уведоми|напомни)(?:\s+мне)?\s+за\s+(\d+)\s*` +
			`(час(?:а|ов)?|минут(?:у|ы)?|недел(?:ю|и|ь)|день|дня|дней)\s+до\s+` +
			`(?:программы\s+|передачи\s+)?["«]?(.+?)["»]?\s+на\s+(.+?)\s*$`)

	reAbsolute = regexp.MustCompile(
		`(?i)напомни?\s+(?:мне\s+)?` +
			`(?:(\d{1,2})[./](\d{1,2})(?:[./](\d{2,4}))?\s+)?` +
			`(?:(завтра|послезавтра|сегодня)\s+)?` +
			`в\s+(\d{1,2})[:.](\d{2})(?::(\d{2}))?\s+(.+)`)

	reRecurring = regexp.MustCompile(
		`(?i)каждый?\s+` +
			`(день|будний\s+день|понедельник|вторник|среду?|четверг|пятницу?|субботу?|воскресенье?)` +
			`\s+в\s+(\d{1,2})[:.](\d{2})(?::(\d{2}))?\s+(.+)`)

	reEveryDay = regexp.MustCompile(`(?i)каждый?\s+день\s+в\s+(\d{1,2})[:.](\d{2})\s+(.+)`)

	reURLExtract = regexp.MustCompile(`https?://\S+`)

	// reRSSDigestKeyword matches a request for a periodic news digest, e.g.
	// "дайджест новостей". Combined with a URL, this identifies an rss digest
	// reminder — checked before reRecurring/reAbsolute so a leading "каждый
	// день в HH:MM" doesn't get misread as a plain recurring text reminder.
	reRSSDigestKeyword = regexp.MustCompile(`(?i)дайджест`)
	reHHMM             = regexp.MustCompile(`\b([01]?\d|2[0-3])[:.]([0-5]\d)\b`)
	reTopN             = regexp.MustCompile(`(?i)топ[- ]?(\d+)|(\d+)\s+новост`)
	reWeatherKeyword   = regexp.MustCompile(`(?i)погод|прогноз|дожд|температур|градус|ночью?`)
	reWeatherForecast  = regexp.MustCompile(`(?i)прогноз(?:\s+погод[ыа])?|погод[уа]\s+на\s+(?:сегодня|завтра)`)
	reWeatherRain      = regexp.MustCompile(`(?i)дожд`)
	reWeatherThreshold = regexp.MustCompile(`(?i)(ниже|меньше|выше|больше)\s+(?:чем\s+)?(-?\d+)`)
	reWeatherCity      = regexp.MustCompile(`(?i)(?:в|для)\s+город(?:е|а)?\s+([\p{L}-]+(?:\s+[\p{L}-]+)?)`)
	reFiatRateAlert    = regexp.MustCompile(`(?i)(?:курс\s+)?(евро|eur|доллар(?:а|ов|ы)?|usd|юан[ья]|cny|фунт(?:а|ов)?|gbp|иен[аы]?|йен[аы]?|jpy|франк(?:а|ов)?|chf|тенге|kzt)\s+(?:станет\s+|будет\s+)?(выше|больше|ниже|меньше)\s+(\d+(?:[.,]\d+)?)\s*(руб(?:ля|лей|ль|\.)?|rub|₽)`)
	reCryptoRateAlert  = regexp.MustCompile(`(?i)(биткоин|bitcoin|btc|эфириум|ethereum|ether|eth|солана|solana|sol)\s+(?:станет\s+|будет\s+)?(выше|больше|ниже|меньше)\s+(\d+(?:[.,]\d+)?)\s*(руб(?:ля|лей|ль|\.)?|rub|₽)`)
	reCryptoDayChange  = regexp.MustCompile(`(?i)(биткоин|bitcoin|btc|эфириум|ethereum|ether|eth|солана|solana|sol)\s+(упад[её]т|снизится|вырастет|поднимется)\s+(?:как\s+минимум\s+)?на\s+(\d+(?:[.,]\d+)?)\s*%\s+(?:за\s+)?(?:день|сутки|24\s*час(?:а|ов)?)`)
)

func (p *FastPath) Parse(ctx context.Context, text string, loc *time.Location) (*ParseResult, error) {
	if loc == nil {
		loc = time.UTC
	}
	text = strings.TrimSpace(text)

	if r := parseExchangeRate(text); r != nil {
		return r, nil
	}
	if r := parseWeather(text, loc); r != nil {
		return r, nil
	}
	if m := reTVAnchor.FindStringSubmatch(text); m != nil {
		return p.parseTVAnchor(m), nil
	}
	if r := parsePriceDrop(text); r != nil {
		return r, nil
	}
	if r := parseRSSDigest(text); r != nil {
		return r, nil
	}
	if m := reRecurring.FindStringSubmatch(text); m != nil {
		return p.parseRecurring(m), nil
	}
	if m := reAbsolute.FindStringSubmatch(text); m != nil {
		return p.parseAbsolute(m, loc), nil
	}
	return &ParseResult{Spec: &domain.Spec{}, Confidence: 0}, nil
}

func parseExchangeRate(text string) *ParseResult {
	if match := reFiatRateAlert.FindStringSubmatch(text); match != nil {
		base, title := fiatCode(match[1])
		targetValue, ok := scaledDecimal(match[3], exchangerate.RateScale)
		if base == "" || !ok || targetValue <= 0 {
			return nil
		}
		operator := domain.ConditionOperatorGT
		if direction := strings.ToLower(match[2]); direction == "ниже" || direction == "меньше" {
			operator = domain.ConditionOperatorLT
		}
		return &ParseResult{
			Kind: domain.KindConditional, EvalCron: parsePollCron(text), Confidence: 0.98,
			Spec: &domain.Spec{
				Trigger: domain.TriggerThreshold, Message: strings.TrimSpace(text), Currency: "RUB",
				Condition: &domain.Condition{Operator: operator, Target: &targetValue, EdgeTriggered: true},
				Event: domain.EventSpec{Type: "exchange_rate", Title: title + "/RUB", Params: map[string]string{
					"asset_type": "fiat", "base": base, "quote": "RUB", "metric": "rate",
				}},
			},
		}
	}
	if match := reCryptoRateAlert.FindStringSubmatch(text); match != nil {
		coinID, title := cryptoID(match[1])
		targetValue, ok := scaledDecimal(match[3], exchangerate.RateScale)
		if coinID == "" || !ok || targetValue <= 0 {
			return nil
		}
		operator := domain.ConditionOperatorGT
		if direction := strings.ToLower(match[2]); direction == "ниже" || direction == "меньше" {
			operator = domain.ConditionOperatorLT
		}
		return &ParseResult{
			Kind: domain.KindConditional, EvalCron: parsePollCron(text), Confidence: 0.98,
			Spec: &domain.Spec{
				Trigger: domain.TriggerThreshold, Message: strings.TrimSpace(text), Currency: "RUB",
				Condition: &domain.Condition{Operator: operator, Target: &targetValue, EdgeTriggered: true},
				Event: domain.EventSpec{Type: "exchange_rate", Title: title + "/RUB", Params: map[string]string{
					"asset_type": "crypto", "base": coinID, "quote": "RUB", "metric": "rate",
				}},
			},
		}
	}

	if match := reCryptoDayChange.FindStringSubmatch(text); match != nil {
		coinID, title := cryptoID(match[1])
		percent, ok := scaledDecimal(match[3], exchangerate.PercentScale)
		if coinID == "" || !ok || percent <= 0 {
			return nil
		}
		operator := domain.ConditionOperatorGTE
		target := percent
		direction := strings.ToLower(match[2])
		if strings.HasPrefix(direction, "упад") || strings.HasPrefix(direction, "сниз") {
			operator = domain.ConditionOperatorLTE
			target = -percent
		}
		return &ParseResult{
			Kind: domain.KindConditional, EvalCron: parsePollCron(text), Confidence: 0.98,
			Spec: &domain.Spec{
				Trigger: domain.TriggerThreshold, Message: strings.TrimSpace(text), Currency: "%",
				Condition: &domain.Condition{Operator: operator, Target: &target, EdgeTriggered: true},
				Event: domain.EventSpec{Type: "exchange_rate", Title: title + " — изменение за 24 часа", Params: map[string]string{
					"asset_type": "crypto", "base": coinID, "quote": "RUB", "metric": "change_24h",
				}},
			},
		}
	}
	return nil
}

func fiatCode(value string) (code, title string) {
	switch strings.ToLower(value) {
	case "евро", "eur":
		return "EUR", "EUR"
	case "доллар", "доллара", "доллары", "долларов", "usd":
		return "USD", "USD"
	case "юань", "юаня", "cny":
		return "CNY", "CNY"
	case "фунт", "фунта", "фунтов", "gbp":
		return "GBP", "GBP"
	case "иена", "иены", "йена", "йены", "jpy":
		return "JPY", "JPY"
	case "франк", "франка", "франков", "chf":
		return "CHF", "CHF"
	case "тенге", "kzt":
		return "KZT", "KZT"
	default:
		return "", ""
	}
}

func cryptoID(value string) (id, title string) {
	switch strings.ToLower(value) {
	case "биткоин", "bitcoin", "btc":
		return "bitcoin", "Bitcoin"
	case "эфириум", "ethereum", "ether", "eth":
		return "ethereum", "Ethereum"
	case "солана", "solana", "sol":
		return "solana", "Solana"
	default:
		return "", ""
	}
}

func scaledDecimal(value string, scale int64) (int64, bool) {
	n, err := strconv.ParseFloat(strings.ReplaceAll(value, ",", "."), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n > float64(math.MaxInt64)/float64(scale) {
		return 0, false
	}
	return int64(math.Round(n * float64(scale))), true
}

func parseWeather(text string, loc *time.Location) *ParseResult {
	if !reWeatherKeyword.MatchString(text) {
		return nil
	}
	lower := strings.ToLower(text)
	params := map[string]string{"timezone": loc.String()}
	if city := extractWeatherCity(text); city != "" {
		params["location"] = city
	}

	if threshold := reWeatherThreshold.FindStringSubmatch(text); threshold != nil &&
		(strings.Contains(lower, "ноч") || strings.Contains(lower, "температур") || strings.Contains(lower, "градус")) {
		degrees, err := strconv.ParseInt(threshold[2], 10, 64)
		if err != nil || degrees < -100 || degrees > 100 {
			return nil
		}
		operator := domain.ConditionOperatorLT
		direction := strings.ToLower(threshold[1])
		if direction == "выше" || direction == "больше" {
			operator = domain.ConditionOperatorGT
		}
		target := degrees * 10 // Weather metrics store tenths of a degree Celsius.
		params["day"] = "today"
		if strings.Contains(lower, "ноч") {
			params["day"] = "next_night"
			params["period"] = "night"
		}
		return &ParseResult{
			Kind: domain.KindConditional,
			Spec: &domain.Spec{
				Trigger: domain.TriggerThreshold,
				Message: strings.TrimSpace(text),
				Condition: &domain.Condition{
					Operator: operator, Target: &target, EdgeTriggered: false,
					Cooldown: domain.Duration{Duration: 24 * time.Hour},
				},
				Event: domain.EventSpec{Type: "weather", Params: params},
			},
			Confidence: 0.96,
		}
	}

	if reWeatherRain.MatchString(text) && strings.Contains(lower, "если") {
		params["condition"] = "rain"
		now := time.Now().In(loc)
		day := now
		if strings.Contains(lower, "завтра") {
			day = day.AddDate(0, 0, 1)
		}
		params["day"] = day.Format("2006-01-02")
		hour, minute := 8, 0
		if match := reHHMM.FindStringSubmatch(text); match != nil {
			hour, _ = strconv.Atoi(match[1])
			minute, _ = strconv.Atoi(match[2])
		}
		fireAt := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, loc).Format(time.RFC3339)
		return &ParseResult{
			Kind: domain.KindConditional,
			Spec: &domain.Spec{
				Trigger: domain.TriggerAnchor,
				Message: strings.TrimSpace(text),
				Event:   domain.EventSpec{Type: "weather", Params: params},
			},
			FireAt:     &fireAt,
			Confidence: 0.97,
		}
	}

	if reWeatherForecast.MatchString(text) {
		params["day"] = "today"
		if strings.Contains(lower, "завтра") {
			params["day"] = "tomorrow"
		}
		cron := ""
		if strings.Contains(lower, "кажд") && strings.Contains(lower, "утр") {
			cron = "0 8 * * *"
			if match := reHHMM.FindStringSubmatch(text); match != nil {
				hour, _ := strconv.Atoi(match[1])
				minute, _ := strconv.Atoi(match[2])
				cron = fmt.Sprintf("%d %d * * *", minute, hour)
			}
		}
		return &ParseResult{
			Kind:     domain.KindConditional,
			EvalCron: cron,
			Spec: &domain.Spec{
				Trigger: domain.TriggerAnchor,
				Message: strings.TrimSpace(text),
				Event:   domain.EventSpec{Type: "weather", Params: params},
			},
			Confidence: 0.96,
		}
	}
	return nil
}

func extractWeatherCity(text string) string {
	if match := reWeatherCity.FindStringSubmatch(text); match != nil {
		return strings.TrimSpace(match[1])
	}
	return ""
}

// rssDigestMaxURLs bounds how many feeds a free-text digest request can
// combine, matching provider/rss's own maxFeedURLs and handlers.go's
// rssMaxURLs — extra URLs beyond this are silently dropped rather than
// rejected, since fastpath has no way to surface a validation error back to
// the user (unlike the /rss command).
const rssDigestMaxURLs = 10

// parseRSSDigest detects "<...дайджест...> <...HH:MM...> <URL[, URL...]>"
// phrasings, e.g. "каждый день в 18:00 создай дайджест новостей на основе
// <ссылка>" or "дайджест по лентам <ссылка1> и <ссылка2>". The time and
// top-N are optional; the digest keyword and at least one URL are required.
func parseRSSDigest(text string) *ParseResult {
	if !reRSSDigestKeyword.MatchString(text) {
		return nil
	}
	urls := ExtractURLs(text)
	if len(urls) == 0 {
		return nil
	}
	if len(urls) > rssDigestMaxURLs {
		urls = urls[:rssDigestMaxURLs]
	}
	// Search for time/top-N outside the URLs so path segments in a feed link
	// (rare, but possible) can't be misread as HH:MM or an item count.
	rest := text
	for _, u := range urls {
		rest = strings.Replace(rest, u, "", 1)
	}

	cron := "0 9 * * *"
	if m := reHHMM.FindStringSubmatch(rest); m != nil {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		cron = fmt.Sprintf("%d %d * * *", min, h)
	}

	topN := 0
	if m := reTopN.FindStringSubmatch(rest); m != nil {
		nStr := m[1]
		if nStr == "" {
			nStr = m[2]
		}
		if n, err := strconv.Atoi(nStr); err == nil && n > 0 {
			topN = n
		}
	}

	return &ParseResult{
		Kind:     domain.KindConditional,
		EvalCron: cron,
		Spec: &domain.Spec{
			Trigger: domain.TriggerDigest,
			Message: "RSS-дайджест новостей",
			TopN:    topN,
			Event: domain.EventSpec{
				Type:   "rss",
				Params: map[string]string{"url": strings.Join(urls, ",")},
			},
		},
		Confidence: 0.97,
	}
}

// parsePriceDrop detects "URL ... уведоми при снижении цены [каждые N часов]" patterns.
func parsePriceDrop(text string) *ParseResult {
	u := ExtractURL(text)
	if u == "" {
		return nil
	}
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "снижени") &&
		!strings.Contains(lower, "подешев") &&
		!strings.Contains(lower, "цена упад") &&
		!strings.Contains(lower, "цена снизится") {
		return nil
	}
	return &ParseResult{
		Kind:     domain.KindConditional,
		EvalCron: parsePollCron(text),
		Spec: &domain.Spec{
			Trigger: domain.TriggerThreshold,
			Message: "Уведомить при снижении цены",
			Condition: &domain.Condition{
				Operator:      domain.ConditionOperatorLT,
				EdgeTriggered: true,
			},
			Event: domain.EventSpec{
				Type:   "price",
				Params: map[string]string{"url": u},
			},
		},
		Confidence: 0.98,
	}
}

// parsePollCron extracts a cron expression from poll-interval phrases like
// "каждые 2 часа", "каждые 30 минут", "каждый час", "раз в час".
// Returns "" when no interval is found (caller uses the system default).
func parsePollCron(text string) string {
	if rePollEveryHour.MatchString(text) {
		// "каждый час" / "раз в час" — but rePollEveryN may also match "каждые 1 час",
		// so check the more specific single-hour pattern first.
		if m := rePollEveryN.FindStringSubmatch(text); m == nil {
			return "0 * * * *"
		}
	}

	if m := rePollEveryN.FindStringSubmatch(text); m != nil {
		return intervalToCron(m[1], m[2])
	}
	if m := rePollOnceIn.FindStringSubmatch(text); m != nil {
		return intervalToCron(m[1], m[2])
	}
	return ""
}

func intervalToCron(nStr, unit string) string {
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 {
		return ""
	}
	unit = strings.ToLower(unit)
	switch {
	case strings.HasPrefix(unit, "час"):
		if n == 1 {
			return "0 * * * *"
		}
		return fmt.Sprintf("0 */%d * * *", n)
	case strings.HasPrefix(unit, "минут"):
		if n == 1 {
			return "* * * * *"
		}
		return fmt.Sprintf("*/%d * * * *", n)
	}
	return ""
}

// ExtractURL returns the first HTTP(S) URL found in s, trimming trailing punctuation.
func ExtractURL(s string) string {
	u := reURLExtract.FindString(s)
	return strings.TrimRight(u, ".,;:!?)")
}

// ExtractURLs returns every HTTP(S) URL found in s, trimming trailing
// punctuation from each — used where a message may reference multiple feeds
// at once (e.g. an rss digest combining several lentas).
func ExtractURLs(s string) []string {
	matches := reURLExtract.FindAllString(s, -1)
	urls := make([]string, 0, len(matches))
	for _, m := range matches {
		urls = append(urls, strings.TrimRight(m, ".,;:!?)"))
	}
	return urls
}

func (p *FastPath) parseTVAnchor(m []string) *ParseResult {
	amount, _ := strconv.Atoi(m[1])
	unit := time.Hour
	switch strings.ToLower(m[2]) {
	case "минута", "минуту", "минуты", "минут":
		unit = time.Minute
	case "день", "дня", "дней":
		unit = 24 * time.Hour
	case "неделю", "недели", "недель":
		unit = 7 * 24 * time.Hour
	}
	title := strings.Trim(strings.TrimSpace(m[3]), `"«»`)
	channel := normalizeTVChannel(m[4])
	return &ParseResult{
		Kind: domain.KindConditional,
		Spec: &domain.Spec{
			Trigger:  domain.TriggerAnchor,
			LeadTime: domain.Duration{Duration: time.Duration(amount) * unit},
			Event: domain.EventSpec{
				Type:   "tv_program",
				Title:  title,
				Params: map[string]string{"channel": channel},
			},
			Message: title,
		},
		Confidence: 0.98,
	}
}

func normalizeTVChannel(value string) string {
	value = strings.Trim(strings.TrimSpace(value), `"«»`)
	lower := strings.ToLower(value)
	for _, suffix := range []string{" канале", " каналу", " канал"} {
		lower = strings.TrimSpace(strings.TrimSuffix(lower, suffix))
	}
	switch lower {
	case "первом", "первый", "первом канале":
		return "Первый канал"
	default:
		return strings.TrimSpace(value)
	}
}

func (p *FastPath) parseAbsolute(m []string, loc *time.Location) *ParseResult {
	// m[1]=day m[2]=month m[3]=year m[4]=relative m[5]=hour m[6]=min m[7]=sec m[8]=text
	h, _ := strconv.Atoi(m[5])
	min, _ := strconv.Atoi(m[6])
	now := time.Now().In(loc)
	target := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, loc)

	switch strings.ToLower(m[4]) {
	case "завтра":
		target = target.AddDate(0, 0, 1)
	case "послезавтра":
		target = target.AddDate(0, 0, 2)
	case "сегодня", "":
		if m[1] != "" {
			day, _ := strconv.Atoi(m[1])
			month, _ := strconv.Atoi(m[2])
			year := now.Year()
			if m[3] != "" {
				year, _ = strconv.Atoi(m[3])
				if year < 100 {
					year += 2000
				}
			}
			target = time.Date(year, time.Month(month), day, h, min, 0, 0, loc)
		} else if target.Before(now) {
			target = target.AddDate(0, 0, 1)
		}
	}

	fireAt := target.Format(time.RFC3339)
	return &ParseResult{
		Kind: domain.KindAbsolute,
		Spec: &domain.Spec{
			Message: strings.TrimSpace(m[8]),
			Event:   domain.EventSpec{Type: "absolute"},
		},
		Confidence: 0.95,
		FireAt:     &fireAt,
	}
}

func (p *FastPath) parseRecurring(m []string) *ParseResult {
	// m[1]=period m[2]=hour m[3]=min m[4]=sec m[5]=text
	h, _ := strconv.Atoi(m[2])
	min, _ := strconv.Atoi(m[3])

	cron := buildCron(strings.ToLower(strings.TrimSpace(m[1])), h, min)
	return &ParseResult{
		Kind: domain.KindRecurring,
		Spec: &domain.Spec{
			Message: strings.TrimSpace(m[5]),
			Event:   domain.EventSpec{Type: "recurring"},
		},
		Confidence: 0.95,
		EvalCron:   cron,
	}
}

func buildCron(period string, h, min int) string {
	// minute hour day month weekday
	switch period {
	case "день":
		return fmt.Sprintf("%d %d * * *", min, h)
	case "будний день":
		return fmt.Sprintf("%d %d * * 1-5", min, h)
	case "понедельник":
		return fmt.Sprintf("%d %d * * 1", min, h)
	case "вторник":
		return fmt.Sprintf("%d %d * * 2", min, h)
	case "среда", "среду":
		return fmt.Sprintf("%d %d * * 3", min, h)
	case "четверг":
		return fmt.Sprintf("%d %d * * 4", min, h)
	case "пятница", "пятницу":
		return fmt.Sprintf("%d %d * * 5", min, h)
	case "суббота", "субботу":
		return fmt.Sprintf("%d %d * * 6", min, h)
	case "воскресенье":
		return fmt.Sprintf("%d %d * * 0", min, h)
	default:
		return fmt.Sprintf("%d %d * * *", min, h)
	}
}
