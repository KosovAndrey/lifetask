package parse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// Model — дешёвая быстрая модель: разбор короткой заметки не требует большего.
const Model = "claude-haiku-4-5"

type Claude struct {
	client anthropic.Client
}

// NewClaude: httpClient — с прокси, если API недоступен напрямую (VPS в РФ).
func NewClaude(apiKey string, httpClient *http.Client) *Claude {
	opts := []option.RequestOption{option.WithAPIKey(apiKey), option.WithMaxRetries(2),
		option.WithRequestTimeout(30 * time.Second)}
	if httpClient != nil {
		opts = append(opts, option.WithHTTPClient(httpClient))
	}
	return &Claude{client: anthropic.NewClient(opts...)}
}

func nullable(t string, desc string) map[string]any {
	return map[string]any{"type": []string{t, "null"}, "description": desc}
}

func schema(spheres []domain.Sphere) map[string]any {
	slugs := make([]any, 0, len(spheres)+1)
	for _, s := range spheres {
		slugs = append(slugs, s.Slug)
	}
	slugs = append(slugs, nil)
	props := map[string]any{
		"intent": map[string]any{"type": "string", "enum": []string{"task", "event", "note", "time_log", "unclear"},
			"description": "task — дело; event — встреча/созвон в конкретное время; note — мысль/заметка без действия; " +
				"time_log — отчёт о потраченном времени («уборка 40м»); unclear — непонятно"},
		"title":        map[string]any{"type": "string", "description": "Короткий заголовок в повелительном наклонении или как событие, с заглавной буквы"},
		"sphere":       map[string]any{"enum": slugs, "description": "Сфера жизни или null, если не очевидно"},
		"important":    map[string]any{"type": "boolean", "description": "Важно: влияет на цели, работу, карьеру, здоровье"},
		"urgent":       map[string]any{"type": "boolean", "description": "Срочно: есть близкий срок (сегодня-завтра) или кто-то ждёт"},
		"planned_date": nullable("string", "YYYY-MM-DD — когда делать, если назван день; для event — null"),
		"start_at":     nullable("string", "RFC3339 с +03:00 — начало, только если названо время"),
		"end_at":       nullable("string", "RFC3339 с +03:00 — конец, если назван диапазон или длительность встречи"),
		"deadline":     nullable("string", "RFC3339 с +03:00 — крайний срок, если сказано «до …»; без времени — 23:59"),
		"estimate_min": nullable("integer", "Оценка длительности в минутах, если названа или очевидна; иначе null"),
		"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
			"description": "Контекстные теги: @звонок, @комп, @дом, @вне_дома, ≤15мин. Только явно следующие из текста"},
		"details":   nullable("string", "Подробности из текста, не вошедшие в заголовок"),
		"checklist": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Пункты, если перечислены"},
		"rrule": nullable("string", "Если дело повторяется — правило RRULE: FREQ=DAILY|WEEKLY|MONTHLY, INTERVAL, BYDAY=MO,TU,WE,TH,FR,SA,SU, "+
			"BYMONTHDAY (−1 = последний день). Примеры: «каждую субботу» → FREQ=WEEKLY;BYDAY=SA; «по вт и чт» → FREQ=WEEKLY;BYDAY=TU,TH; "+
			"«через неделю по пн» → FREQ=WEEKLY;INTERVAL=2;BYDAY=MO; «каждое 1 число» → FREQ=MONTHLY;BYMONTHDAY=1. "+
			"planned_date/start_at — первый повтор. Не повторяется — null"),
		"time_minutes": nullable("integer", "Для time_log: сколько минут потрачено"),
		"time_item_id": nullable("string", "Для time_log: id задачи из списка кандидатов, к которой относится время; null, если подходящей нет"),
		"confident":    map[string]any{"type": "boolean", "description": "true, если смысл и даты однозначны"},
		"question":     nullable("string", "Если confident=false — один короткий уточняющий вопрос"),
	}
	required := make([]string, 0, len(props))
	for k := range props {
		required = append(required, k)
	}
	// Стабильный порядок: схема компилируется на стороне API и кешируется по содержимому.
	sort.Strings(required)
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

const systemPrompt = `Ты разбираешь короткие заметки, которые Андрей надиктовал или набрал с телефона для своего планировщика.
Заметка может быть с опечатками, без знаков препинания, из распознанной речи.
Всегда часовой пояс Москвы (+03:00). Относительные даты («завтра», «во вторник», «через пару часов») считай от текущего момента;
«во вторник» — ближайший будущий вторник (если сегодня вторник и время ещё не прошло — сегодня).
Не выдумывай: нет времени — start_at null; нет оценки — estimate_min null; сферу ставь, только если она понятна.
Сферы: work — работа (стажировка в Яндексе); career — собеседования, поиск работы, Т-Банк; study — учёба, курсы; product — свои продукты и пет-проекты;
home — быт, а также деньги и документы (оплаты, налоги, справки); leisure — досуг; health — здоровье, спорт.`

func (c *Claude) Parse(ctx context.Context, in Input) (Result, error) {
	now := in.Now.In(domain.MSK)
	var user strings.Builder
	fmt.Fprintf(&user, "Сейчас: %s, %s.\n", now.Format("2006-01-02 15:04"), weekdayRU[now.Weekday()])
	if len(in.Candidates) > 0 {
		user.WriteString("Кандидаты для time_log (id — название):\n")
		for _, c := range in.Candidates {
			fmt.Fprintf(&user, "- %s — %s\n", c.ID, c.Title)
		}
	}
	if in.Previous != nil {
		prev, _ := json.Marshal(in.Previous)
		fmt.Fprintf(&user, "Предыдущий разбор этой заметки: %s\nПользователь уточнил — исправь разбор с учётом уточнения.\n", prev)
	}
	fmt.Fprintf(&user, "Заметка:\n%s", in.Text)

	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     Model,
		MaxTokens: 1024,
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user.String()))},
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{Schema: schema(in.Spheres)},
		},
	})
	if err != nil {
		return Result{}, err
	}
	switch resp.StopReason {
	case anthropic.StopReasonRefusal:
		return Result{}, errors.New("модель отказалась разбирать")
	case anthropic.StopReasonMaxTokens:
		return Result{}, errors.New("ответ модели обрезан")
	}
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			var r Result
			if err := json.Unmarshal([]byte(t.Text), &r); err != nil {
				return Result{}, fmt.Errorf("ответ модели не по схеме: %w", err)
			}
			return r, nil
		}
	}
	return Result{}, errors.New("пустой ответ модели")
}

var weekdayRU = [...]string{"воскресенье", "понедельник", "вторник", "среда", "четверг", "пятница", "суббота"}
