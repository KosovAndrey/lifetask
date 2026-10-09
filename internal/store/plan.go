package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// DefaultEstimateMin — сколько считать задаче без оценки, когда прикидываем, влезет ли день.
const DefaultEstimateMin = 30

// DayPlan — день вместе с прикидкой загрузки: утренний ритуал в вебе и бриф в 07:00.
type DayPlan struct {
	Day
	CapacityMin int `json:"capacity_min"` // время на дела в день (настройка)
	FreeMin     int `json:"free_min"`     // минус встречи и блоки со временем
	LoadMin     int `json:"load_min"`     // открытые задачи дня (без оценки — по DefaultEstimateMin)
	Unestimated int `json:"unestimated"`  // сколько задач без оценки
	// Fits и Overflow делят открытые задачи дня по приоритету: что влезает в
	// свободное время и что нет. Ожидание и «когда-нибудь» не в счёт — это не твоя работа сейчас.
	Fits     []domain.Item `json:"fits"`
	Overflow []domain.Item `json:"overflow"`
	Brief    *Brief        `json:"brief,omitempty"`  // утренний бриф дня, если есть
	Health   *HealthDay    `json:"health,omitempty"` // сон прошлой ночи и т.п.
}

type Brief struct {
	Kind string `json:"kind"`
	Body string `json:"body"`
	Sent bool   `json:"sent"`
}

// estimate — оценка задачи для прикидки загрузки.
func estimate(it domain.Item) int {
	if it.EstimateMin != nil {
		return max(0, *it.EstimateMin-it.SpentMin)
	}
	return max(0, DefaultEstimateMin-it.SpentMin)
}

// planRank — порядок «что делать первым»: начатое, затем квадранты матрицы;
// внутри — ближний дедлайн и более старые задачи.
func planRank(a, b domain.Item) bool {
	ra, rb := a.Quadrant(), b.Quadrant()
	if a.Status == domain.StatusDoing {
		ra = 0
	}
	if b.Status == domain.StatusDoing {
		rb = 0
	}
	if ra != rb {
		return ra < rb
	}
	switch {
	case a.Deadline != nil && b.Deadline == nil:
		return true
	case a.Deadline == nil && b.Deadline != nil:
		return false
	case a.Deadline != nil && !a.Deadline.Equal(*b.Deadline):
		return a.Deadline.Before(*b.Deadline)
	}
	return a.CreatedAt.Before(b.CreatedAt)
}

// Plan — день и прикидка: какие задачи влезают в свободное время, какие нет.
func (s *Store) Plan(ctx context.Context, d domain.Date) (DayPlan, error) {
	day, err := s.Day(ctx, d)
	if err != nil {
		return DayPlan{}, err
	}
	set, err := s.Settings(ctx)
	if err != nil {
		return DayPlan{}, err
	}
	p := DayPlan{Day: day, CapacityMin: set.CapacityMin, Fits: []domain.Item{}, Overflow: []domain.Item{}}
	p.FreeMin = max(0, p.CapacityMin-day.BusyMin)

	var open []domain.Item
	for _, it := range day.Planned {
		if it.Status.Closed() || it.Status == domain.StatusWaiting || it.Status == domain.StatusSomeday || it.Kind == domain.KindEvent {
			continue
		}
		open = append(open, it)
	}
	sort.SliceStable(open, func(i, j int) bool { return planRank(open[i], open[j]) })
	used := 0
	for _, it := range open {
		e := estimate(it)
		if it.EstimateMin == nil {
			p.Unestimated++
		}
		p.LoadMin += e
		// Порядок важен: задача, которая не влезла, не пропускает вперёд менее важные.
		if len(p.Overflow) == 0 && used+e <= p.FreeMin {
			used += e
			p.Fits = append(p.Fits, it)
		} else {
			p.Overflow = append(p.Overflow, it)
		}
	}

	body, sent, err := s.PendingBrief(ctx, d, "morning")
	if err != nil {
		return p, err
	}
	if body != "" {
		p.Brief = &Brief{Kind: "morning", Body: body, Sent: sent}
	}
	if hs, err := s.Health(ctx, d, d.AddDays(1)); err != nil {
		return p, err
	} else if len(hs) > 0 {
		p.Health = &hs[0]
	}
	return p, nil
}

// Reschedule переносит задачи на дату (вызывать в транзакции). Задача со временем
// сохраняет время суток и длительность. date == nil — в «когда-нибудь» без даты.
// Каждый перенос идёт через UpdateItem: журнал и счётчик переносов как обычно.
func (s *Store) Reschedule(ctx context.Context, ids []string, date *domain.Date, actor string) ([]domain.Item, error) {
	if len(ids) == 0 {
		return nil, errors.New("нечего переносить")
	}
	out := make([]domain.Item, 0, len(ids))
	for _, id := range ids {
		it, err := s.GetItem(ctx, id)
		if err != nil {
			return out, err
		}
		p := Patch{}
		if date == nil {
			p["status"] = json.RawMessage(`"someday"`)
			p["planned_date"] = json.RawMessage(`null`)
			p["start_at"] = json.RawMessage(`null`)
			p["end_at"] = json.RawMessage(`null`)
		} else {
			p["planned_date"] = mustJSON(date.String())
			if it.StartAt != nil {
				st := it.StartAt.In(domain.MSK)
				ns := time.Date(date.Year(), date.Month(), date.Day(), st.Hour(), st.Minute(), 0, 0, domain.MSK)
				p["start_at"] = mustJSON(ns)
				if it.EndAt != nil {
					p["end_at"] = mustJSON(ns.Add(it.EndAt.Sub(*it.StartAt)))
				}
			}
			if it.Status == domain.StatusSomeday {
				p["status"] = json.RawMessage(`"todo"`)
			}
		}
		next, err := s.UpdateItem(ctx, id, p, actor)
		if err != nil {
			return out, err
		}
		out = append(out, next)
	}
	return out, nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
