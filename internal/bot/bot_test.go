package bot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/parse"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
	"gitlab.com/KosovAndrey/lifeplan/internal/testdb"
)

const owner = 42

type sent struct {
	id   int64
	text string
	kb   Keyboard
}

type fakeTG struct {
	next  int64
	sent  []sent
	edits map[int64]string
}

func (f *fakeTG) Send(_ int64, text string, kb Keyboard) (int64, error) {
	f.next++
	f.sent = append(f.sent, sent{f.next, text, kb})
	return f.next, nil
}
func (f *fakeTG) Edit(_, id int64, text string, _ Keyboard) error {
	if f.edits == nil {
		f.edits = map[int64]string{}
	}
	f.edits[id] = text
	return nil
}
func (f *fakeTG) AnswerCallback(string, string) error { return nil }
func (f *fakeTG) Download(string) ([]byte, error)     { return []byte("ogg"), nil }

func (f *fakeTG) last() sent { return f.sent[len(f.sent)-1] }

type fakeParser struct {
	fn   func(parse.Input) (parse.Result, error)
	seen []parse.Input
}

func (p *fakeParser) Parse(_ context.Context, in parse.Input) (parse.Result, error) {
	p.seen = append(p.seen, in)
	return p.fn(in)
}

type fakeSTT struct{}

func (fakeSTT) Transcribe(context.Context, []byte, string) (string, error) {
	return "завтра купить продукты", nil
}

func ptr[T any](v T) *T { return &v }

func setup(t *testing.T, fn func(parse.Input) (parse.Result, error)) (*Bot, *fakeTG, *fakeParser, *store.Store) {
	st := store.New(testdb.New(t))
	tg := &fakeTG{}
	p := &fakeParser{fn: fn}
	b := New(st, tg, owner, p, fakeSTT{})
	return b, tg, p, st
}

func button(t *testing.T, s sent, prefix string) string {
	t.Helper()
	for _, row := range s.kb {
		for _, b := range row {
			if strings.HasPrefix(b.Data, prefix) {
				return b.Data
			}
		}
	}
	t.Fatalf("нет кнопки %s в %q", prefix, s.text)
	return ""
}

func TestTextToTaskViaButton(t *testing.T) {
	ctx := context.Background()
	b, tg, _, st := setup(t, func(in parse.Input) (parse.Result, error) {
		return parse.Result{Intent: parse.IntentEvent, Title: "Созвон с Т-Банком", Sphere: ptr("career"),
			Important: true, StartAt: ptr("2026-10-06T16:00:00+03:00"), EndAt: ptr("2026-10-06T17:00:00+03:00"),
			Confident: true}, nil
	})
	b.Handle(ctx, Incoming{ChatID: owner, MessageID: 1, Text: "во вторник созвон с тбанком 16-17"})

	card := tg.last()
	if !strings.Contains(card.text, "📅 Событие") || !strings.Contains(card.text, "вт 06.10 16:00–17:00") {
		t.Fatalf("карточка: %q", card.text)
	}
	if items, _ := st.ListItems(ctx, store.Filter{}); len(items) != 0 {
		t.Fatal("до нажатия задача создаваться не должна")
	}

	b.Handle(ctx, Incoming{ChatID: owner, CallbackID: "c1", CallbackData: button(t, card, "a:"), CallbackMsgID: card.id})
	if !strings.HasPrefix(tg.edits[card.id], "✅ Добавлено") {
		t.Fatalf("карточка после кнопки: %q", tg.edits[card.id])
	}
	items, _ := st.ListItems(ctx, store.Filter{})
	if len(items) != 1 || items[0].Source != "tg" || items[0].Kind != domain.KindEvent {
		t.Fatalf("задачи: %+v", items)
	}
	if inbox, _ := st.OpenInbox(ctx); len(inbox) != 0 {
		t.Fatalf("входящее должно закрыться, осталось %d", len(inbox))
	}

	// Повторное нажатие не создаёт дубль.
	b.Handle(ctx, Incoming{ChatID: owner, CallbackID: "c2", CallbackData: button(t, card, "a:"), CallbackMsgID: card.id})
	if items, _ := st.ListItems(ctx, store.Filter{}); len(items) != 1 {
		t.Fatalf("дубль: %d", len(items))
	}
}

