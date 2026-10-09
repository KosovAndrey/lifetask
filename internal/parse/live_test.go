package parse

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// TestLiveCompare — разбор настоящих заметок разными моделями через API (платно, центы).
// Запускается только с ключом:
//
//	ANTHROPIC_API_KEY=... [AI_PROXY_URL=...] go test -run TestLiveCompare -v ./internal/parse
//
// PARSE_EVAL — какие варианты сравнить, через запятую «модель:effort» (effort пустой — без него).
func TestLiveCompare(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY не задан")
	}
	variants := os.Getenv("PARSE_EVAL")
	if variants == "" {
		variants = "claude-haiku-4-5:,claude-haiku-5-5:low,claude-haiku-5-5:medium"
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	if p := os.Getenv("AI_PROXY_URL"); p != "" {
		u, err := url.Parse(p)
		if err != nil {
			t.Fatal(err)
		}
		hc.Transport = &http.Transport{Proxy: http.ProxyURL(u)}
	}
	// Суббота, 3 октября 2026, 18:00 — от неё считаются «завтра», «во вторник».
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, domain.MSK)
	spheres := []domain.Sphere{{ID: 1, Slug: "work"}, {ID: 2, Slug: "career"}, {ID: 3, Slug: "study"}, {ID: 4, Slug: "product"},
		{ID: 5, Slug: "home"}, {ID: 6, Slug: "leisure"}, {ID: 7, Slug: "health"}}
	cands := []Candidate{{ID: "11111111-1111-1111-1111-111111111111", Title: "Уборка в квартире"}, {ID: "22222222-2222-2222-2222-222222222222", Title: "Отчёт по стажировке"}}

	for _, v := range strings.Split(variants, ",") {
		model, effort, _ := strings.Cut(strings.TrimSpace(v), ":")
		c := NewClaude(key, hc)
		c.model, c.effort = model, anthropic.OutputConfigEffort(effort)
		ok, total := 0, 0
		start := time.Now()
		for _, tc := range liveCases {
			total++
			res, err := c.Parse(context.Background(), Input{Text: tc.text, Now: now, Spheres: spheres, Candidates: cands})
			if err != nil {
				t.Logf("[%s] ✗ %q: ошибка %v", v, tc.text, err)
				continue
			}
			if bad := tc.check(res); bad != "" {
				t.Logf("[%s] ✗ %q: %s — %s", v, tc.text, bad, res)
				continue
			}
			ok++
		}
		t.Logf("[%s] верно %d из %d, в среднем %s на заметку", v, ok, total, (time.Since(start) / time.Duration(total)).Round(10*time.Millisecond))
	}
}

type liveCase struct {
	text  string
	check func(Result) string // "" — разбор верный, иначе что не так
}

func is[T comparable](got *T, want T) bool { return got != nil && *got == want }

// sameTime — совпадение момента (модель может написать время в другой записи).
func sameTime(got *string, want string) bool {
	if got == nil {
		return false
	}
	g, err1 := time.Parse(time.RFC3339, *got)
	w, err2 := time.Parse(time.RFC3339, want)
	return err1 == nil && err2 == nil && g.Equal(w)
}

func checks(fs ...func(Result) string) func(Result) string {
	return func(r Result) string {
		for _, f := range fs {
			if s := f(r); s != "" {
				return s
			}
		}
		return ""
	}
}

func intent(i Intent) func(Result) string {
	return func(r Result) string {
		if r.Intent != i {
			return "intent " + string(r.Intent) + ", ждали " + string(i)
		}
		return ""
	}
}
func planned(d string) func(Result) string {
	return func(r Result) string {
		if !is(r.PlannedDate, d) {
			return "planned_date ≠ " + d
		}
		return ""
	}
}
func startAt(ts string) func(Result) string {
	return func(r Result) string {
		if !sameTime(r.StartAt, ts) {
			return "start_at ≠ " + ts
		}
		return ""
	}
}
func endAt(ts string) func(Result) string {
	return func(r Result) string {
		if !sameTime(r.EndAt, ts) {
			return "end_at ≠ " + ts
		}
		return ""
	}
}
func deadline(ts string) func(Result) string {
	return func(r Result) string {
		if !sameTime(r.Deadline, ts) {
			return "deadline ≠ " + ts
		}
		return ""
	}
}
func rrule(want ...string) func(Result) string {
	return func(r Result) string {
		if r.RRule == nil {
			return "нет rrule"
		}
		for _, w := range want {
			if !strings.Contains(*r.RRule, w) {
				return "rrule без " + w
			}
		}
		return ""
	}
}
func sphere(s string) func(Result) string {
	return func(r Result) string {
		if !is(r.Sphere, s) {
			return "сфера ≠ " + s
		}
		return ""
	}
}

var liveCases = []liveCase{
	{"во вторник созвон с тбанком 16-17", checks(intent(IntentEvent), startAt("2026-10-06T16:00:00+03:00"), endAt("2026-10-06T17:00:00+03:00"), sphere("career"))},
	{"завтра купить продукты", checks(intent(IntentTask), planned("2026-10-04"))},
	{"уборка каждую субботу", checks(intent(IntentTask), rrule("FREQ=WEEKLY", "SA"))},
	{"спорт по вт и чт", checks(rrule("FREQ=WEEKLY", "TU", "TH"), sphere("health"))},
	{"уборка 40м", func(r Result) string {
		if r.Intent != IntentTimeLog || !is(r.TimeMinutes, 40) || !is(r.TimeItemID, "11111111-1111-1111-1111-111111111111") {
			return "ждали time_log 40м к «Уборка в квартире»"
		}
		return ""
	}},
	{"идея: бот для учёта прочитанных книг", checks(intent(IntentNote))},
	{"до пятницы сдать отчёт по стажировке", checks(intent(IntentTask), deadline("2026-10-09T23:59:00+03:00"), sphere("work"))},
	{"через 2 часа позвонить маме", checks(startAt("2026-10-03T20:00:00+03:00"))},
	{"в понедельник в 10 собес в яндекс на час", checks(intent(IntentEvent), startAt("2026-10-05T10:00:00+03:00"), endAt("2026-10-05T11:00:00+03:00"), sphere("career"))},
	{"оплатить налоги до 1 декабря", checks(deadline("2026-12-01T23:59:00+03:00"), sphere("home"))},
	{"ну короче надо бы в среду записаться к зубному а то давно не был", checks(intent(IntentTask), planned("2026-10-07"), sphere("health"))},
	{"каждое первое число платить за квартиру", checks(rrule("FREQ=MONTHLY", "BYMONTHDAY=1"), sphere("home"))},
}
