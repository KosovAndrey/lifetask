package api

import (
	"encoding/json"
	"net/http"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
)

// routes — утренний план, таймер, напоминания (настройки), здоровье и разбор.
func (a *API) planningRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/plan/{date}", a.plan)
	mux.HandleFunc("POST /api/reschedule", a.reschedule)
	mux.HandleFunc("GET /api/timer", a.timer)
	mux.HandleFunc("POST /api/items/{id}/timer", a.startTimer)
	mux.HandleFunc("POST /api/timer/stop", a.stopTimer)
	mux.HandleFunc("DELETE /api/timer", a.discardTimer)
	mux.HandleFunc("GET /api/settings", a.settings)
	mux.HandleFunc("PATCH /api/settings", a.saveSettings)
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("PUT /api/health/{date}", a.putHealth)
	mux.HandleFunc("GET /api/review", a.review)
	mux.HandleFunc("POST /api/items/{id}/split", a.split)
	mux.HandleFunc("GET /api/counts", a.counts)
}

func (a *API) plan(w http.ResponseWriter, r *http.Request) {
	d, err := dateParam(r.PathValue("date"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	p, err := a.st.Plan(r.Context(), d)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// reschedule — перенос пачкой: «всё, что не влезает, — на завтра».
// date пустая или не задана — в «когда-нибудь».
func (a *API) reschedule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs  []string `json:"ids"`
		Date string   `json:"date"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var date *domain.Date
	if req.Date != "" {
		d, err := dateParam(req.Date)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		date = &d
	}
	var out []domain.Item
	err := a.st.InTx(r.Context(), func(tx *store.Store) error {
		var err error
		out, err = tx.Reschedule(r.Context(), req.IDs, date, "me")
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) timer(w http.ResponseWriter, r *http.Request) {
	t, err := a.st.ActiveTimer(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"timer": t})
}

func (a *API) startTimer(w http.ResponseWriter, r *http.Request) {
	var t store.Timer
	var stopped *domain.TimeEntry
	err := a.st.InTx(r.Context(), func(tx *store.Store) error {
		var err error
		t, stopped, err = tx.StartTimer(r.Context(), r.PathValue("id"), "me")
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"timer": t, "stopped": stopped})
}

func (a *API) stopTimer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Note string `json:"note"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	e, err := a.st.StopTimer(r.Context(), req.Note)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": e})
}

func (a *API) discardTimer(w http.ResponseWriter, r *http.Request) {
	if err := a.st.DiscardTimer(r.Context()); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) settings(w http.ResponseWriter, r *http.Request) {
	s, err := a.st.Settings(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *API) saveSettings(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := decode(r, &raw); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s, err := a.st.SaveSettings(r.Context(), raw)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	to := domain.Today().AddDays(1)
	from := to.AddDays(-30)
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
	v, err := a.st.Health(r.Context(), from, to)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// putHealth — показатели дня: скрипт синхронизации Garmin или ввод руками в дневнике.
func (a *API) putHealth(w http.ResponseWriter, r *http.Request) {
	d, err := dateParam(r.PathValue("date"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var h store.HealthDay
	if err := decode(r, &h); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	h.Date = d
	out, err := a.st.PutHealth(r.Context(), h)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) review(w http.ResponseWriter, r *http.Request) {
	v, err := a.st.Review(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) split(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Titles []string `json:"titles"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var out domain.Item
	err := a.st.InTx(r.Context(), func(tx *store.Store) error {
		var err error
		out, err = tx.Split(r.Context(), r.PathValue("id"), req.Titles, "me")
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// counts — бейджи меню одним запросом: входящие, разбор и таймер.
func (a *API) counts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	inbox, err := a.st.OpenInbox(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	review, err := a.st.ReviewCount(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	t, err := a.st.ActiveTimer(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"inbox": len(inbox), "review": review, "timer": t})
}
