package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"time"
)

const (
	LoginTTL   = 10 * time.Minute
	SessionTTL = 90 * 24 * time.Hour
)

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// NewLoginToken — одноразовая ссылка для входа, живёт LoginTTL.
func (s *Store) NewLoginToken(ctx context.Context) (string, error) {
	t := randomToken()
	_, err := s.db.Exec(ctx, `INSERT INTO login_tokens (token_hash, expires_at) VALUES ($1, $2)`,
		hashToken(t), time.Now().Add(LoginTTL))
	return t, err
}

// Login гасит одноразовый токен и открывает сессию. Пустая строка — токен
// неверный, просрочен или уже использован.
func (s *Store) Login(ctx context.Context, token, userAgent string) (string, error) {
	tag, err := s.db.Exec(ctx, `UPDATE login_tokens SET used_at = now()
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()`, hashToken(token))
	if err != nil || tag.RowsAffected() == 0 {
		return "", err
	}
	sid := randomToken()
	_, err = s.db.Exec(ctx, `INSERT INTO sessions (id_hash, expires_at, user_agent) VALUES ($1, $2, $3)`,
		hashToken(sid), time.Now().Add(SessionTTL), userAgent)
	return sid, err
}

func (s *Store) ValidSession(ctx context.Context, sid string) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE id_hash = $1 AND expires_at > now())`,
		hashToken(sid)).Scan(&ok)
	return ok, err
}

func (s *Store) Logout(ctx context.Context, sid string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE id_hash = $1`, hashToken(sid))
	return err
}
