// Package domain — типы ядра. Жёсткие ограничения только на поля, от которых
// зависит логика (статус, даты, матрица, связи); body/props — свободные.
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MSK — единственный часовой пояс планировщика: «день» всегда считается по Москве.
var MSK = mustLoad("Europe/Moscow")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

type Kind string

const (
	KindTask  Kind = "task"
	KindGoal  Kind = "goal"
	KindEvent Kind = "event"
	KindNote  Kind = "note"
)

type Status string

const (
	StatusInbox     Status = "inbox"
	StatusTodo      Status = "todo"
	StatusDoing     Status = "doing"
	StatusWaiting   Status = "waiting"
	StatusDone      Status = "done"
	StatusCancelled Status = "cancelled"
	StatusSomeday   Status = "someday"
)

func (s Status) Closed() bool { return s == StatusDone || s == StatusCancelled }

type RelationType string

const (
	RelBlocks   RelationType = "blocks"
	RelRelated  RelationType = "related"
	RelFollows  RelationType = "follows"
	RelPartOf   RelationType = "part_of"
	RelPrepares RelationType = "prepares"
)

var (
	kinds     = set(KindTask, KindGoal, KindEvent, KindNote)
	statuses  = set(StatusInbox, StatusTodo, StatusDoing, StatusWaiting, StatusDone, StatusCancelled, StatusSomeday)
	relations = set(RelBlocks, RelRelated, RelFollows, RelPartOf, RelPrepares)
)

func set[T comparable](vs ...T) map[T]bool {
	m := make(map[T]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

func (k Kind) Valid() bool         { return kinds[k] }
func (s Status) Valid() bool       { return statuses[s] }
func (r RelationType) Valid() bool { return relations[r] }

// Date — календарная дата без времени (YYYY-MM-DD в JSON).
type Date struct{ time.Time }

func ParseDate(s string) (Date, error) {
	t, err := time.ParseInLocation(time.DateOnly, s, MSK)
	return Date{t}, err
}

func Today() Date {
	y, m, d := time.Now().In(MSK).Date()
	return Date{time.Date(y, m, d, 0, 0, 0, 0, MSK)}
}

func (d Date) String() string { return d.Format(time.DateOnly) }
func (d Date) AddDays(n int) Date {
	return Date{d.AddDate(0, 0, n)}
}

func (d Date) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }
func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	p, err := ParseDate(s)
	if err != nil {
		return fmt.Errorf("дата %q: ждём YYYY-MM-DD", s)
	}
	*d = p
	return nil
}

// Block — единица оформления body. Тип обязателен, остальное — по типу.
type Block map[string]any

// KnownBlocks — типы, которые веб рендерит своим компонентом. Неизвестный тип
// допустим (покажется как markdown), но без поля type блок отвергается.
var KnownBlocks = set("md", "checklist", "link", "file", "ref", "callout", "code", "table", "comment")

func ValidateBody(body []Block) error {
	for i, b := range body {
		t, _ := b["type"].(string)
		if t == "" {
			return fmt.Errorf("body[%d]: нет поля type", i)
		}
	}
	return nil
}

