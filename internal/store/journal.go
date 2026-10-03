package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// Journal — запись дневника за день. Текст хранится md-блоком в body, чтобы
// позже туда же ложились и другие блоки (итоги от Claude, ссылки).
type Journal struct {
	Date    domain.Date `json:"date"`
	Mood    *int        `json:"mood,omitempty"` // 1..5
	Text    string      `json:"text"`
	Summary string      `json:"summary"`
	// Факты дня — для веба и для разбора с Claude.
	Done    []domain.Item `json:"done"`
	Minutes int           `json:"minutes"`
}

func (s *Store) Journal(ctx context.Context, d domain.Date) (Journal, error) {
	j := Journal{Date: d, Done: []domain.Item{}}
	var body []domain.Block
	err := s.db.QueryRow(ctx, `SELECT mood, body, summary FROM journal_days WHERE date=$1`, d.String()).Scan(&j.Mood, &body, &j.Summary)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return j, err
	}
	for _, b := range body {
		if b["type"] == "md" {
			if t, ok := b["text"].(string); ok {
				j.Text = t
			}
		}
	}
	from, to := d.Time, d.AddDays(1).Time
	if j.Done, err = s.queryItems(ctx, `i.done_at >= $1 AND i.done_at < $2 AND i.kind <> 'note' ORDER BY i.done_at`, from, to); err != nil {
		return j, err
	}
	err = s.db.QueryRow(ctx, `SELECT COALESCE(sum(minutes), 0) FROM time_entries
		WHERE COALESCE(started_at, created_at) >= $1 AND COALESCE(started_at, created_at) < $2`, from, to).Scan(&j.Minutes)
	return j, err
}

// JournalPatch — что меняем: nil — не трогаем.
type JournalPatch struct {
	Mood       *int    `json:"mood"`
	Text       *string `json:"text"`
	AppendText *string `json:"append_text"` // дописать абзацем (ответ боту)
	Summary    *string `json:"summary"`
}

func (s *Store) SaveJournal(ctx context.Context, d domain.Date, p JournalPatch) (Journal, error) {
	if p.Mood != nil && (*p.Mood < 1 || *p.Mood > 5) {
		return Journal{}, errors.New("настроение — от 1 до 5")
	}
	cur, err := s.Journal(ctx, d)
	if err != nil {
		return cur, err
	}
	if p.Mood != nil {
		cur.Mood = p.Mood
	}
	if p.Text != nil {
		cur.Text = *p.Text
	}
	if p.AppendText != nil && strings.TrimSpace(*p.AppendText) != "" {
		add := strings.TrimSpace(*p.AppendText)
		if cur.Text != "" {
			cur.Text += "\n\n"
		}
		cur.Text += time.Now().In(domain.MSK).Format("15:04") + " — " + add
	}
	if p.Summary != nil {
		cur.Summary = *p.Summary
	}
	body := []domain.Block{}
	if cur.Text != "" {
		body = append(body, domain.Block{"type": "md", "text": cur.Text})
	}
	_, err = s.db.Exec(ctx, `INSERT INTO journal_days (date, mood, body, summary, updated_at) VALUES ($1,$2,$3,$4, now())
		ON CONFLICT (date) DO UPDATE SET mood=EXCLUDED.mood, body=EXCLUDED.body, summary=EXCLUDED.summary, updated_at=now()`,
		d.String(), cur.Mood, body, cur.Summary)
	return cur, err
}

// JournalList — последние записи (для ленты в вебе).
func (s *Store) JournalList(ctx context.Context, limit int) ([]Journal, error) {
	rows, err := s.db.Query(ctx, `SELECT date, mood, body, summary FROM journal_days ORDER BY date DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Journal{}
	for rows.Next() {
		var j Journal
		var d time.Time
		var body []domain.Block
		if err := rows.Scan(&d, &j.Mood, &body, &j.Summary); err != nil {
			return nil, err
		}
		j.Date = *toDate(&d)
		for _, b := range body {
			if t, ok := b["text"].(string); ok && b["type"] == "md" {
				j.Text = t
			}
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// SetBriefMessage / BriefByMessage — связь брифа с сообщением в Telegram.
func (s *Store) SetBriefMessage(ctx context.Context, d domain.Date, kind string, msgID int64) error {
	_, err := s.db.Exec(ctx, `UPDATE briefs SET tg_message_id=$3 WHERE date=$1 AND kind=$2`, d.String(), kind, msgID)
	return err
}

func (s *Store) BriefByMessage(ctx context.Context, msgID int64) (domain.Date, string, bool, error) {
	var d time.Time
	var kind string
	err := s.db.QueryRow(ctx, `SELECT date, kind FROM briefs WHERE tg_message_id=$1`, msgID).Scan(&d, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Date{}, "", false, nil
	}
	if err != nil {
		return domain.Date{}, "", false, err
	}
	return *toDate(&d), kind, true, nil
}

// ClaimIdempotencyKey — true, если ключ новый (запрос выполняем); false — повтор.
func (s *Store) ClaimIdempotencyKey(ctx context.Context, key string) (bool, error) {
	tag, err := s.db.Exec(ctx, `INSERT INTO idempotency_keys (key) VALUES ($1) ON CONFLICT DO NOTHING`, key)
	return tag.RowsAffected() == 1, err
}

func (s *Store) ReleaseIdempotencyKey(ctx context.Context, key string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM idempotency_keys WHERE key=$1`, key)
	return err
}
