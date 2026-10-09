package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"gitlab.com/KosovAndrey/lifeplan/internal/changeplan"
	"gitlab.com/KosovAndrey/lifeplan/internal/files"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
)

type bufferedResponse struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(code int) {
	if b.code == 0 {
		b.code = code
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.code == 0 {
		b.code = http.StatusOK
	}
	return b.body.Write(p)
}
func (b *bufferedResponse) flush(w http.ResponseWriter) {
	for k, values := range b.header {
		w.Header()[k] = values
	}
	if b.code == 0 {
		b.code = http.StatusOK
	}
	w.WriteHeader(b.code)
	_, _ = w.Write(b.body.Bytes())
}

// The claim, database effects and replayable response commit together. In-flight
// duplicates wait on the unique key; a rollback leaves the request retryable.
func (a *API) idempotent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" || !strings.HasPrefix(r.URL.Path, "/api/") || r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if len(key) > 100 {
			writeErr(w, http.StatusBadRequest, errors.New("Idempotency-Key длиннее 100 символов"))
			return
		}
		// Spool uploads instead of holding a second copy of a large body in memory.
		body, err := os.CreateTemp("", "lifeplan-request-*")
		if err != nil {
			writeErr(w, 500, errors.New("не удалось подготовить запрос"))
			return
		}
		defer os.Remove(body.Name())
		defer body.Close()
		hash := sha256.New()
		_, _ = io.WriteString(hash, r.Method+"\n"+r.URL.RequestURI()+"\n"+r.Header.Get("Content-Type")+"\n")
		limit := int64(1 << 20)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			limit = 4 * files.MaxSize
		}
		if _, err := io.Copy(io.MultiWriter(body, hash), http.MaxBytesReader(w, r.Body, limit)); err != nil {
			writeErr(w, http.StatusRequestEntityTooLarge, err)
			return
		}
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			writeErr(w, 500, err)
			return
		}
		r.Body = io.NopCloser(body)
		fingerprint := hex.EncodeToString(hash.Sum(nil))
		response := &bufferedResponse{header: make(http.Header)}
		rejected := errors.New("rollback unsuccessful request")
		err = a.st.InTx(r.Context(), func(tx *store.Store) error {
			tag, err := tx.Raw().Exec(r.Context(), `INSERT INTO idempotency_keys (key, request_hash) VALUES ($1,$2) ON CONFLICT DO NOTHING`, key, fingerprint)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				var originalHash *string
				var code *int
				var payload []byte
				if err := tx.Raw().QueryRow(r.Context(), `SELECT request_hash, status_code, response_headers, response_body FROM idempotency_keys WHERE key=$1`, key).Scan(&originalHash, &code, &response.header, &payload); err != nil {
					return err
				}
				if originalHash == nil || code == nil || *originalHash != fingerprint {
					response.header = make(http.Header)
					writeErr(response, http.StatusConflict, errors.New("ключ уже использован другим запросом или результат старого запроса неизвестен; проверь данные"))
					return rejected
				}
				response.code = *code
				_, _ = response.body.Write(payload)
				return nil
			}
			scoped := *a
			scoped.st, scoped.plans = tx, changeplan.New(tx)
			scoped.routes().ServeHTTP(response, r)
			if response.code == 0 {
				response.code = http.StatusOK
			}
			if response.code >= 400 {
				return rejected
			}
			_, err = tx.Raw().Exec(r.Context(), `UPDATE idempotency_keys SET status_code=$2, response_headers=$3, response_body=$4 WHERE key=$1`, key, response.code, response.header, response.body.Bytes())
			return err
		})
		if err != nil && !errors.Is(err, rejected) {
			slog.Error("idempotent request", "err", err)
			writeErr(w, http.StatusServiceUnavailable, errors.New("изменения не подтверждены; повтори запрос с тем же ключом"))
			return
		}
		response.flush(w)
	})
}
