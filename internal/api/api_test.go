package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitlab.com/KosovAndrey/lifeplan/internal/api"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
	"gitlab.com/KosovAndrey/lifeplan/internal/testdb"
)

const token = "test-token-0123456789abcdef"

type client struct {
	t *testing.T
	h http.Handler
}

func (c client) do(method, path, body string, out any) int {
	c.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			c.t.Fatalf("%s %s: %v: %s", method, path, err, rec.Body)
		}
	}
	return rec.Code
}

func TestFlow(t *testing.T) {
	h := api.New(store.New(testdb.New(t)), token).Handler()
	c := client{t, h}

	// Без токена — 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/spheres", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена: %d", rec.Code)
	}

	// Входящее с телефона.
	var msg struct{ ID string }
	if code := c.do("POST", "/api/inbox", `{"text":"уборка в субботу"}`, &msg); code != 201 {
		t.Fatalf("inbox: %d", code)
	}

	// Claude предлагает разобрать его в задачу.
	var plan struct {
		ID      string
		Preview []string
		Refs    map[string]string
	}
	body := `{"summary":"разбор","ops":[
		{"op":"create","ref":"u","item":{"title":"Уборка","sphere":"home","planned_date":"2026-10-10","estimate_min":60}},
		{"op":"inbox","id":"` + msg.ID + `","status":"accepted","to":"$u"}]}`
	if code := c.do("POST", "/api/plans", body, &plan); code != 201 {
		t.Fatalf("propose: %d", code)
	}
	var inbox []any
	c.do("GET", "/api/inbox", "", &inbox)
	if len(inbox) != 1 {
		t.Fatalf("до apply инбокс должен остаться: %d", len(inbox))
	}
	if code := c.do("POST", "/api/plans/"+plan.ID+"/apply", "", &plan); code != 200 {
		t.Fatalf("apply: %d", code)
	}
	c.do("GET", "/api/inbox", "", &inbox)
	if len(inbox) != 0 {
		t.Fatalf("после apply инбокс пуст, а там %d", len(inbox))
	}

	// Ручная правка из веба: перенос на воскресенье.
	var it struct {
		PostponeCount int `json:"postpone_count"`
		Quadrant      int
	}
	if code := c.do("PATCH", "/api/items/"+plan.Refs["u"], `{"planned_date":"2026-10-11","important":true}`, &it); code != 200 {
		t.Fatalf("patch: %d", code)
	}
	if it.PostponeCount != 1 || it.Quadrant != 2 {
		t.Fatalf("после переноса: %+v", it)
	}

	var day struct{ Planned []any }
	c.do("GET", "/api/day/2026-10-11", "", &day)
	if len(day.Planned) != 1 {
		t.Fatalf("день: %+v", day)
	}

	// Ошибки валидации — 400, неизвестная задача — 404.
	if code := c.do("PATCH", "/api/items/"+plan.Refs["u"], `{"status":"бред"}`, nil); code != 400 {
		t.Fatalf("плохой статус: %d", code)
	}
	if code := c.do("GET", "/api/items/00000000-0000-0000-0000-000000000000", "", nil); code != 404 {
		t.Fatalf("нет задачи: %d", code)
	}
}

func TestWebSession(t *testing.T) {
	st := store.New(testdb.New(t))
	a := api.New(st, token)
	h := a.Handler()
	ctx := t.Context()

	do := func(method, path, cookie string, hdr map[string]string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "lp_session", Value: cookie})
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("GET", "/api/day/today", "", nil, ""); rec.Code != 401 {
		t.Fatalf("без сессии: %d", rec.Code)
	}
	tok, err := st.NewLoginToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rec := do("GET", "/login?t="+tok, "", nil, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("вход: %d", rec.Code)
	}
	var sid string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "lp_session" && c.HttpOnly {
			sid = c.Value
		}
	}
	if sid == "" {
		t.Fatal("нет HttpOnly-cookie")
	}
	if rec := do("GET", "/login?t="+tok, "", nil, ""); rec.Code != 401 {
		t.Fatalf("повторный вход по той же ссылке: %d", rec.Code)
	}
	if rec := do("GET", "/api/day/today", sid, nil, ""); rec.Code != 200 {
		t.Fatalf("чтение по сессии: %d", rec.Code)
	}
	// Запись по cookie без X-Requested-With — отказ (CSRF).
	if rec := do("POST", "/api/items", sid, nil, `{"title":"x"}`); rec.Code != 403 {
		t.Fatalf("CSRF: %d", rec.Code)
	}
	if rec := do("POST", "/api/items", sid, map[string]string{"X-Requested-With": "lifetask"}, `{"title":"x"}`); rec.Code != 201 {
		t.Fatalf("запись по сессии: %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/api/logout", sid, map[string]string{"X-Requested-With": "lifetask"}, ""); rec.Code != 204 {
		t.Fatalf("выход: %d", rec.Code)
	}
	if rec := do("GET", "/api/day/today", sid, nil, ""); rec.Code != 401 {
		t.Fatalf("после выхода: %d", rec.Code)
	}
}