func TestReplyCorrectsParse(t *testing.T) {
	ctx := context.Background()
	b, tg, p, _ := setup(t, func(in parse.Input) (parse.Result, error) {
		date := "2026-10-10"
		if in.Previous != nil {
			date = "2026-10-11"
		}
		return parse.Result{Intent: parse.IntentTask, Title: "Уборка", PlannedDate: &date, Confident: true}, nil
	})
	b.Handle(ctx, Incoming{ChatID: owner, MessageID: 1, Text: "уборка в субботу"})
	first := tg.last()
	b.Handle(ctx, Incoming{ChatID: owner, MessageID: 3, Text: "не, в воскресенье", ReplyTo: first.id})

	if len(p.seen) != 2 || p.seen[1].Previous == nil || !strings.Contains(p.seen[1].Text, "Уточнение: не, в воскресенье") {
		t.Fatalf("второй разбор: %+v", p.seen)
	}
	if !strings.Contains(tg.last().text, "вс 11.10") || tg.edits[first.id] == "" {
		t.Fatalf("новая карточка: %q, старая: %q", tg.last().text, tg.edits[first.id])
	}
}

func TestParserFailureKeepsNote(t *testing.T) {
	ctx := context.Background()
	b, tg, _, st := setup(t, func(parse.Input) (parse.Result, error) { return parse.Result{}, errors.New("timeout") })
	b.Handle(ctx, Incoming{ChatID: owner, MessageID: 1, Text: "что-то важное"})
	if !strings.Contains(tg.last().text, "разберём вечером") {
		t.Fatalf("ответ: %q", tg.last().text)
	}
	inbox, _ := st.OpenInbox(ctx)
	if len(inbox) != 1 || inbox[0].ParseError == nil {
		t.Fatalf("входящие: %+v", inbox)
	}
}

func TestVoiceAndTimeLog(t *testing.T) {
	ctx := context.Background()
	var taskID string
	b, tg, p, st := setup(t, func(in parse.Input) (parse.Result, error) {
		if in.Text == "уборка 40м" {
			return parse.Result{Intent: parse.IntentTimeLog, Title: "Уборка", TimeMinutes: ptr(40), TimeItemID: &taskID, Confident: true}, nil
		}
		return parse.Result{Intent: parse.IntentTask, Title: "Купить продукты", Confident: true}, nil
	})
	it, err := st.CreateItem(ctx, domain.Item{Title: "Уборка"}, "me")
	if err != nil {
		t.Fatal(err)
	}
	taskID = it.ID

	b.Handle(ctx, Incoming{ChatID: owner, MessageID: 1, VoiceID: "f1"})
	if !strings.HasPrefix(tg.last().text, "🎤 «завтра купить продукты»") {
		t.Fatalf("голос: %q", tg.last().text)
	}

	b.Handle(ctx, Incoming{ChatID: owner, MessageID: 2, Text: "уборка 40м"})
	if len(p.seen[1].Candidates) == 0 {
		t.Fatal("кандидаты для времени не переданы")
	}
	card := tg.last()
	b.Handle(ctx, Incoming{ChatID: owner, CallbackID: "c", CallbackData: button(t, card, "a:"), CallbackMsgID: card.id})
	got, _ := st.GetItem(ctx, taskID)
	if got.SpentMin != 40 {
		t.Fatalf("время: %d, карточка %q", got.SpentMin, tg.edits[card.id])
	}
}

func TestStrangerIgnored(t *testing.T) {
	ctx := context.Background()
	b, tg, p, st := setup(t, nil)
	b.Handle(ctx, Incoming{ChatID: 7, MessageID: 1, Text: "привет"})
	if len(tg.sent) != 0 || len(p.seen) != 0 {
		t.Fatal("чужому не отвечаем")
	}
	if inbox, _ := st.OpenInbox(ctx); len(inbox) != 0 {
		t.Fatal("чужое не сохраняем")
	}
}

