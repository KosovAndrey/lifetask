// Package api — HTTP JSON API. Единственный клиент с правом записи без плана —
// сам пользователь (веб, бот); ИИ пишет через /api/plans.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/changeplan"
	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/parse"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
)

type API struct {
	st    *store.Store
	plans *changeplan.Service
	token string
	// Static — веб-интерфейс (оболочка публичная, данные — только через /api с сессией).
	Static http.Handler
	// Secure — cookie только по HTTPS (прод за nginx); в разработке false.
	Secure bool
}

func New(st *store.Store, token string) *API {
	return &API{st: st, plans: changeplan.New(st), token: token}
}

const sessionCookie = "lp_session"

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /login", a.login)
	mux.HandleFunc("POST /api/logout", a.logout)
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	mux.HandleFunc("POST /api/inbox/{id}/resolve", a.resolveInbox)
	if a.Static != nil {
		mux.Handle("GET /", a.Static)
	}

	mux.HandleFunc("GET /api/spheres", a.spheres)
	mux.HandleFunc("GET /api/projects", a.projects)
	mux.HandleFunc("GET /api/items", a.listItems)
	mux.HandleFunc("POST /api/items", a.createItem)
	mux.HandleFunc("GET /api/items/{id}", a.getItem)
	mux.HandleFunc("PATCH /api/items/{id}", a.patchItem)
	mux.HandleFunc("DELETE /api/items/{id}", a.deleteItem)
	mux.HandleFunc("POST /api/items/{id}/time", a.logTime)
	mux.HandleFunc("GET /api/day/{date}", a.day)
	mux.HandleFunc("GET /api/week/{date}", a.week)
	mux.HandleFunc("GET /api/stats", a.stats)
	mux.HandleFunc("GET /api/recurrences", a.recurrences)
	mux.HandleFunc("POST /api/recurrences", a.createRecurrence)
	mux.HandleFunc("POST /api/recurrences/{id}/stop", a.stopRecurrence)
	mux.HandleFunc("POST /api/projects", a.createProject)
	mux.HandleFunc("GET /api/graph", a.graph)
	mux.HandleFunc("POST /api/quick", a.quick)
	mux.HandleFunc("GET /api/journal", a.journalList)
	mux.HandleFunc("GET /api/journal/{date}", a.journal)
	mux.HandleFunc("PATCH /api/journal/{date}", a.saveJournal)
	mux.HandleFunc("POST /api/relations", a.relate)
	mux.HandleFunc("DELETE /api/relations", a.relate)
	mux.HandleFunc("GET /api/inbox", a.inbox)
	mux.HandleFunc("POST /api/inbox", a.addInbox)
	mux.HandleFunc("PUT /api/briefs/{date}/{kind}", a.putBrief)
	mux.HandleFunc("POST /api/plans", a.proposePlan)
	mux.HandleFunc("GET /api/plans/{id}", a.getPlan)
	mux.HandleFunc("POST /api/plans/{id}/apply", a.applyPlan)
	mux.HandleFunc("POST /api/plans/{id}/reject", a.rejectPlan)
	return a.auth(a.idempotent(mux))
}

// statusRecorder запоминает код ответа обработчика.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) { r.code = code; r.ResponseWriter.WriteHeader(code) }

// idempotent: запись с заголовком Idempotency-Key выполняется один раз. Офлайн-очередь
// веба может повторить запрос, ответ на который потерялся, — дубля не будет.
// Ключ освобождается, если обработчик упал с 5xx: такой запрос можно повторить.
func (a *API) idempotent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" || r.Method == http.MethodGet || len(key) > 100 {
			next.ServeHTTP(w, r)
			return
		}
		fresh, err := a.st.ClaimIdempotencyKey(r.Context(), key)
		if err != nil {
			fail(w, err)
			return
		}
		if !fresh {
			writeJSON(w, http.StatusOK, map[string]bool{"duplicate": true})
			return
		}
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.code >= 500 {
			_ = a.st.ReleaseIdempotencyKey(r.Context(), key)
		}
	})
}

