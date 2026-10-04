// Package changeplan — изменения от ИИ (Claude на разборе, бот) идут не напрямую,
// а планом: список операций. Propose прогоняет план в транзакции с откатом
// (dry-run) — так проверяется всё сразу и строится человекочитаемое превью;
// Apply применяет тот же план атомарно после подтверждения.
package changeplan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
)

// Op — одна операция плана. Ссылки на создаваемые в этом же плане сущности
// пишутся как "$<ref>" (например, задача-подготовка → "$call").
type Op struct {
	Op   string                     `json:"op"` // create | update | done | delete | relate | unrelate | log_time | inbox | project
	Ref  string                     `json:"ref,omitempty"`
	ID   string                     `json:"id,omitempty"`
	Item map[string]json.RawMessage `json:"item,omitempty"` // create
	Set  map[string]json.RawMessage `json:"set,omitempty"`  // update

	From string `json:"from,omitempty"` // relate
	To   string `json:"to,omitempty"`
	Type string `json:"type,omitempty"`

	Minutes   int        `json:"minutes,omitempty"` // log_time
	StartedAt *time.Time `json:"started_at,omitempty"`
	Note      string     `json:"note,omitempty"`

	Status string `json:"status,omitempty"` // inbox

	Name   string `json:"name,omitempty"` // project
	Parent string `json:"parent,omitempty"`
	Sphere string `json:"sphere,omitempty"`

	Rule        string `json:"rule,omitempty"`  // recur: RRULE-подмножество, см. internal/recur
	Start       string `json:"start,omitempty"` // recur: дата первого повтора; recur_stop: с какой даты остановить
	Until       string `json:"until,omitempty"`
	Time        string `json:"time,omitempty"` // HH:MM — повтор со временем (встреча, тренировка)
	DurationMin int    `json:"duration_min,omitempty"`
}

type Plan struct {
	ID        string            `json:"id"`
	Author    string            `json:"author"`
	Summary   string            `json:"summary"`
	Ops       []Op              `json:"ops"`
	Status    string            `json:"status"`
	Preview   []string          `json:"preview"`
	Refs      map[string]string `json:"refs,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	AppliedAt *time.Time        `json:"applied_at,omitempty"`
}

type Service struct {
	st *store.Store
}

var errDryRun = errors.New("dry run")

func New(st *store.Store) *Service { return &Service{st: st} }

// Propose проверяет план прогоном с откатом и сохраняет его вместе с превью.
func (s *Service) Propose(ctx context.Context, author, summary string, ops []Op) (Plan, error) {
	if len(ops) == 0 {
		return Plan{}, errors.New("пустой план")
	}
	var preview []string
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		var err error
		preview, _, err = run(ctx, tx, ops, author)
		if err != nil {
			return err
		}
		return errDryRun
	})
	if !errors.Is(err, errDryRun) {
		return Plan{}, err
	}
	p := Plan{Author: author, Summary: summary, Ops: ops, Status: "proposed", Preview: preview}
	err = s.st.Raw().QueryRow(ctx, `INSERT INTO change_plans (author, summary, ops, result)
		VALUES ($1,$2,$3,$4) RETURNING id, created_at`,
		author, summary, ops, map[string]any{"preview": preview}).Scan(&p.ID, &p.CreatedAt)
	return p, err
}

// Preview — dry-run без сохранения плана: проверка и человекочитаемые строки.
func (s *Service) Preview(ctx context.Context, author string, ops []Op) ([]string, error) {
	var preview []string
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		var err error
		if preview, _, err = run(ctx, tx, ops, author); err != nil {
			return err
		}
		return errDryRun
	})
	if !errors.Is(err, errDryRun) {
		return nil, err
	}
	return preview, nil
}

// ApplyNow — предложить и сразу применить: когда подтверждение уже получено
// (кнопка «Добавить» в боте). План всё равно сохраняется — для истории.
func (s *Service) ApplyNow(ctx context.Context, author, summary string, ops []Op) (Plan, error) {
	p, err := s.Propose(ctx, author, summary, ops)
	if err != nil {
		return p, err
	}
	return s.Apply(ctx, p.ID)
}

func (s *Service) Get(ctx context.Context, id string) (Plan, error) {
	var p Plan
	var result struct {
		Preview []string          `json:"preview"`
		Refs    map[string]string `json:"refs"`
	}
	err := s.st.Raw().QueryRow(ctx, `SELECT id, author, summary, ops, status, result, created_at, applied_at
		FROM change_plans WHERE id=$1`, id).Scan(&p.ID, &p.Author, &p.Summary, &p.Ops, &p.Status, &result, &p.CreatedAt, &p.AppliedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, store.ErrNotFound
	}
	p.Preview, p.Refs = result.Preview, result.Refs
	return p, err
}

// Apply применяет ранее предложенный план. Данные могли измениться с момента
// Propose — тогда план упадёт целиком и ничего не применится.
func (s *Service) Apply(ctx context.Context, id string) (Plan, error) {
	p, err := s.Get(ctx, id)
	if err != nil {
		return p, err
	}
	if p.Status != "proposed" {
		return p, fmt.Errorf("план уже %s", p.Status)
	}
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		lines, refs, err := run(ctx, tx, p.Ops, p.Author)
		if err != nil {
			return err
		}
		p.Preview, p.Refs = lines, refs
		_, err = tx.Raw().Exec(ctx, `UPDATE change_plans SET status='applied', applied_at=now(), result=$2 WHERE id=$1`,
			id, map[string]any{"preview": lines, "refs": refs})
		return err
	})
	if err != nil {
		return p, err
	}
	return s.Get(ctx, id)
}

func (s *Service) Reject(ctx context.Context, id string) error {
	tag, err := s.st.Raw().Exec(ctx, `UPDATE change_plans SET status='rejected' WHERE id=$1 AND status='proposed'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("план не найден или уже не в статусе proposed")
	}
	return nil
}

