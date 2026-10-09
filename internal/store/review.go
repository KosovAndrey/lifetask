package store

import (
	"context"
	"errors"
	"strings"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// Review — что стоит пересмотреть: разовые задачи, которые переносятся снова и
// снова, и долгожители. Уже разбитые на подзадачи не показываем — решение принято.
type Review struct {
	Postponed []domain.Item `json:"postponed"`
	Stale     []domain.Item `json:"stale"`
}

const reviewOpen = `i.recurrence_id IS NULL AND i.kind IN ('task','goal')
	AND i.status NOT IN ('done','cancelled','someday','waiting')
	AND NOT EXISTS (SELECT 1 FROM items c WHERE c.parent_id = i.id AND c.status NOT IN ('done','cancelled'))`

func (s *Store) Review(ctx context.Context) (Review, error) {
	var r Review
	var err error
	if r.Postponed, err = s.queryItems(ctx, reviewOpen+` AND i.postpone_count >= 2
		ORDER BY i.postpone_count DESC, i.created_at LIMIT 30`); err != nil {
		return r, err
	}
	r.Stale, err = s.queryItems(ctx, reviewOpen+` AND i.postpone_count < 2 AND i.kind = 'task'
		AND i.created_at < now() - interval '14 days' ORDER BY i.created_at LIMIT 30`)
	return r, err
}

// ReviewCount — сколько ждёт разбора (бейдж в меню).
func (s *Store) ReviewCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM items i WHERE `+reviewOpen+` AND i.postpone_count >= 2`).Scan(&n)
	return n, err
}

// Split разбивает задачу на подзадачи (вызывать в транзакции): они получают её
// сферу, проект и дату, а сама задача становится контейнером без даты — её
// прогресс считается по частям, и в планах дня она больше не мешает.
func (s *Store) Split(ctx context.Context, id string, titles []string, actor string) (domain.Item, error) {
	parent, err := s.GetItem(ctx, id)
	if err != nil {
		return parent, err
	}
	n := 0
	for _, t := range titles {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		child := domain.Item{Title: t, Kind: domain.KindTask, Status: domain.StatusTodo, ParentID: &parent.ID,
			SphereID: parent.SphereID, ProjectID: parent.ProjectID, PlannedDate: parent.PlannedDate, Source: parent.Source}
		if parent.Source == "gcal" || parent.Source == "gtasks" {
			child.Source = "web"
		}
		if _, err := s.CreateItem(ctx, child, actor); err != nil {
			return parent, err
		}
		n++
	}
	if n == 0 {
		return parent, errors.New("нужна хотя бы одна подзадача")
	}
	if parent.PlannedDate == nil && parent.StartAt == nil {
		return s.GetItem(ctx, id)
	}
	return s.UpdateItem(ctx, id, Patch{"planned_date": []byte("null"), "start_at": []byte("null"), "end_at": []byte("null")}, actor)
}
