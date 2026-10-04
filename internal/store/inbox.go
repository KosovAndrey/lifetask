package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

const inboxCols = `id, text, transcript, parsed, parse_error, status, item_id, tg_message_id, bot_message_id, created_at, files`

func scanInbox(row pgx.Row) (domain.InboxMessage, error) {
	var m domain.InboxMessage
	err := row.Scan(&m.ID, &m.Text, &m.Transcript, &m.Parsed, &m.ParseError, &m.Status, &m.ItemID,
		&m.TgMessageID, &m.BotMessageID, &m.CreatedAt, &m.Files)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

func (s *Store) AddInbox(ctx context.Context, m domain.InboxMessage) (domain.InboxMessage, error) {
	if strings.TrimSpace(m.Text) == "" && (m.Transcript == nil || *m.Transcript == "") {
		return m, errors.New("пустое сообщение")
	}
	var parsed any
	if len(m.Parsed) > 0 {
		parsed = string(m.Parsed)
	}
	files := m.Files
	if files == nil {
		files = []domain.Block{}
	}
	return scanInbox(s.db.QueryRow(ctx, `INSERT INTO inbox_messages (text, transcript, parsed, tg_message_id, files)
		VALUES ($1,$2,$3,$4,$5) RETURNING `+inboxCols, m.Text, m.Transcript, parsed, m.TgMessageID, files))
}

func (s *Store) GetInbox(ctx context.Context, id string) (domain.InboxMessage, error) {
	return scanInbox(s.db.QueryRow(ctx, `SELECT `+inboxCols+` FROM inbox_messages WHERE id=$1`, id))
}

// InboxByBotMessage — входящее, чью карточку пользователь процитировал ответом-уточнением.
func (s *Store) InboxByBotMessage(ctx context.Context, botMsgID int64) (domain.InboxMessage, error) {
	return scanInbox(s.db.QueryRow(ctx, `SELECT `+inboxCols+` FROM inbox_messages WHERE bot_message_id=$1`, botMsgID))
}

func (s *Store) OpenInbox(ctx context.Context) ([]domain.InboxMessage, error) {
	rows, err := s.db.Query(ctx, `SELECT `+inboxCols+` FROM inbox_messages
		WHERE status IN ('new','proposed','deferred') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.InboxMessage, error) { return scanInbox(r) })
}

// SaveParse — результат разбора ботом: черновик (или ошибка), карточка в TG, статус.
func (s *Store) SaveParse(ctx context.Context, id string, parsed json.RawMessage, parseErr *string, botMsgID *int64, status string) error {
	var p any
	if len(parsed) > 0 {
		p = string(parsed)
	}
	_, err := s.db.Exec(ctx, `UPDATE inbox_messages SET parsed=$2, parse_error=$3,
		bot_message_id=COALESCE($4, bot_message_id), status=$5 WHERE id=$1`, id, p, parseErr, botMsgID, status)
	return err
}

func (s *Store) SetTranscript(ctx context.Context, id, transcript string) error {
	_, err := s.db.Exec(ctx, `UPDATE inbox_messages SET transcript=$2 WHERE id=$1`, id, transcript)
	return err
}

func (s *Store) ResolveInbox(ctx context.Context, id, status string, itemID *string) error {
	switch status {
	case "accepted", "rejected", "deferred", "proposed":
	default:
		return fmt.Errorf("неизвестный статус инбокса %q", status)
	}
	tag, err := s.db.Exec(ctx, `UPDATE inbox_messages SET status=$2, item_id=COALESCE($3, item_id) WHERE id=$1`, id, status, itemID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if status == "accepted" && itemID != nil {
		return s.moveInboxFiles(ctx, id, *itemID)
	}
	return nil
}

// TimeCandidates — к чему может относиться «потратил 40м»: открытые задачи и
// закрытые за последние двое суток, свежие первыми.
func (s *Store) TimeCandidates(ctx context.Context, limit int) ([]domain.Item, error) {
	return s.queryItems(ctx, `i.kind IN ('task','event') AND (i.status NOT IN ('done','cancelled')
		OR i.done_at > now() - interval '2 days') ORDER BY i.updated_at DESC LIMIT $1`, limit)
}

// ── Брифы ────────────────────────────────────────────────────────────────────

// PutBrief — свой текст брифа (Claude на вечернем разборе). Если стандартный бриф
// уже ушёл, новый текст сбрасывает отметку и уходит повторно: разбор обычно
// позже 21:00, и итоговый план на завтра важнее автоматического.
func (s *Store) PutBrief(ctx context.Context, d domain.Date, kind, body string) error {
	switch kind {
	case "evening", "morning", "weekly":
	default:
		return fmt.Errorf("неизвестный вид брифа %q", kind)
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("пустой бриф")
	}
	_, err := s.db.Exec(ctx, `INSERT INTO briefs (date, kind, body) VALUES ($1,$2,$3)
		ON CONFLICT (date, kind) DO UPDATE SET body = EXCLUDED.body, sent_at = NULL`, d.String(), kind, body)
	return err
}

// PendingBrief — заготовленный текст, если он есть, и признак отправки.
func (s *Store) PendingBrief(ctx context.Context, d domain.Date, kind string) (body string, sent bool, err error) {
	var sentAt *time.Time
	err = s.db.QueryRow(ctx, `SELECT body, sent_at FROM briefs WHERE date=$1 AND kind=$2`, d.String(), kind).Scan(&body, &sentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return body, sentAt != nil, err
}

func (s *Store) MarkBriefSent(ctx context.Context, d domain.Date, kind, body string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO briefs (date, kind, body, sent_at) VALUES ($1,$2,$3, now())
		ON CONFLICT (date, kind) DO UPDATE SET body = EXCLUDED.body, sent_at = now()`, d.String(), kind, body)
	return err
}
