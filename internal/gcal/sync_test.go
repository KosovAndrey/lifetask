package gcal

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
	"gitlab.com/KosovAndrey/lifeplan/internal/testdb"
)

// fakeGoogle — календари и задачи в памяти; etag меняется при каждой записи.
type fakeGoogle struct {
	cals    map[string]map[string]Event // calendar → event id → event
	names   map[string]string
	tasks   map[string]Task
	done    map[string]bool
	seq     int
	inserts int
	updates int
	deletes []string
}

func newFake() *fakeGoogle {
	return &fakeGoogle{cals: map[string]map[string]Event{Primary: {}}, names: map[string]string{},
		tasks: map[string]Task{}, done: map[string]bool{}}
}

func (f *fakeGoogle) etag() string { f.seq++; return fmt.Sprintf(`"%d"`, f.seq) }

func (f *fakeGoogle) FindCalendar(_ context.Context, name string) (string, error) {
	if id, ok := f.names[name]; ok {
		return id, nil
	}
	return "", ErrNotFound
}
func (f *fakeGoogle) CreateCalendar(_ context.Context, name string) (string, error) {
	id := "cal-" + name
	f.names[name], f.cals[id] = id, map[string]Event{}
	return id, nil
}
func (f *fakeGoogle) ListEvents(_ context.Context, cal string, from, to time.Time) ([]Event, error) {
	var out []Event
	for _, e := range f.cals[cal] {
		if e.Start != nil && (e.Start.Before(from) || !e.Start.Before(to)) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
func (f *fakeGoogle) InsertEvent(_ context.Context, cal string, e Event) (Event, error) {
	f.inserts++
	e.ID, e.Etag = fmt.Sprintf("ev%d", f.seq+1), f.etag()
	f.cals[cal][e.ID] = e
	return e, nil
}
func (f *fakeGoogle) UpdateEvent(_ context.Context, cal string, e Event) (Event, error) {
	if _, ok := f.cals[cal][e.ID]; !ok {
		return Event{}, ErrNotFound
	}
	f.updates++
	e.Etag = f.etag()
	f.cals[cal][e.ID] = e
	return e, nil
}
func (f *fakeGoogle) DeleteEvent(_ context.Context, cal, id string) error {
	f.deletes = append(f.deletes, id)
	delete(f.cals[cal], id)
	return nil
}
func (f *fakeGoogle) FindTaskList(context.Context, string) (string, error) { return "list", nil }
func (f *fakeGoogle) CreateTaskList(context.Context, string) (string, error) {
	return "list", nil
}
func (f *fakeGoogle) OpenTasks(context.Context, string) ([]Task, error) {
	var out []Task
	for id, t := range f.tasks {
		if !f.done[id] {
			out = append(out, t)
		}
	}
	return out, nil
}
func (f *fakeGoogle) CompleteTask(_ context.Context, _, id string) error {
	f.done[id] = true
	return nil
}

// userEdit — правка руками в Google: новое содержимое и новый etag.
func (f *fakeGoogle) userEdit(cal, id string, fn func(*Event)) {
	e := f.cals[cal][id]
	fn(&e)
	e.Etag = f.etag()
	f.cals[cal][id] = e
}

func at(day, hm string) *time.Time {
	t, _ := time.ParseInLocation("2006-01-02 15:04", day+" "+hm, domain.MSK)
	return &t
}

func setup(t *testing.T) (*Syncer, *fakeGoogle, *store.Store, context.Context) {
	st := store.New(testdb.New(t))
	f := newFake()
	return NewSyncer(st, f), f, st, context.Background()
}

func once(t *testing.T, s *Syncer) {
	t.Helper()
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPushAndPullOwnCalendar(t *testing.T) {
	s, f, st, ctx := setup(t)
	day := domain.Today().AddDays(1).String()
	work, _ := st.SphereBySlug(ctx, "work")
	it, err := st.CreateItem(ctx, domain.Item{Kind: domain.KindEvent, Title: "Созвон", SphereID: &work,
		StartAt: at(day, "16:00"), EndAt: at(day, "17:00"),
		Body: []domain.Block{{"type": "md", "text": "повестка"}}}, "me")
	if err != nil {
		t.Fatal(err)
	}
	// Задача без времени в календарь не идёт.
	if _, err := st.CreateItem(ctx, domain.Item{Title: "Без времени"}, "me"); err != nil {
		t.Fatal(err)
	}

	once(t, s)
	cal := f.names[CalendarName]
	if len(f.cals[cal]) != 1 {
		t.Fatalf("событий: %d", len(f.cals[cal]))
	}
	var ev Event
	for _, e := range f.cals[cal] {
		ev = e
	}
	if ev.LifeplanID != it.ID || ev.ColorID != "9" || ev.Description != "повестка\n— LifeTask" {
		t.Fatalf("событие: %+v", ev)
	}

	// Повторный проход ничего не пишет: нет эха.
	once(t, s)
	if f.inserts != 1 || f.updates != 0 {
		t.Fatalf("эхо: inserts=%d updates=%d", f.inserts, f.updates)
	}

	// Перенесли в Google на час позже — подтянулось к нам и не выгрузилось обратно.
	f.userEdit(cal, ev.ID, func(e *Event) {
		e.Start, e.End = at(day, "17:00"), at(day, "18:00")
		e.Summary = "Созвон с командой"
	})
	once(t, s)
	got, _ := st.GetItem(ctx, it.ID)
	if !got.StartAt.Equal(*at(day, "17:00")) || got.Title != "Созвон с командой" || f.updates != 0 {
		t.Fatalf("после правки в Google: %+v updates=%d", got, f.updates)
	}

	// Отметили выполненным у нас — в Google галочка в названии.
	if _, err := st.UpdateItem(ctx, it.ID, store.Patch{"status": json.RawMessage(`"done"`)}, "me"); err != nil {
		t.Fatal(err)
	}
	once(t, s)
	if f.cals[cal][ev.ID].Summary != "✓ Созвон с командой" {
		t.Fatalf("done: %q", f.cals[cal][ev.ID].Summary)
	}

	// Удалили событие в Google — задача у нас отменяется (если не закрыта) и отвязывается.
	it2, _ := st.CreateItem(ctx, domain.Item{Title: "Тренировка", StartAt: at(day, "19:00")}, "me")
	once(t, s)
	sync2, _ := st.SyncedByID(ctx, it2.ID)
	delete(f.cals[cal], *sync2.EventID)
	once(t, s)
	got2, _ := st.GetItem(ctx, it2.ID)
	if got2.Status != domain.StatusCancelled {
		t.Fatalf("после удаления в Google: %s", got2.Status)
	}
	if after, _ := st.SyncedByID(ctx, it2.ID); after.EventID != nil {
		t.Fatal("должна отвязаться")
	}
}

func TestLocalDeleteAndUnschedule(t *testing.T) {
	s, f, st, ctx := setup(t)
	day := domain.Today().AddDays(2).String()
	a, _ := st.CreateItem(ctx, domain.Item{Title: "A", StartAt: at(day, "10:00")}, "me")
	b, _ := st.CreateItem(ctx, domain.Item{Title: "B", StartAt: at(day, "12:00")}, "me")
	once(t, s)
	cal := f.names[CalendarName]

	if err := st.DeleteItem(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateItem(ctx, b.ID, store.Patch{"start_at": json.RawMessage("null"),
		"planned_date": mustJSON(day)}, "me"); err != nil {
		t.Fatal(err)
	}
	once(t, s)
	if len(f.cals[cal]) != 0 || len(f.deletes) != 2 {
		t.Fatalf("осталось событий %d, удалений %d", len(f.cals[cal]), len(f.deletes))
	}
	if tombs, _ := st.Tombstones(ctx); len(tombs) != 0 {
		t.Fatal("надгробия должны убраться")
	}
}

func TestPrimaryMirror(t *testing.T) {
	s, f, st, ctx := setup(t)
	day := domain.Today().AddDays(1).String()
	f.cals[Primary]["p1"] = Event{ID: "p1", Summary: "Стоматолог", Start: at(day, "09:00"), End: at(day, "10:00"), Etag: f.etag()}
	f.cals[Primary]["p2"] = Event{ID: "p2", Summary: "ДР Пети", Date: day, Etag: f.etag()}

	once(t, s)
	d, _ := domain.ParseDate(day)
	dayView, _ := st.Day(ctx, d)
	if len(dayView.Scheduled) != 1 || dayView.Scheduled[0].Source != "gcal" || len(dayView.Planned) != 1 {
		t.Fatalf("день: %+v", dayView)
	}
	// Зеркало не выгружается в наш календарь.
	if len(f.cals[f.names[CalendarName]]) != 0 {
		t.Fatal("зеркало primary не должно уходить в LifeTask")
	}

	// Удалили в Google — зеркало пропало, а чужой календарь мы не трогали.
	delete(f.cals[Primary], "p1")
	once(t, s)
	dayView, _ = st.Day(ctx, d)
	if len(dayView.Scheduled) != 0 || len(f.deletes) != 0 {
		t.Fatalf("после удаления: %d зеркал, удалений в Google %d", len(dayView.Scheduled), len(f.deletes))
	}
}

func TestTasksToInbox(t *testing.T) {
	s, f, st, ctx := setup(t)
	f.tasks["t1"] = Task{ID: "t1", Title: "Позвонить в банк", Due: "2026-10-05T00:00:00.000Z"}
	once(t, s)
	once(t, s)
	inbox, _ := st.OpenInbox(ctx)
	if len(inbox) != 1 || inbox[0].Text != "Позвонить в банк\nсрок: 2026-10-05" || !f.done["t1"] {
		t.Fatalf("входящие: %+v", inbox)
	}
}

func TestNearestColor(t *testing.T) {
	cases := map[string]string{"#3f51b5": "9", "#d00000": "11", "#10B981": "2", "#F59E0B": "5", "#646464": "8", "bad": ""}
	for in, want := range cases {
		if got := NearestColor(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
