package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// AttachFiles дописывает блоки файлов в body задачи — через UpdateItem, чтобы
// изменение попало в журнал. Вызывать в транзакции (UpdateItem блокирует строку).
func (s *Store) AttachFiles(ctx context.Context, itemID string, blocks []domain.Block, actor string) (domain.Item, error) {
	if s.pool != nil {
		var out domain.Item
		err := s.InTx(ctx, func(tx *Store) error {
			var err error
			out, err = tx.AttachFiles(ctx, itemID, blocks, actor)
			return err
		})
		return out, err
	}
	cur, err := s.getForUpdate(ctx, itemID)
	if err != nil {
		return cur, err
	}
	raw, err := json.Marshal(append(cur.Body, blocks...))
	if err != nil {
		return cur, err
	}
	return s.UpdateItem(ctx, itemID, Patch{"body": raw}, actor)
}

// AddInboxFile — файл к ещё не разобранной заметке: приедет в задачу при принятии.
func (s *Store) AddInboxFile(ctx context.Context, inboxID string, block domain.Block) error {
	raw, err := json.Marshal([]domain.Block{block})
	if err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `UPDATE inbox_messages SET files = files || $2::jsonb WHERE id=$1`, inboxID, string(raw))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// moveInboxFiles переносит файлы принятой заметки в её задачу.
func (s *Store) moveInboxFiles(ctx context.Context, inboxID, itemID string) error {
	var blocks []domain.Block
	err := s.db.QueryRow(ctx, `SELECT files FROM inbox_messages WHERE id=$1`, inboxID).Scan(&blocks)
	if err != nil || len(blocks) == 0 {
		return err
	}
	if _, err := s.AttachFiles(ctx, itemID, blocks, "bot"); err != nil {
		return fmt.Errorf("вложения заметки: %w", err)
	}
	_, err = s.db.Exec(ctx, `UPDATE inbox_messages SET files='[]' WHERE id=$1`, inboxID)
	return err
}

// FileBlock — блок файла с этим file_id из задачи или входящего. Отдаём только
// файлы, на которые есть ссылка: по произвольному id ничего не скачать.
func (s *Store) FileBlock(ctx context.Context, fileID string) (domain.Block, error) {
	probe, _ := json.Marshal([]map[string]string{{"type": "file", "file_id": fileID}})
	var raw []byte
	err := s.db.QueryRow(ctx, `
		SELECT b FROM (
			SELECT body AS arr FROM items WHERE body @> $1::jsonb
			UNION ALL
			SELECT files FROM inbox_messages WHERE files @> $1::jsonb
		) src, jsonb_array_elements(src.arr) b
		WHERE b->>'file_id' = $2 LIMIT 1`, string(probe), fileID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var b domain.Block
	return b, json.Unmarshal(raw, &b)
}
