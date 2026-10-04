package api

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/files"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
)

func (a *API) createSphere(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Color string `json:"color"`
		Icon  string `json:"icon"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sp, err := a.st.CreateSphere(r.Context(), req.Name, req.Color, req.Icon)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sp)
}

func (a *API) patchSphere(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("id сферы — число"))
		return
	}
	var p store.SpherePatch
	if err := decode(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sp, err := a.st.UpdateSphere(r.Context(), id, p)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sp)
}

// uploadFiles — multipart, поле file (можно несколько). Файлы кладутся в
// хранилище по мере чтения, потом одним патчем дописываются в body задачи.
func (a *API) uploadFiles(w http.ResponseWriter, r *http.Request) {
	if a.Files == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("хранилище файлов не настроено"))
		return
	}
	id := r.PathValue("id")
	if _, err := a.st.GetItem(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*files.MaxSize)
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var blocks []domain.Block
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			continue
		}
		data, err := files.ReadAll(part)
		if err != nil {
			writeErr(w, http.StatusRequestEntityTooLarge, err)
			return
		}
		b, err := a.Files.Put(r.Context(), part.FileName(), part.Header.Get("Content-Type"), data)
		if err != nil {
			fail(w, err)
			return
		}
		blocks = append(blocks, b)
	}
	if len(blocks) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("нет файлов в поле file"))
		return
	}
	var out domain.Item
	err = a.st.InTx(r.Context(), func(tx *store.Store) error {
		var err error
		out, err = tx.AttachFiles(r.Context(), id, blocks, "me")
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// downloadFile отдаёт только файлы, на которые ссылается задача или входящее.
// Картинки, PDF, текст и медиа показываются в браузере, остальное скачивается;
// CSP sandbox не даёт файлу исполнить скрипт от имени сайта.
func (a *API) downloadFile(w http.ResponseWriter, r *http.Request) {
	if a.Files == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("хранилище файлов не настроено"))
		return
	}
	fid := r.PathValue("fid")
	b, err := a.st.FileBlock(r.Context(), fid)
	if err != nil {
		fail(w, err)
		return
	}
	rc, err := a.Files.Open(r.Context(), fid)
	if errors.Is(err, files.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	defer rc.Close()
	name, _ := b["name"].(string)
	mimeType, _ := b["mime"].(string)
	disp := "attachment"
	if files.Inline(mimeType) {
		disp = "inline"
	} else {
		mimeType = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", mimeType)
	h.Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": name}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'")
	// Содержимое по file_id не меняется — пусть браузер кеширует.
	h.Set("Cache-Control", "private, max-age=604800, immutable")
	_, _ = io.Copy(w, rc)
}
