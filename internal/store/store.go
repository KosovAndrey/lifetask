// Package store — доступ к Postgres. Любое изменение задачи идёт через
// UpdateItem/CreateItem, чтобы журнал item_events и счётчик переносов
// не зависели от того, кто менял: веб, бот, Claude или синхронизация.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

var ErrNotFound = errors.New("не найдено")

type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct {
	pool *pgxpool.Pool
	db   DBTX
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool, db: pool} }

// Raw — прямой доступ для пакетов с собственными таблицами (change_plans, briefs).
func (s *Store) Raw() DBTX { return s.db }

// InTx выполняет fn в транзакции; Store внутри fn пишет в неё.
func (s *Store) InTx(ctx context.Context, fn func(*Store) error) error {
	if s.pool == nil {
		return fn(s) // уже внутри транзакции
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(&Store{db: tx})
	})
}

const itemCols = `i.id, i.kind, i.title, i.body, i.props, i.sphere_id, i.project_id, i.parent_id,
	i.status, i.important, i.urgent, i.planned_date, i.start_at, i.end_at, i.deadline,
	i.estimate_min, i.weight, i.postpone_count, i.recurrence_id, i.occurrence_date, i.source,
	i.created_at, i.updated_at, i.done_at,
	COALESCE((SELECT sum(minutes) FROM time_entries t WHERE t.item_id = i.id), 0),
	COALESCE((SELECT array_agg(g.name ORDER BY g.name) FROM item_tags it JOIN tags g ON g.id = it.tag_id WHERE it.item_id = i.id), '{}'),
	` + progressCol

// progressCol — прогресс по весам: подзадачи и (для целей) задачи с part_of.
// NULL, если частей нет — тогда прогресс не показывается вовсе.
const progressCol = `(SELECT (COALESCE(sum(c.weight) FILTER (WHERE c.status = 'done'), 0) / NULLIF(sum(c.weight), 0))::float8
		FROM items c WHERE c.status <> 'cancelled'
		AND (c.parent_id = i.id OR c.id IN (SELECT from_id FROM relations WHERE to_id = i.id AND type = 'part_of')))`

func scanItem(row pgx.Row) (domain.Item, error) {
	var it domain.Item
	var planned, occ *time.Time
	err := row.Scan(&it.ID, &it.Kind, &it.Title, &it.Body, &it.Props, &it.SphereID, &it.ProjectID, &it.ParentID,
		&it.Status, &it.Important, &it.Urgent, &planned, &it.StartAt, &it.EndAt, &it.Deadline,
		&it.EstimateMin, &it.Weight, &it.PostponeCount, &it.RecurrenceID, &occ, &it.Source,
		&it.CreatedAt, &it.UpdatedAt, &it.DoneAt, &it.SpentMin, &it.Tags, &it.Progress)
	if errors.Is(err, pgx.ErrNoRows) {
		return it, ErrNotFound
	}
	if err != nil {
		return it, err
	}
	it.PlannedDate = toDate(planned)
	it.OccurrenceDate = toDate(occ)
	it.QuadrantN = it.Quadrant()
	if it.Body == nil {
		it.Body = []domain.Block{}
	}
	return it, nil
}

func toDate(t *time.Time) *domain.Date {
	if t == nil {
		return nil
	}
	y, m, d := t.Date()
	return &domain.Date{Time: time.Date(y, m, d, 0, 0, 0, 0, domain.MSK)}
}

func dateArg(d *domain.Date) any {
	if d == nil {
		return nil
	}
	return d.String()
}