func TestEveningBriefOnce(t *testing.T) {
	ctx := context.Background()
	b, tg, _, st := setup(t, nil)
	b.parser = nil
	tomorrow := domain.Today().AddDays(1)
	if _, err := st.CreateItem(ctx, domain.Item{Title: "Купить продукты", PlannedDate: &tomorrow}, "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddInbox(ctx, domain.InboxMessage{Text: "мысль"}); err != nil {
		t.Fatal(err)
	}
	y, m, d := time.Now().In(domain.MSK).Date()
	b.now = func() time.Time { return time.Date(y, m, d, 21, 5, 0, 0, domain.MSK) }

	for range 2 {
		if err := b.briefTick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(tg.sent) != 1 {
		t.Fatalf("брифов: %d", len(tg.sent))
	}
	text := tg.sent[0].text
	if !strings.HasPrefix(text, "🌙 Завтра") || !strings.Contains(text, "Купить продукты") || !strings.Contains(text, "Во входящих 1") {
		t.Fatalf("бриф: %q", text)
	}
}

func TestPreparedBriefWins(t *testing.T) {
	ctx := context.Background()
	b, tg, _, st := setup(t, nil)
	if err := st.PutBrief(ctx, domain.Today(), "morning", "Свой бриф от Claude"); err != nil {
		t.Fatal(err)
	}
	y, m, d := time.Now().In(domain.MSK).Date()
	b.now = func() time.Time { return time.Date(y, m, d, 7, 0, 0, 0, domain.MSK) }
	if err := b.briefTick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(tg.sent) != 1 || tg.sent[0].text != "Свой бриф от Claude" {
		t.Fatalf("бриф: %+v", tg.sent)
	}
	// Обновлённый после отправки бриф уходит ещё раз (разбор позже 21:00).
	if err := st.PutBrief(ctx, domain.Today(), "morning", "Обновлённый"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := b.briefTick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(tg.sent) != 2 || tg.sent[1].text != "Обновлённый" {
		t.Fatalf("повторная отправка: %+v", tg.sent)
	}
}

func TestWeeklyReport(t *testing.T) {
	ctx := context.Background()
	b, tg, _, st := setup(t, nil)
	// Ближайшее прошедшее (или сегодняшнее) воскресенье.
	sunday := domain.Today()
	for sunday.Weekday() != time.Sunday {
		sunday = sunday.AddDays(-1)
	}
	d := sunday.AddDays(-2)
	it, _ := st.CreateItem(ctx, domain.Item{Title: "Отчёт", PlannedDate: &d, EstimateMin: ptr(30)}, "me")
	at := time.Date(d.Year(), d.Month(), d.Day(), 10, 0, 0, 0, domain.MSK)
	if _, err := st.LogTime(ctx, domain.TimeEntry{ItemID: it.ID, Minutes: 45, StartedAt: &at}); err != nil {
		t.Fatal(err)
	}
	b.now = func() time.Time {
		return time.Date(sunday.Year(), sunday.Month(), sunday.Day(), 19, 30, 0, 0, domain.MSK)
	}
	text, err := b.composeWeekly(ctx, sunday)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "📊 Неделя") || !strings.Contains(text, "Учтено времени: 45м") || !strings.Contains(text, "План выполнен на 0%") {
		t.Fatalf("отчёт:\n%s", text)
	}
	_ = tg
}

func TestDiaryFromBot(t *testing.T) {
	ctx := context.Background()
	b, tg, _, st := setup(t, nil)
	b.parser = nil
	y, m, d := time.Now().In(domain.MSK).Date()
	b.now = func() time.Time { return time.Date(y, m, d, 21, 10, 0, 0, domain.MSK) }
	if err := b.briefTick(ctx); err != nil {
		t.Fatal(err)
	}
	brief := tg.last()
	if !strings.Contains(brief.text, "Как прошёл день?") || len(brief.kb) != 1 || len(brief.kb[0]) != 5 {
		t.Fatalf("вечерний бриф: %q %v", brief.text, brief.kb)
	}

	// Смайлик → настроение за сегодня.
	b.Handle(ctx, Incoming{ChatID: owner, CallbackID: "c", CallbackData: brief.kb[0][3].Data, CallbackMsgID: brief.id})
	// Ответ на бриф → текст в дневник за сегодня (бриф — про завтра).
	b.Handle(ctx, Incoming{ChatID: owner, MessageID: 9, Text: "Сдал собес, устал", ReplyTo: brief.id})
	// /d → дописать ещё.
	b.Handle(ctx, Incoming{ChatID: owner, MessageID: 10, Text: "/d вечером погулял"})

	j, err := st.Journal(ctx, domain.Today())
	if err != nil {
		t.Fatal(err)
	}
	if j.Mood == nil || *j.Mood != 4 || !strings.Contains(j.Text, "Сдал собес, устал") || !strings.Contains(j.Text, "вечером погулял") {
		t.Fatalf("дневник: %+v", j)
	}
	if inbox, _ := st.OpenInbox(ctx); len(inbox) != 0 {
		t.Fatal("ответ на бриф не должен падать во входящие")
	}
}