// ── Исполнение ───────────────────────────────────────────────────────────────

type runner struct {
	ctx      context.Context
	st       *store.Store
	actor    string
	refs     map[string]string // $ref → uuid задачи
	projRefs map[string]int    // $ref → id проекта
	lines    []string
	spheres  map[int]string // id → «иконка название», лениво
}

func run(ctx context.Context, st *store.Store, ops []Op, actor string) ([]string, map[string]string, error) {
	r := &runner{ctx: ctx, st: st, actor: actor, refs: map[string]string{}, projRefs: map[string]int{}}
	for i, op := range ops {
		if err := r.do(op); err != nil {
			return nil, nil, fmt.Errorf("операция %d (%s): %w", i+1, op.Op, err)
		}
	}
	return r.lines, r.refs, nil
}

func (r *runner) id(v string) (string, error) {
	if strings.HasPrefix(v, "$") {
		id, ok := r.refs[v[1:]]
		if !ok {
			return "", fmt.Errorf("ссылка %s не определена выше по плану", v)
		}
		return id, nil
	}
	if v == "" {
		return "", errors.New("не указан id")
	}
	return v, nil
}

func (r *runner) do(op Op) error {
	switch op.Op {
	case "create":
		fields, err := r.sugar(op.Item)
		if err != nil {
			return err
		}
		var it domain.Item
		if err := strictDecode(fields, &it); err != nil {
			return err
		}
		if it.Source == "" {
			it.Source = "claude"
		}
		created, err := r.st.CreateItem(r.ctx, it, r.actor)
		if err != nil {
			return err
		}
		if op.Ref != "" {
			r.refs[op.Ref] = created.ID
		}
		r.add("+ %s", describe(created, r.sphereLabel(created.SphereID)))

	case "update":
		id, err := r.id(op.ID)
		if err != nil {
			return err
		}
		before, err := r.st.GetItem(r.ctx, id)
		if err != nil {
			return err
		}
		fields, err := r.sugar(op.Set)
		if err != nil {
			return err
		}
		after, err := r.st.UpdateItem(r.ctx, id, store.Patch(fields), r.actor)
		if err != nil {
			return err
		}
		r.add("~ «%s»: %s", before.Title, diff(before, after, fields))

	case "done":
		id, err := r.id(op.ID)
		if err != nil {
			return err
		}
		it, err := r.st.UpdateItem(r.ctx, id, store.Patch{"status": json.RawMessage(`"done"`)}, r.actor)
		if err != nil {
			return err
		}
		r.add("✓ «%s»", it.Title)

	case "delete":
		id, err := r.id(op.ID)
		if err != nil {
			return err
		}
		it, err := r.st.GetItem(r.ctx, id)
		if err != nil {
			return err
		}
		if err := r.st.DeleteItem(r.ctx, id); err != nil {
			return err
		}
		r.add("✗ удалить «%s»", it.Title)

	case "relate", "unrelate":
		from, err := r.id(op.From)
		if err != nil {
			return err
		}
		to, err := r.id(op.To)
		if err != nil {
			return err
		}
		rel := domain.Relation{FromID: from, ToID: to, Type: domain.RelationType(op.Type)}
		a, err := r.st.GetItem(r.ctx, from)
		if err != nil {
			return err
		}
		b, err := r.st.GetItem(r.ctx, to)
		if err != nil {
			return err
		}
		if op.Op == "relate" {
			err = r.st.Relate(r.ctx, rel)
			r.add("↔ «%s» —%s→ «%s»", a.Title, op.Type, b.Title)
		} else {
			err = r.st.Unrelate(r.ctx, rel)
			r.add("⌫ «%s» —%s→ «%s»", a.Title, op.Type, b.Title)
		}
		return err

	case "log_time":
		id, err := r.id(op.ID)
		if err != nil {
			return err
		}
		it, err := r.st.GetItem(r.ctx, id)
		if err != nil {
			return err
		}
		if _, err := r.st.LogTime(r.ctx, domain.TimeEntry{ItemID: id, Minutes: op.Minutes, StartedAt: op.StartedAt,
			Note: op.Note, Source: "claude"}); err != nil {
			return err
		}
		r.add("⏱ «%s» +%s", it.Title, fmtMin(op.Minutes))

	case "inbox":
		var itemID *string
		if op.To != "" {
			id, err := r.id(op.To)
			if err != nil {
				return err
			}
			itemID = &id
		}
		if err := r.st.ResolveInbox(r.ctx, op.ID, op.Status, itemID); err != nil {
			return err
		}
		r.add("📥 инбокс %s → %s", short(op.ID), op.Status)

	case "project":
		p := domain.Project{Name: op.Name}
		if op.Sphere != "" {
			sid, err := r.st.SphereBySlug(r.ctx, op.Sphere)
			if err != nil {
				return err
			}
			p.SphereID = &sid
		}
		if op.Parent != "" {
			pid, err := r.projectID(op.Parent)
			if err != nil {
				return err
			}
			p.ParentID = &pid
		}
		created, err := r.st.CreateProject(r.ctx, p)
		if err != nil {
			return err
		}
		if op.Ref != "" {
			r.projRefs[op.Ref] = created.ID
		}
		r.add("+ проект «%s»", created.Name)

	case "recur":
		fields, err := r.sugar(op.Item)
		if err != nil {
			return err
		}
		var tpl domain.Item
		if err := strictDecode(fields, &tpl); err != nil {
			return err
		}
		if tpl.Source == "" {
			tpl.Source = "claude"
		}
		rec := store.Recurrence{Rule: op.Rule, Template: tpl, Start: domain.Today()}
		if op.Start != "" {
			if rec.Start, err = domain.ParseDate(op.Start); err != nil {
				return fmt.Errorf("start: %w", err)
			}
		}
		if op.Until != "" {
			u, err := domain.ParseDate(op.Until)
			if err != nil {
				return fmt.Errorf("until: %w", err)
			}
			rec.Until = &u
		}
		if op.Time != "" {
			rec.StartTime = &op.Time
		}
		if op.DurationMin > 0 {
			rec.DurationMin = &op.DurationMin
		}
		created, err := r.st.CreateRecurrence(r.ctx, rec)
		if err != nil {
			return err
		}
		items, err := r.st.GenerateOne(r.ctx, created.ID, domain.Today().AddDays(store.RecurHorizonDays))
		if err != nil {
			return err
		}
		when := created.Human
		if op.Time != "" {
			when += " в " + op.Time
		}
		r.add("🔁 «%s» %s · ближайшие: %s", tpl.Title, when, store.HumanDates(items, 3))

	case "recur_stop":
		from := domain.Today()
		if op.Start != "" {
			var err error
			if from, err = domain.ParseDate(op.Start); err != nil {
				return fmt.Errorf("start: %w", err)
			}
		}
		rec, n, err := r.st.StopRecurrence(r.ctx, op.ID, from)
		if err != nil {
			return err
		}
		r.add("⏹ повтор «%s» остановлен с %s, убрано будущих: %d", rec.Template.Title, fmtDay(from.Time), n)

	default:
		return fmt.Errorf("неизвестная операция %q", op.Op)
	}
	return nil
}

