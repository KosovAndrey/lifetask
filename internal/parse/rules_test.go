package parse

import (
	"context"
	"testing"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// Суббота, 3 октября 2026, 18:00 по Москве.
var now = time.Date(2026, 10, 3, 18, 0, 0, 0, domain.MSK)

func TestRules(t *testing.T) {
	cands := []Candidate{{ID: "u1", Title: "Уборка в квартире"}, {ID: "s1", Title: "Подготовка к собесу"}}
	cases := []struct {
		in                                     string
		intent                                 Intent
		title, sphere, date, start, end, rrule string
		deadline                               string
		estimate, minutes                      int
		timeItem                               string
		urgent, important                      bool
	}{
		{in: "во вторник созвон с тбанком 16-17", intent: IntentEvent, title: "Созвон с тбанком", sphere: "career",
			start: "2026-10-06T16:00:00+03:00", end: "2026-10-06T17:00:00+03:00"},
		{in: "завтра в 10:30 стоматолог", intent: IntentEvent, title: "Стоматолог", sphere: "health", start: "2026-10-04T10:30:00+03:00"},
		{in: "купить продукты завтра", intent: IntentTask, title: "Купить продукты", sphere: "home", date: "2026-10-04"},
		{in: "уборка каждую субботу", intent: IntentTask, title: "Уборка", sphere: "home", rrule: "FREQ=WEEKLY;BYDAY=SA"},
		{in: "спорт по вт и чт в 19 на полтора часа", intent: IntentEvent, title: "Спорт", sphere: "health",
			rrule: "FREQ=WEEKLY;BYDAY=TU,TH", start: "2026-10-03T19:00:00+03:00"},
		{in: "уборка 40м", intent: IntentTimeLog, title: "Уборка", sphere: "home", minutes: 40, timeItem: "u1"},
		{in: "подготовка к собесу 1ч20", intent: IntentTimeLog, title: "Подготовка к собесу", sphere: "career", minutes: 80, timeItem: "s1"},
		{in: "чтение 1.5ч", intent: IntentTimeLog, title: "Чтение", minutes: 90},
		{in: "сдать отчёт до пятницы срочно", intent: IntentTask, title: "Сдать отчёт", deadline: "2026-10-09T23:59:00+03:00", urgent: true},
		{in: "оплатить интернет 10.10", intent: IntentTask, title: "Оплатить интернет", sphere: "finance", date: "2026-10-10"},
		{in: "курс 1с 12 октября в 19:00 на 2 часа", intent: IntentEvent, title: "Курс 1с", sphere: "study",
			start: "2026-10-12T19:00:00+03:00", end: "2026-10-12T21:00:00+03:00", estimate: 120},
		{in: "позвонить маме через 2 часа", intent: IntentEvent, title: "Позвонить маме", start: "2026-10-03T20:00:00+03:00"},
		{in: "идея: бот для дневника", intent: IntentNote, title: "Бот для дневника", sphere: "product"},
		{in: "важно резюме обновить на выходных", intent: IntentTask, title: "Резюме обновить", sphere: "career", date: "2026-10-03", important: true},
		{in: "уборка в субботу\nУточнение: не, в воскресенье", intent: IntentTask, title: "Уборка", sphere: "home", date: "2026-10-04"},
		{in: "купить 2 яблока", intent: IntentTask, title: "Купить 2 яблока", sphere: "home"},
	}
	for _, c := range cases {
		r, err := Rules{}.Parse(context.Background(), Input{Text: c.in, Now: now, Candidates: cands})
		if err != nil {
			t.Fatal(err)
		}
		check := func(field, got, want string) {
			if got != want {
				t.Errorf("%q: %s = %q, want %q (%s)", c.in, field, got, want, r)
			}
		}
		check("intent", string(r.Intent), string(c.intent))
		check("title", r.Title, c.title)
		check("sphere", deref(r.Sphere), or(c.sphere))
		check("date", deref(r.PlannedDate), or(c.date))
		check("start", deref(r.StartAt), or(c.start))
		if c.end != "" || r.EndAt != nil && c.estimate == 0 && c.intent != IntentEvent {
			check("end", deref(r.EndAt), or(c.end))
		}
		check("rrule", deref(r.RRule), or(c.rrule))
		check("deadline", deref(r.Deadline), or(c.deadline))
		check("time_item", deref(r.TimeItemID), or(c.timeItem))
		if c.minutes != 0 && (r.TimeMinutes == nil || *r.TimeMinutes != c.minutes) {
			t.Errorf("%q: minutes = %v, want %d", c.in, r.TimeMinutes, c.minutes)
		}
		if c.estimate != 0 && (r.EstimateMin == nil || *r.EstimateMin != c.estimate) {
			t.Errorf("%q: estimate = %v, want %d", c.in, r.EstimateMin, c.estimate)
		}
		if r.Urgent != c.urgent || r.Important != c.important {
			t.Errorf("%q: urgent=%v important=%v", c.in, r.Urgent, r.Important)
		}
		// Результат всегда должен превращаться в операции плана.
		if c.intent != IntentNote || r.Title != "" {
			if _, err := r.Ops("inbox-1"); err != nil {
				t.Errorf("%q: Ops: %v", c.in, err)
			}
		}
	}
}

func or(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