// auth: /api/* — Bearer-токен (CLI, Claude) или cookie-сессия (веб). Для записи
// по cookie нужен заголовок X-Requested-With: чужой сайт не может послать его
// без CORS-preflight, а CORS мы не разрешаем — это и есть защита от CSRF.
func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			if a.token != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(a.token)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
			writeErr(w, http.StatusUnauthorized, errors.New("неверный токен"))
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, errors.New("войди: напиши боту /login"))
			return
		}
		ok, err := a.st.ValidSession(r.Context(), c.Value)
		if err != nil {
			fail(w, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusUnauthorized, errors.New("сессия истекла: напиши боту /login"))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Requested-With") != "lifetask" {
			writeErr(w, http.StatusForbidden, errors.New("нет заголовка X-Requested-With"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// login — переход по одноразовой ссылке из бота: гасим токен, ставим cookie, на главную.
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	sid, err := a.st.Login(r.Context(), r.URL.Query().Get("t"), r.UserAgent())
	if err != nil {
		fail(w, err)
		return
	}
	if sid == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`<meta name="viewport" content="width=device-width"><p style="font:16px system-ui;padding:24px">` +
			`Ссылка устарела или уже использована. Напиши боту <b>/login</b> ещё раз.</p>`))
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: sid, Path: "/", HttpOnly: true, Secure: a.Secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(store.SessionTTL.Seconds())})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.st.Logout(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) resolveInbox(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status string `json:"status"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Status != "deferred" && req.Status != "rejected" {
		writeErr(w, http.StatusBadRequest, errors.New("из веба: deferred или rejected"))
		return
	}
	if err := a.st.ResolveInbox(r.Context(), r.PathValue("id"), req.Status, nil); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// fail раскладывает ошибки хранилища по кодам: не найдено → 404, ошибки БД → 500,
// остальное (валидация) → 400.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case isDBError(err):
		slog.Error("db", "err", err)
		writeErr(w, http.StatusInternalServerError, err)
	default:
		writeErr(w, http.StatusBadRequest, err)
	}
}

func isDBError(err error) bool {
	s := err.Error()
	// Нарушения ограничений (FK, CHECK) — ошибка запроса, а не сервера.
	if strings.Contains(s, "SQLSTATE 23") || strings.Contains(s, "SQLSTATE 22") {
		return false
	}
	return strings.Contains(s, "SQLSTATE") || strings.Contains(s, "connect")
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (a *API) spheres(w http.ResponseWriter, r *http.Request) {
	v, err := a.st.Spheres(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) projects(w http.ResponseWriter, r *http.Request) {
	v, err := a.st.Projects(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) listItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.Filter{Kind: q.Get("kind"), Query: q.Get("q"), Open: q.Get("open") == "1"}
	if s := q.Get("status"); s != "" {
		f.Status = strings.Split(s, ",")
	}
	if s := q.Get("sphere"); s != "" {
		id, err := a.st.SphereBySlug(r.Context(), s)
		if err != nil {
			fail(w, err)
			return
		}
		f.SphereID = id
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.DoneDays, _ = strconv.Atoi(q.Get("done_days"))
	v, err := a.st.ListItems(r.Context(), f)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) createItem(w http.ResponseWriter, r *http.Request) {
	var it domain.Item
	if err := decode(r, &it); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	created, err := a.st.CreateItem(r.Context(), it, "me")
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (a *API) getItem(w http.ResponseWriter, r *http.Request) {
	d, err := a.st.Detail(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (a *API) patchItem(w http.ResponseWriter, r *http.Request) {
	var p store.Patch
	if err := decode(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var out domain.Item
	err := a.st.InTx(r.Context(), func(tx *store.Store) error {
		var err error
		out, err = tx.UpdateItem(r.Context(), r.PathValue("id"), p, "me")
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) deleteItem(w http.ResponseWriter, r *http.Request) {
	if err := a.st.DeleteItem(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) logTime(w http.ResponseWriter, r *http.Request) {
	var e domain.TimeEntry
	if err := decode(r, &e); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	e.ItemID = r.PathValue("id")
	out, err := a.st.LogTime(r.Context(), e)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func dateParam(s string) (domain.Date, error) {
	if s == "today" {
		return domain.Today(), nil
	}
	if s == "tomorrow" {
		return domain.Today().AddDays(1), nil
	}
	return domain.ParseDate(s)
}

func (a *API) day(w http.ResponseWriter, r *http.Request) {
	d, err := dateParam(r.PathValue("date"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	v, err := a.st.Day(r.Context(), d)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// week — 7 дней с понедельника недели, в которую попадает дата.
func (a *API) week(w http.ResponseWriter, r *http.Request) {
	d, err := dateParam(r.PathValue("date"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	offset := (int(d.Weekday()) + 6) % 7
	start := d.AddDays(-offset)
	days := make([]store.Day, 0, 7)
	for i := range 7 {
		day, err := a.st.Day(r.Context(), start.AddDays(i))
		if err != nil {
			fail(w, err)
			return
		}
		day.Overdue = nil // просрочка — только в дне, в неделе она дублировалась бы
		days = append(days, day)
	}
	writeJSON(w, http.StatusOK, days)
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	to := domain.Today().AddDays(1)
	from := to.AddDays(-7)
	var err error
	if s := q.Get("from"); s != "" {
		if from, err = domain.ParseDate(s); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	if s := q.Get("to"); s != "" {
		if to, err = domain.ParseDate(s); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	v, err := a.st.Stats(r.Context(), from, to)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) inbox(w http.ResponseWriter, r *http.Request) {
	v, err := a.st.OpenInbox(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) addInbox(w http.ResponseWriter, r *http.Request) {
	var m domain.InboxMessage
	if err := decode(r, &m); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := a.st.AddInbox(r.Context(), m)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *API) proposePlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Author  string          `json:"author"`
		Summary string          `json:"summary"`
		Ops     []changeplan.Op `json:"ops"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Author == "" {
		req.Author = "claude"
	}
	p, err := a.plans.Propose(r.Context(), req.Author, req.Summary, req.Ops)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (a *API) getPlan(w http.ResponseWriter, r *http.Request) {
	p, err := a.plans.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) applyPlan(w http.ResponseWriter, r *http.Request) {
	p, err := a.plans.Apply(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) rejectPlan(w http.ResponseWriter, r *http.Request) {
	if err := a.plans.Reject(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// putBrief — свой текст брифа (готовит Claude на вечернем разборе); бот отправит его в своё время.
func (a *API) putBrief(w http.ResponseWriter, r *http.Request) {
	d, err := dateParam(r.PathValue("date"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.st.PutBrief(r.Context(), d, r.PathValue("kind"), req.Body); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) recurrences(w http.ResponseWriter, r *http.Request) {
	v, err := a.st.Recurrences(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) graph(w http.ResponseWriter, r *http.Request) {
	var sphereID int
	if slug := r.URL.Query().Get("sphere"); slug != "" {
		id, err := a.st.SphereBySlug(r.Context(), slug)
		if err != nil {
			fail(w, err)
			return
		}
		sphereID = id
	}
	g, err := a.st.Graph(r.Context(), sphereID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// relate — связь из веба: POST создаёт, DELETE снимает. Цикл зависимостей не пропустит Relate.
func (a *API) relate(w http.ResponseWriter, r *http.Request) {
	var rel domain.Relation
	if err := decode(r, &rel); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if rel.FromID == rel.ToID {
		writeErr(w, http.StatusBadRequest, errors.New("связь с самой собой"))
		return
	}
	err := a.st.InTx(r.Context(), func(tx *store.Store) error {
		if r.Method == http.MethodDelete {
			return tx.Unrelate(r.Context(), rel)
		}
		return tx.Relate(r.Context(), rel)
	})
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) journalList(w http.ResponseWriter, r *http.Request) {
	v, err := a.st.JournalList(r.Context(), 60)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) journal(w http.ResponseWriter, r *http.Request) {
	d, err := dateParam(r.PathValue("date"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	v, err := a.st.Journal(r.Context(), d)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) saveJournal(w http.ResponseWriter, r *http.Request) {
	d, err := dateParam(r.PathValue("date"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var p store.JournalPatch
	if err := decode(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	v, err := a.st.SaveJournal(r.Context(), d, p)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// quick — быстрый ввод из веба: текст (или входящее) разбирается правилами и сразу
// применяется планом. Без сети запрос ждёт в очереди и разбирается при отправке.
func (a *API) quick(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text    string `json:"text"`
		InboxID string `json:"inbox_id"`
		Date    string `json:"date"` // день экрана — если в тексте даты нет
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.InboxID != "" {
		m, err := a.st.GetInbox(r.Context(), req.InboxID)
		if err != nil {
			fail(w, err)
			return
		}
		req.Text = m.Content()
	}
	if strings.TrimSpace(req.Text) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("пустой текст"))
		return
	}
	cands, err := a.st.TimeCandidates(r.Context(), 30)
	if err != nil {
		fail(w, err)
		return
	}
	in := parse.Input{Text: req.Text, Now: time.Now()}
	for _, c := range cands {
		in.Candidates = append(in.Candidates, parse.Candidate{ID: c.ID, Title: c.Title})
	}
	res, _ := parse.Rules{}.Parse(r.Context(), in)
	if req.Date != "" && res.PlannedDate == nil && res.StartAt == nil && res.RRule == nil && res.Intent != parse.IntentTimeLog {
		res.PlannedDate = &req.Date
	}
	ops, err := res.Ops(req.InboxID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.InboxID == "" { // Ops всегда закрывает входящее — без него эта операция лишняя
		ops = ops[:len(ops)-1]
	}
	for i := range ops {
		if ops[i].Item != nil {
			ops[i].Item["source"] = json.RawMessage(`"web"`)
		}
	}
	p, err := a.plans.ApplyNow(r.Context(), "me", "Быстрый ввод: "+req.Text, ops)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) createProject(w http.ResponseWriter, r *http.Request) {
	var p domain.Project
	if err := decode(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := a.st.CreateProject(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// createRecurrence — серия из веба: шаблон задачи + правило; экземпляры на
// горизонт создаются сразу, в той же транзакции.
func (a *API) createRecurrence(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rule        string      `json:"rule"`
		Start       string      `json:"start"`
		Until       string      `json:"until"`
		Time        string      `json:"time"`
		DurationMin int         `json:"duration_min"`
		Item        domain.Item `json:"item"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	rec := store.Recurrence{Rule: req.Rule, Template: req.Item, Start: domain.Today()}
	var err error
	if req.Start != "" {
		if rec.Start, err = domain.ParseDate(req.Start); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	if req.Until != "" {
		u, err := domain.ParseDate(req.Until)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		rec.Until = &u
	}
	if req.Time != "" {
		rec.StartTime = &req.Time
	}
	if req.DurationMin > 0 {
		rec.DurationMin = &req.DurationMin
	}
	if rec.Template.Source == "" {
		rec.Template.Source = "web"
	}
	var out store.Recurrence
	err = a.st.InTx(r.Context(), func(tx *store.Store) error {
		var err error
		if out, err = tx.CreateRecurrence(r.Context(), rec); err != nil {
			return err
		}
		_, err = tx.GenerateOne(r.Context(), out.ID, domain.Today().AddDays(store.RecurHorizonDays))
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *API) stopRecurrence(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From string `json:"from"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	from := domain.Today()
	if req.From != "" {
		var err error
		if from, err = domain.ParseDate(req.From); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	var removed int
	err := a.st.InTx(r.Context(), func(tx *store.Store) error {
		var err error
		_, removed, err = tx.StopRecurrence(r.Context(), r.PathValue("id"), from)
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"removed": removed})
}
