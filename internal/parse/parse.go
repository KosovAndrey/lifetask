// Package parse — разбор короткой заметки с телефона («во вторник созвон с тбанком
// 16–17», «уборка 40м») в черновик задачи или запись времени. Модель возвращает
// JSON строго по схеме (structured outputs), дальше результат превращается в
// операции плана — те же, что у Claude на разборе.
package parse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/changeplan"
	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

type Intent string

const (
	IntentTask    Intent = "task"
	IntentEvent   Intent = "event"
	IntentNote    Intent = "note"
	IntentTimeLog Intent = "time_log"
	IntentUnclear Intent = "unclear"
)

// Result — ровно то, что возвращает модель (схема в schema.go).
type Result struct {
	Intent      Intent   `json:"intent"`
	Title       string   `json:"title"`
	Sphere      *string  `json:"sphere"`
	Important   bool     `json:"important"`
	Urgent      bool     `json:"urgent"`
	PlannedDate *string  `json:"planned_date"`
	StartAt     *string  `json:"start_at"`
	EndAt       *string  `json:"end_at"`
	Deadline    *string  `json:"deadline"`
	EstimateMin *int     `json:"estimate_min"`
	Tags        []string `json:"tags"`
	Details     *string  `json:"details"`
	Checklist   []string `json:"checklist"`
	RRule       *string  `json:"rrule"`
	TimeMinutes *int     `json:"time_minutes"`
	TimeItemID  *string  `json:"time_item_id"`
	Confident   bool     `json:"confident"`
	Question    *string  `json:"question"`
}

// Candidate — существующая задача, к которой может относиться запись времени.
type Candidate struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type Input struct {
	Text       string
	Now        time.Time
	Spheres    []domain.Sphere
	Candidates []Candidate
	Previous   *Result // при уточнении ответом на карточку
}

type Parser interface {
	Parse(ctx context.Context, in Input) (Result, error)
}

// Ops превращает разбор в операции плана: создание задачи, серии повторов или
// запись времени плюс закрытие входящего.
func (r Result) Ops(inboxID string) ([]changeplan.Op, error) {
	resolve := changeplan.Op{Op: "inbox", ID: inboxID, Status: "accepted", To: "$t"}
	if r.RRule != nil && *r.RRule != "" && (r.Intent == IntentTask || r.Intent == IntentEvent) {
		return r.recurOps(inboxID)
	}
	switch r.Intent {
	case IntentTimeLog:
		if r.TimeMinutes == nil || *r.TimeMinutes <= 0 {
			return nil, errors.New("не понял, сколько времени")
		}
		if r.TimeItemID != nil && *r.TimeItemID != "" {
			resolve.To = *r.TimeItemID
			return []changeplan.Op{
				{Op: "log_time", ID: *r.TimeItemID, Minutes: *r.TimeMinutes, Note: "из TG"},
				resolve,
			}, nil
		}
		// Задачи под это время нет — заводим сразу выполненную, чтобы время не потерялось.
		item, err := r.item()
		if err != nil {
			return nil, err
		}
		item["status"] = raw("done")
		return []changeplan.Op{
			{Op: "create", Ref: "t", Item: item},
			{Op: "log_time", ID: "$t", Minutes: *r.TimeMinutes, Note: "из TG"},
			resolve,
		}, nil
	case IntentTask, IntentEvent, IntentNote:
		item, err := r.item()
		if err != nil {
			return nil, err
		}
		return []changeplan.Op{{Op: "create", Ref: "t", Item: item}, resolve}, nil
	}
	return nil, errors.New("непонятно, что сделать — оставь на вечерний разбор")
}

func (r Result) item() (map[string]json.RawMessage, error) {
	title := strings.TrimSpace(r.Title)
	if title == "" {
		return nil, errors.New("пустой заголовок")
	}
	kind := "task"
	switch r.Intent {
	case IntentEvent:
		kind = "event"
	case IntentNote:
		kind = "note"
	}
	m := map[string]json.RawMessage{
		"kind": raw(kind), "title": raw(title), "source": raw("tg"),
		"important": raw(r.Important), "urgent": raw(r.Urgent),
	}
	if r.Sphere != nil {
		m["sphere"] = raw(*r.Sphere)
	}
	if r.PlannedDate != nil {
		if _, err := domain.ParseDate(*r.PlannedDate); err != nil {
			return nil, fmt.Errorf("planned_date %q", *r.PlannedDate)
		}
		m["planned_date"] = raw(*r.PlannedDate)
	}
	for key, v := range map[string]*string{"start_at": r.StartAt, "end_at": r.EndAt, "deadline": r.Deadline} {
		if v == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, *v)
		if err != nil {
			return nil, fmt.Errorf("%s %q", key, *v)
		}
		m[key] = raw(t)
	}
	if r.EndAt != nil && r.StartAt == nil {
		delete(m, "end_at")
	}
	if r.EstimateMin != nil && *r.EstimateMin > 0 {
		m["estimate_min"] = raw(*r.EstimateMin)
	}
	if len(r.Tags) > 0 {
		m["tags"] = raw(r.Tags)
	}
	var body []domain.Block
	if r.Details != nil && strings.TrimSpace(*r.Details) != "" {
		body = append(body, domain.Block{"type": "md", "text": *r.Details})
	}
	if len(r.Checklist) > 0 {
		items := make([]map[string]any, 0, len(r.Checklist))
		for _, c := range r.Checklist {
			items = append(items, map[string]any{"text": c, "done": false})
		}
		body = append(body, domain.Block{"type": "checklist", "items": items})
	}
	if len(body) > 0 {
		m["body"] = raw(body)
	}
	return m, nil
}

// recurOps — серия: день старта и время берутся из разобранных дат, в шаблон
// даты не попадают (их задаёт правило).
func (r Result) recurOps(inboxID string) ([]changeplan.Op, error) {
	item, err := r.item()
	if err != nil {
		return nil, err
	}
	op := changeplan.Op{Op: "recur", Rule: *r.RRule, Item: item}
	delete(item, "planned_date")
	delete(item, "start_at")
	delete(item, "end_at")
	delete(item, "deadline")
	if r.PlannedDate != nil {
		op.Start = *r.PlannedDate
	}
	if r.StartAt != nil {
		start, err := time.Parse(time.RFC3339, *r.StartAt)
		if err != nil {
			return nil, fmt.Errorf("start_at %q", *r.StartAt)
		}
		start = start.In(domain.MSK)
		op.Start = start.Format(time.DateOnly)
		op.Time = start.Format("15:04")
		if r.EndAt != nil {
			if end, err := time.Parse(time.RFC3339, *r.EndAt); err == nil && end.After(start) {
				op.DurationMin = int(end.Sub(start).Minutes())
			}
		}
	}
	// У серии нет одной задачи, на которую сослаться из входящего — просто закрываем его.
	return []changeplan.Op{op, {Op: "inbox", ID: inboxID, Status: "accepted"}}, nil
}

func raw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