func (s *Store) queryItems(ctx context.Context, where string, args ...any) ([]domain.Item, error) {
	rows, err := s.db.Query(ctx, `SELECT `+itemCols+` FROM items i WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Item{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) GetItem(ctx context.Context, id string) (domain.Item, error) {
	return scanItem(s.db.QueryRow(ctx, `SELECT `+itemCols+` FROM items i WHERE i.id = $1`, id))
}

// CreateItem проставляет дефолты, валидирует и пишет задачу.
func (s *Store) CreateItem(ctx context.Context, it domain.Item, actor string) (domain.Item, error) {
	if it.Kind == "" {
		it.Kind = domain.KindTask
	}
	if it.Status == "" {
		it.Status = domain.StatusTodo
	}
	if it.Weight == 0 {
		it.Weight = 1
	}
	if it.Source == "" {
		it.Source = "web"
	}
	if it.Body == nil {
		it.Body = []domain.Block{}
	}
	if it.Props == nil {
		it.Props = map[string]any{}
	}
	if err := it.Validate(); err != nil {
		return it, err
	}
	var doneAt *time.Time
	if it.Status == domain.StatusDone {
		now := time.Now()
		doneAt = &now
	}
	var id string
	err := s.db.QueryRow(ctx, `INSERT INTO items (kind, title, body, props, sphere_id, project_id, parent_id,
		status, important, urgent, planned_date, start_at, end_at, deadline, estimate_min, weight,
		recurrence_id, occurrence_date, source, done_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20) RETURNING id`,
		it.Kind, it.Title, it.Body, it.Props, it.SphereID, it.ProjectID, it.ParentID,
		it.Status, it.Important, it.Urgent, dateArg(it.PlannedDate), it.StartAt, it.EndAt, it.Deadline,
		it.EstimateMin, it.Weight, it.RecurrenceID, dateArg(it.OccurrenceDate), it.Source, doneAt).Scan(&id)
	if err != nil {
		return it, err
	}
	if len(it.Tags) > 0 {
		if err := s.SetTags(ctx, id, it.Tags); err != nil {
			return it, err
		}
	}
	if err := s.logEvent(ctx, id, actor, "created", nil, it.Title); err != nil {
		return it, err
	}
	return s.GetItem(ctx, id)
}

// Patch — частичное изменение: ключ = имя поля, значение = новое (null очищает).
type Patch map[string]json.RawMessage

// patchable — поля, которые можно менять патчем, и их колонки.
var patchable = map[string]string{
	"title": "title", "body": "body", "props": "props", "kind": "kind",
	"sphere_id": "sphere_id", "project_id": "project_id", "parent_id": "parent_id",
	"status": "status", "important": "important", "urgent": "urgent",
	"planned_date": "planned_date", "start_at": "start_at", "end_at": "end_at", "deadline": "deadline",
	"estimate_min": "estimate_min", "weight": "weight",
}

// UpdateItem применяет патч: декодирует его поверх текущей задачи (валидация
// целиком), пишет в БД, журналирует каждое изменённое поле и считает переносы.
func (s *Store) UpdateItem(ctx context.Context, id string, p Patch, actor string) (domain.Item, error) {
	cur, err := s.getForUpdate(ctx, id)
	if err != nil {
		return cur, err
	}
	// Глубокая копия: у Item поля-указатели, и декодирование патча поверх
	// поверхностной копии меняло бы заодно и cur (ломая подсчёт переносов и журнал).
	next, err := clone(cur)
	if err != nil {
		return cur, err
	}
	var tags []string
	var setTags bool
	for k, raw := range p {
		if k == "tags" {
			if err := json.Unmarshal(raw, &tags); err != nil {
				return cur, fmt.Errorf("tags: %w", err)
			}
			setTags = true
			continue
		}
		if _, ok := patchable[k]; !ok {
			return cur, fmt.Errorf("поле %q нельзя менять", k)
		}
	}
	// Декодируем патч поверх копии: json сам разберёт типы и null.
	buf, _ := json.Marshal(map[string]json.RawMessage(withoutKey(p, "tags")))
	if err := decodeOnto(&next, buf); err != nil {
		return cur, err
	}
	if err := next.Validate(); err != nil {
		return cur, err
	}

	// Перенос: разовая задача с плановой датой ушла на более позднюю дату.
	if cur.RecurrenceID == nil && cur.PlannedDate != nil && next.PlannedDate != nil &&
		next.PlannedDate.After(cur.PlannedDate.Time) && !cur.Status.Closed() {
		next.PostponeCount++
	}
	if next.Status == domain.StatusDone && cur.Status != domain.StatusDone {
		now := time.Now()
		next.DoneAt = &now
	} else if next.Status != domain.StatusDone {
		next.DoneAt = nil
	}

	_, err = s.db.Exec(ctx, `UPDATE items SET kind=$2, title=$3, body=$4, props=$5, sphere_id=$6, project_id=$7,
		parent_id=$8, status=$9, important=$10, urgent=$11, planned_date=$12, start_at=$13, end_at=$14,
		deadline=$15, estimate_min=$16, weight=$17, postpone_count=$18, done_at=$19, updated_at=now()
		WHERE id=$1`,
		id, next.Kind, next.Title, next.Body, next.Props, next.SphereID, next.ProjectID, next.ParentID,
		next.Status, next.Important, next.Urgent, dateArg(next.PlannedDate), next.StartAt, next.EndAt,
		next.Deadline, next.EstimateMin, next.Weight, next.PostponeCount, next.DoneAt)
	if err != nil {
		return cur, err
	}
	for k := range p {
		if k == "tags" {
			continue
		}
		oldV, newV := fieldJSON(cur, k), fieldJSON(next, k)
		if string(oldV) != string(newV) {
			if err := s.logEvent(ctx, id, actor, k, oldV, newV); err != nil {
				return cur, err
			}
		}
	}
	if setTags {
		if err := s.SetTags(ctx, id, tags); err != nil {
			return cur, err
		}
	}
	return s.GetItem(ctx, id)
}

func (s *Store) getForUpdate(ctx context.Context, id string) (domain.Item, error) {
	if _, err := s.db.Exec(ctx, `SELECT 1 FROM items WHERE id=$1 FOR UPDATE`, id); err != nil {
		return domain.Item{}, err
	}
	return s.GetItem(ctx, id)
}

func clone(it domain.Item) (domain.Item, error) {
	var out domain.Item
	b, err := json.Marshal(it)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}

func withoutKey(p Patch, key string) Patch {
	out := make(Patch, len(p))
	for k, v := range p {
		if k != key {
			out[k] = v
		}
	}
	return out
}

// decodeOnto кладёт JSON поверх существующих значений структуры. Явный null
// в json.Unmarshal для указателей обнуляет поле — то, что нужно для «очистить».
func decodeOnto(it *domain.Item, buf []byte) error {
	dec := json.NewDecoder(strings.NewReader(string(buf)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(it); err != nil {
		return fmt.Errorf("патч: %w", err)
	}
	return nil
}

func fieldJSON(it domain.Item, field string) json.RawMessage {
	var m map[string]json.RawMessage
	b, _ := json.Marshal(it)
	_ = json.Unmarshal(b, &m)
	if v, ok := m[field]; ok {
		return v
	}
	return json.RawMessage("null")
}

func (s *Store) DeleteItem(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM items WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) logEvent(ctx context.Context, itemID, actor, field string, old, new any) error {
	_, err := s.db.Exec(ctx, `INSERT INTO item_events (item_id, actor, field, old, new) VALUES ($1,$2,$3,$4,$5)`,
		itemID, actor, field, jsonArg(old), jsonArg(new))
	return err
}

func jsonArg(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case json.RawMessage:
		if string(x) == "null" {
			return nil
		}
		return string(x)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func (s *Store) SetTags(ctx context.Context, itemID string, names []string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM item_tags WHERE item_id=$1`, itemID); err != nil {
		return err
	}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		kind := "label"
		if strings.HasPrefix(n, "@") || strings.HasPrefix(n, "≤") {
			kind = "context"
		}
		_, err := s.db.Exec(ctx, `WITH t AS (
			INSERT INTO tags (name, kind) VALUES ($2, $3) ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name RETURNING id)
			INSERT INTO item_tags (item_id, tag_id) SELECT $1, id FROM t ON CONFLICT DO NOTHING`, itemID, n, kind)
		if err != nil {
			return err
		}
	}
	return nil
}