type Item struct {
	ID        string         `json:"id"`
	Kind      Kind           `json:"kind"`
	Title     string         `json:"title"`
	Body      []Block        `json:"body"`
	Props     map[string]any `json:"props"`
	SphereID  *int           `json:"sphere_id,omitempty"`
	ProjectID *int           `json:"project_id,omitempty"`
	ParentID  *string        `json:"parent_id,omitempty"`
	Status    Status         `json:"status"`
	Important bool           `json:"important"`
	Urgent    bool           `json:"urgent"`

	PlannedDate   *Date      `json:"planned_date,omitempty"`
	StartAt       *time.Time `json:"start_at,omitempty"`
	EndAt         *time.Time `json:"end_at,omitempty"`
	Deadline      *time.Time `json:"deadline,omitempty"`
	EstimateMin   *int       `json:"estimate_min,omitempty"`
	Weight        float64    `json:"weight"`
	PostponeCount int        `json:"postpone_count"`

	RecurrenceID   *string `json:"recurrence_id,omitempty"`
	OccurrenceDate *Date   `json:"occurrence_date,omitempty"`

	Source    string     `json:"source"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DoneAt    *time.Time `json:"done_at,omitempty"`

	// Вычисляемые поля для чтения (заполняет store).
	QuadrantN int      `json:"quadrant"`
	SpentMin  int      `json:"spent_min"`
	Progress  *float64 `json:"progress,omitempty"` // доля веса сделанных частей; nil — частей нет
	Tags      []string `json:"tags,omitempty"`
}

// Quadrant — квадрант матрицы Эйзенхауэра: 1 важно+срочно, 2 важно, 3 срочно, 4 ни то ни другое.
func (it Item) Quadrant() int {
	switch {
	case it.Important && it.Urgent:
		return 1
	case it.Important:
		return 2
	case it.Urgent:
		return 3
	}
	return 4
}

func (it *Item) Validate() error {
	if it.Title == "" {
		return errors.New("пустой title")
	}
	if !it.Kind.Valid() {
		return fmt.Errorf("неизвестный kind %q", it.Kind)
	}
	if !it.Status.Valid() {
		return fmt.Errorf("неизвестный status %q", it.Status)
	}
	if it.EndAt != nil && it.StartAt == nil {
		return errors.New("end_at без start_at")
	}
	if it.EndAt != nil && it.EndAt.Before(*it.StartAt) {
		return errors.New("end_at раньше start_at")
	}
	if it.EstimateMin != nil && *it.EstimateMin < 0 {
		return errors.New("отрицательная оценка")
	}
	if it.Weight < 0 {
		return errors.New("отрицательный вес")
	}
	return ValidateBody(it.Body)
}

type Sphere struct {
	ID       int            `json:"id"`
	Slug     string         `json:"slug"`
	Name     string         `json:"name"`
	Color    string         `json:"color"`
	Icon     string         `json:"icon"`
	Style    map[string]any `json:"style"` // color_dark, gcal_color, hint
	Sort     int            `json:"sort"`
	Archived bool           `json:"archived,omitempty"`
}

type Project struct {
	ID       int    `json:"id"`
	ParentID *int   `json:"parent_id,omitempty"`
	SphereID *int   `json:"sphere_id,omitempty"`
	Name     string `json:"name"`
	Color    string `json:"color"`
}

type Relation struct {
	FromID string       `json:"from_id"`
	ToID   string       `json:"to_id"`
	Type   RelationType `json:"type"`
}

type TimeEntry struct {
	ID        string     `json:"id"`
	ItemID    string     `json:"item_id"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	Minutes   int        `json:"minutes"`
	Source    string     `json:"source"`
	Note      string     `json:"note"`
	CreatedAt time.Time  `json:"created_at"`
}

type InboxMessage struct {
	ID           string          `json:"id"`
	Text         string          `json:"text"`
	Transcript   *string         `json:"transcript,omitempty"`
	Parsed       json.RawMessage `json:"parsed,omitempty"`
	ParseError   *string         `json:"parse_error,omitempty"`
	Status       string          `json:"status"`
	ItemID       *string         `json:"item_id,omitempty"`
	TgMessageID  *int64          `json:"tg_message_id,omitempty"`
	BotMessageID *int64          `json:"bot_message_id,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	Files        []Block         `json:"files,omitempty"` // вложения: переедут в задачу при принятии
}

// Content — что пришло: текст или расшифровка голоса.
func (m InboxMessage) Content() string {
	if m.Transcript != nil && *m.Transcript != "" {
		return *m.Transcript
	}
	return m.Text
}

// ItemDetail — задача со всем окружением, для карточки и для Claude.
type ItemDetail struct {
	Item
	Children  []Item      `json:"children"`
	Relations []Relation  `json:"relations"`
	Related   []Item      `json:"related"` // задачи на другом конце связей
	Time      []TimeEntry `json:"time"`
}