// sugar переводит удобные для ИИ ключи в колонки: sphere (slug) → sphere_id,
// project ("$ref" | имя | число) → project_id, parent ("$ref" | uuid) → parent_id.
func (r *runner) sugar(in map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		out[k] = v
	}
	if raw, ok := out["sphere"]; ok {
		delete(out, "sphere")
		var slug *string
		if err := json.Unmarshal(raw, &slug); err != nil {
			return nil, fmt.Errorf("sphere: ждём slug строкой")
		}
		if slug == nil {
			out["sphere_id"] = json.RawMessage("null")
		} else {
			id, err := r.st.SphereBySlug(r.ctx, *slug)
			if err != nil {
				return nil, err
			}
			out["sphere_id"] = mustJSON(id)
		}
	}
	if raw, ok := out["project"]; ok {
		delete(out, "project")
		var v any
		_ = json.Unmarshal(raw, &v)
		switch x := v.(type) {
		case nil:
			out["project_id"] = json.RawMessage("null")
		case float64:
			out["project_id"] = mustJSON(int(x))
		case string:
			id, err := r.projectID(x)
			if err != nil {
				return nil, err
			}
			out["project_id"] = mustJSON(id)
		default:
			return nil, errors.New("project: ждём $ref, имя или id")
		}
	}
	if raw, ok := out["parent"]; ok {
		delete(out, "parent")
		var v *string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, errors.New("parent: ждём $ref или uuid")
		}
		if v == nil {
			out["parent_id"] = json.RawMessage("null")
		} else {
			id, err := r.id(*v)
			if err != nil {
				return nil, err
			}
			out["parent_id"] = mustJSON(id)
		}
	}
	return out, nil
}