// ── Выборки ──────────────────────────────────────────────────────────────────

type Filter struct {
	Status   []string
	Kind     string
	SphereID int
	Query    string
	Open     bool // только незакрытые
	DoneDays int  // вместе со статусами — ещё и закрытые за последние N дней (канбан)
	Limit    int
}

func (s *Store) ListItems(ctx context.Context, f Filter) ([]domain.Item, error) {
	where := []string{"true"}
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if len(f.Status) > 0 && f.DoneDays > 0 {
		args = append(args, f.Status, f.DoneDays)
		where = append(where, fmt.Sprintf("(i.status = ANY($%d) OR i.status = 'done' AND i.done_at > now() - make_interval(days => $%d))",
			len(args)-1, len(args)))
	} else if len(f.Status) > 0 {
		add("i.status = ANY($%d)", f.Status)
	}
	if f.Open {
		where = append(where, "i.status NOT IN ('done','cancelled')")
	}
	if f.Kind != "" {
		add("i.kind = $%d", f.Kind)
	}
	if f.SphereID != 0 {
		add("i.sphere_id = $%d", f.SphereID)
	}
	if f.Query != "" {
		add("i.title ILIKE '%%' || $%d || '%%'", f.Query)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	return s.queryItems(ctx, strings.Join(where, " AND ")+
		fmt.Sprintf(" ORDER BY i.planned_date NULLS LAST, i.start_at NULLS LAST, i.created_at LIMIT %d", limit), args...)
}

// Day — всё, что относится к дню: встречи и блоки со временем, задачи на день,
// дедлайны дня и просроченное (незакрытое с плановой датой раньше).
type Day struct {
	Date      domain.Date   `json:"date"`
	Scheduled []domain.Item `json:"scheduled"`
	Planned   []domain.Item `json:"planned"`
	Deadlines []domain.Item `json:"deadlines"`
	Overdue   []domain.Item `json:"overdue"`
	BusyMin   int           `json:"busy_min"`
	PlanMin   int           `json:"plan_min"`
}

func (s *Store) Day(ctx context.Context, d domain.Date) (Day, error) {
	from := d.Time
	to := d.AddDays(1).Time
	day := Day{Date: d}
	var err error
	if day.Scheduled, err = s.queryItems(ctx,
		`i.start_at >= $1 AND i.start_at < $2 AND i.status <> 'cancelled' ORDER BY i.start_at`, from, to); err != nil {
		return day, err
	}
	if day.Planned, err = s.queryItems(ctx,
		`i.planned_date = $1 AND i.start_at IS NULL AND i.kind <> 'note' AND i.status <> 'cancelled'
		 ORDER BY (i.status = 'done'), NOT i.important, NOT i.urgent, i.created_at`, d.String()); err != nil {
		return day, err
	}
	if day.Deadlines, err = s.queryItems(ctx,
		`i.deadline >= $1 AND i.deadline < $2 AND i.status NOT IN ('done','cancelled') ORDER BY i.deadline`, from, to); err != nil {
		return day, err
	}
	if !d.After(domain.Today().Time) {
		if day.Overdue, err = s.queryItems(ctx,
			`i.planned_date < $1 AND i.status NOT IN ('done','cancelled','someday') AND i.kind <> 'note'
			 ORDER BY i.planned_date`, d.String()); err != nil {
			return day, err
		}
	} else {
		day.Overdue = []domain.Item{}
	}
	for _, it := range day.Scheduled {
		if it.EndAt != nil {
			day.BusyMin += int(it.EndAt.Sub(*it.StartAt).Minutes())
		}
	}
	for _, it := range day.Planned {
		if it.EstimateMin != nil && !it.Status.Closed() {
			day.PlanMin += *it.EstimateMin
		}
	}
	return day, nil
}

func (s *Store) Detail(ctx context.Context, id string) (domain.ItemDetail, error) {
	var d domain.ItemDetail
	it, err := s.GetItem(ctx, id)
	if err != nil {
		return d, err
	}
	d.Item = it
	if d.Children, err = s.queryItems(ctx, `i.parent_id = $1 ORDER BY i.created_at`, id); err != nil {
		return d, err
	}
	if d.Relations, err = s.Relations(ctx, id); err != nil {
		return d, err
	}
	if d.Related, err = s.RelatedItems(ctx, d.Relations, id); err != nil {
		return d, err
	}
	if d.Time, err = s.TimeEntries(ctx, id); err != nil {
		return d, err
	}
	return d, nil
}

// ── Связи и время ────────────────────────────────────────────────────────────

func (s *Store) Relate(ctx context.Context, r domain.Relation) error {
	if !r.Type.Valid() {
		return fmt.Errorf("неизвестный тип связи %q", r.Type)
	}
	if r.Type == domain.RelBlocks || r.Type == domain.RelFollows {
		// Цикл в зависимостях сделает задачи невыполнимыми — не пускаем.
		var cyc bool
		err := s.db.QueryRow(ctx, `WITH RECURSIVE up(id) AS (
				SELECT $1::uuid
				UNION SELECT r.from_id FROM relations r JOIN up ON r.to_id = up.id WHERE r.type IN ('blocks','follows'))
			SELECT EXISTS (SELECT 1 FROM up WHERE id = $2)`, r.FromID, r.ToID).Scan(&cyc)
		if err != nil {
			return err
		}
		if cyc {
			return errors.New("связь создаёт цикл зависимостей")
		}
	}
	_, err := s.db.Exec(ctx, `INSERT INTO relations (from_id, to_id, type) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
		r.FromID, r.ToID, r.Type)
	return err
}

func (s *Store) Unrelate(ctx context.Context, r domain.Relation) error {
	_, err := s.db.Exec(ctx, `DELETE FROM relations WHERE from_id=$1 AND to_id=$2 AND type=$3`, r.FromID, r.ToID, r.Type)
	return err
}

func (s *Store) Relations(ctx context.Context, id string) ([]domain.Relation, error) {
	rows, err := s.db.Query(ctx, `SELECT from_id, to_id, type FROM relations WHERE from_id=$1 OR to_id=$1`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Relation, error) {
		var x domain.Relation
		err := r.Scan(&x.FromID, &x.ToID, &x.Type)
		return x, err
	})
}

func (s *Store) LogTime(ctx context.Context, e domain.TimeEntry) (domain.TimeEntry, error) {
	if e.Minutes <= 0 {
		return e, errors.New("minutes должно быть > 0")
	}
	if e.Source == "" {
		e.Source = "web"
	}
	err := s.db.QueryRow(ctx, `INSERT INTO time_entries (item_id, started_at, minutes, source, note)
		VALUES ($1,$2,$3,$4,$5) RETURNING id, created_at`,
		e.ItemID, e.StartedAt, e.Minutes, e.Source, e.Note).Scan(&e.ID, &e.CreatedAt)
	return e, err
}

func (s *Store) TimeEntries(ctx context.Context, itemID string) ([]domain.TimeEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT id, item_id, started_at, minutes, source, note, created_at
		FROM time_entries WHERE item_id=$1 ORDER BY created_at`, itemID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.TimeEntry, error) {
		var e domain.TimeEntry
		err := r.Scan(&e.ID, &e.ItemID, &e.StartedAt, &e.Minutes, &e.Source, &e.Note, &e.CreatedAt)
		return e, err
	})
}