func TestIdempotencyAndProgress(t *testing.T) {
	st := store.New(testdb.New(t))
	h := api.New(st, token).Handler()
	post := func(body, key string) (int, string) {
		req := httptest.NewRequest("POST", "/api/items", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	firstCode, firstBody := post(`{"title":"Цель","kind":"goal"}`, "k1")
	if firstCode != 201 {
		t.Fatalf("первый: %d", firstCode)
	}
	if code, body := post(`{"title":"Цель","kind":"goal"}`, "k1"); code != firstCode || body != firstBody {
		t.Fatalf("повтор: %d %s", code, body)
	}
	items, _ := st.ListItems(t.Context(), store.Filter{})
	if len(items) != 1 {
		t.Fatalf("дубль создан: %d", len(items))
	}
	// Повтор возвращает тот же результат, включая id созданной задачи.
	goal := items[0].ID
	_, _ = post(`{"title":"a","parent_id":"`+goal+`","weight":3,"status":"done"}`, "k2")
	_, _ = post(`{"title":"b","parent_id":"`+goal+`","weight":1}`, "k3")
	got, _ := st.GetItem(t.Context(), goal)
	if got.Progress == nil || *got.Progress != 0.75 {
		t.Fatalf("прогресс по весам: %v", got.Progress)
	}
}

func TestQuickInput(t *testing.T) {
	st := store.New(testdb.New(t))
	c := client{t, api.New(st, token).Handler()}
	var p struct{ Preview []string }
	if code := c.do("POST", "/api/quick", `{"text":"уборка каждую субботу"}`, &p); code != 200 {
		t.Fatalf("повтор: %d", code)
	}
	if len(p.Preview) != 1 || !strings.Contains(p.Preview[0], "🔁 «Уборка» по сб") {
		t.Fatalf("превью: %q", p.Preview)
	}
	// Входящее → задача на день экрана, входящее закрывается.
	var msg struct{ ID string }
	c.do("POST", "/api/inbox", `{"text":"позвонить в банк"}`, &msg)
	if code := c.do("POST", "/api/quick", `{"inbox_id":"`+msg.ID+`"}`, &p); code != 200 {
		t.Fatalf("оформить: %d", code)
	}
	var inbox []any
	c.do("GET", "/api/inbox", "", &inbox)
	if len(inbox) != 0 {
		t.Fatalf("входящее не закрылось: %d", len(inbox))
	}
	if code := c.do("POST", "/api/quick", `{"text":"купить хлеб","date":"2026-10-07"}`, &p); code != 200 || !strings.Contains(p.Preview[0], "ср 07.10") {
		t.Fatalf("дата экрана: %d %q", code, p.Preview)
	}
}

func TestProjectsRecurrencesNotes(t *testing.T) {
	st := store.New(testdb.New(t))
	c := client{t, api.New(st, token).Handler()}
	var p struct{ ID int }
	if code := c.do("POST", "/api/projects", `{"name":"LifeTask"}`, &p); code != 201 || p.ID == 0 {
		t.Fatalf("проект: %d", code)
	}
	var rec struct{ ID string }
	if code := c.do("POST", "/api/recurrences", `{"rule":"FREQ=DAILY","time":"08:00","duration_min":20,"item":{"title":"Зарядка"}}`, &rec); code != 201 {
		t.Fatalf("повтор: %d", code)
	}
	var items []struct{ ID string }
	c.do("GET", "/api/items?q=Зарядка", "", &items)
	if len(items) != store.RecurHorizonDays+1 {
		t.Fatalf("экземпляров: %d", len(items))
	}
	var stopped struct{ Removed int }
	if code := c.do("POST", "/api/recurrences/"+rec.ID+"/stop", `{}`, &stopped); code != 200 || stopped.Removed != len(items) {
		t.Fatalf("остановка: %d removed=%d", code, stopped.Removed)
	}
	// Поиск заметок по тексту, а не только по заголовку.
	c.do("POST", "/api/items", `{"kind":"note","title":"Ментор","body":[{"type":"md","text":"спросить про on-call"}]}`, nil)
	var notes []struct{ Title string }
	c.do("GET", "/api/items?kind=note&q=on-call", "", &notes)
	if len(notes) != 1 || notes[0].Title != "Ментор" {
		t.Fatalf("поиск заметок: %+v", notes)
	}
}