func (r *runner) projectID(v string) (int, error) {
	if strings.HasPrefix(v, "$") {
		id, ok := r.projRefs[v[1:]]
		if !ok {
			return 0, fmt.Errorf("проект %s не определён выше по плану", v)
		}
		return id, nil
	}
	var id int
	err := r.st.Raw().QueryRow(r.ctx, `SELECT id FROM projects WHERE lower(name) = lower($1) AND NOT archived`, v).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("проект %q не найден", v)
	}
	return id, err
}

func (r *runner) add(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func strictDecode(fields map[string]json.RawMessage, it *domain.Item) error {
	buf, _ := json.Marshal(fields)
	dec := json.NewDecoder(strings.NewReader(string(buf)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(it); err != nil {
		return fmt.Errorf("item: %w", err)
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// ── Превью ───────────────────────────────────────────────────────────────────

var weekdays = [...]string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}

func fmtDay(t time.Time) string {
	t = t.In(domain.MSK)
	return fmt.Sprintf("%s %02d.%02d", weekdays[t.Weekday()], t.Day(), t.Month())
}

func fmtMin(m int) string {
	if m >= 60 {
		if m%60 == 0 {
			return fmt.Sprintf("%dч", m/60)
		}
		return fmt.Sprintf("%dч%02dм", m/60, m%60)
	}
	return fmt.Sprintf("%dм", m)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// describe — одна строка о задаче: вид, заголовок, когда, оценка, матрица.
// sphereLabel — «💪 Здоровье» для превью; сферы читаются один раз за план.
func (r *runner) sphereLabel(id *int) string {
	if id == nil {
		return ""
	}
	if r.spheres == nil {
		r.spheres = map[int]string{}
		ss, err := r.st.AllSpheres(r.ctx)
		if err != nil {
			return ""
		}
		for _, sp := range ss {
			r.spheres[sp.ID] = strings.TrimSpace(sp.Icon + " " + sp.Name)
		}
	}
	return r.spheres[*id]
}

func describe(it domain.Item, sphere string) string {
	parts := []string{}
	if it.Kind != domain.KindTask {
		parts = append(parts, string(it.Kind))
	}
	parts = append(parts, "«"+it.Title+"»")
	if sphere != "" {
		parts = append(parts, sphere)
	}
	switch {
	case it.StartAt != nil:
		w := fmtDay(*it.StartAt) + " " + it.StartAt.In(domain.MSK).Format("15:04")
		if it.EndAt != nil {
			w += "–" + it.EndAt.In(domain.MSK).Format("15:04")
		}
		parts = append(parts, w)
	case it.PlannedDate != nil:
		parts = append(parts, fmtDay(it.PlannedDate.Time))
	}
	if it.Deadline != nil {
		parts = append(parts, "дедлайн "+fmtDay(*it.Deadline))
	}
	if it.EstimateMin != nil {
		parts = append(parts, "~"+fmtMin(*it.EstimateMin))
	}
	parts = append(parts, fmt.Sprintf("Q%d", it.Quadrant()))
	if it.Status != domain.StatusTodo {
		parts = append(parts, string(it.Status))
	}
	if len(it.Tags) > 0 {
		parts = append(parts, strings.Join(it.Tags, " "))
	}
	return strings.Join(parts, " · ")
}

func diff(before, after domain.Item, fields map[string]json.RawMessage) string {
	var out []string
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	var bm, am map[string]json.RawMessage
	_ = json.Unmarshal(b, &bm)
	_ = json.Unmarshal(a, &am)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "body" || k == "props" {
			out = append(out, k+" обновлено")
			continue
		}
		ov, nv := string(bm[k]), string(am[k])
		if ov == "" {
			ov = "—"
		}
		if nv == "" {
			nv = "—"
		}
		if ov != nv {
			out = append(out, fmt.Sprintf("%s %s → %s", k, ov, nv))
		}
	}
	if after.PostponeCount > before.PostponeCount {
		out = append(out, fmt.Sprintf("перенос №%d", after.PostponeCount))
	}
	if len(out) == 0 {
		return "без изменений"
	}
	return strings.Join(out, "; ")
}