// ── Справочники ──────────────────────────────────────────────────────────────

func (s *Store) Spheres(ctx context.Context) ([]domain.Sphere, error) {
	rows, err := s.db.Query(ctx, `SELECT id, slug, name, color, icon, style FROM spheres WHERE NOT archived ORDER BY sort, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Sphere, error) {
		var x domain.Sphere
		err := r.Scan(&x.ID, &x.Slug, &x.Name, &x.Color, &x.Icon, &x.Style)
		return x, err
	})
}

// SphereBySlug — для ИИ удобнее ссылаться на сферу словом, а не числом.
func (s *Store) SphereBySlug(ctx context.Context, slug string) (int, error) {
	var id int
	err := s.db.QueryRow(ctx, `SELECT id FROM spheres WHERE slug=$1`, slug).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("сфера %q: %w", slug, ErrNotFound)
	}
	return id, err
}

func (s *Store) Projects(ctx context.Context) ([]domain.Project, error) {
	rows, err := s.db.Query(ctx, `SELECT id, parent_id, sphere_id, name, color FROM projects WHERE NOT archived ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Project, error) {
		var x domain.Project
		err := r.Scan(&x.ID, &x.ParentID, &x.SphereID, &x.Name, &x.Color)
		return x, err
	})
}

func (s *Store) CreateProject(ctx context.Context, p domain.Project) (domain.Project, error) {
	if strings.TrimSpace(p.Name) == "" {
		return p, errors.New("пустое имя проекта")
	}
	err := s.db.QueryRow(ctx, `INSERT INTO projects (parent_id, sphere_id, name, color) VALUES ($1,$2,$3,$4) RETURNING id`,
		p.ParentID, p.SphereID, p.Name, p.Color).Scan(&p.ID)
	return p, err
}
