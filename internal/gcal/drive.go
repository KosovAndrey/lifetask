package gcal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// Drive со scope drive.file: приложение видит только файлы, которые создало само.
// Остальной Drive ему недоступен.
const (
	ScopeDrive  = "https://www.googleapis.com/auth/drive.file"
	driveAPI    = "https://www.googleapis.com/drive/v3"
	driveUpload = "https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart&fields=id"
	folderMime  = "application/vnd.google-apps.folder"
)

type DriveFile struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	CreatedTime time.Time `json:"createdTime"`
}

// DriveFolder находит папку по имени внутри parent ("" — корень) или создаёт её.
func (g *Google) DriveFolder(ctx context.Context, name, parent string) (string, error) {
	if parent == "" {
		parent = "root"
	}
	q := fmt.Sprintf("name = '%s' and mimeType = '%s' and '%s' in parents and trashed = false",
		driveEscape(name), folderMime, driveEscape(parent))
	var resp struct{ Files []DriveFile }
	if err := g.do(ctx, "GET", driveAPI+"/files?fields=files(id,name)&q="+url.QueryEscape(q), nil, &resp); err != nil {
		return "", err
	}
	if len(resp.Files) > 0 {
		return resp.Files[0].ID, nil
	}
	var created DriveFile
	err := g.do(ctx, "POST", driveAPI+"/files?fields=id",
		map[string]any{"name": name, "mimeType": folderMime, "parents": []string{parent}}, &created)
	return created.ID, err
}

// DriveUpload кладёт файл в папку одним multipart-запросом (до десятков МБ — то, что нужно).
func (g *Google) DriveUpload(ctx context.Context, folder, name, mime string, data []byte) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	meta, _ := json.Marshal(map[string]any{"name": name, "parents": []string{folder}})
	part, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	part.Write(meta)
	if mime == "" {
		mime = "application/octet-stream"
	}
	part, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {mime}})
	part.Write(data)
	mw.Close()

	resp, err := g.raw(ctx, "POST", driveUpload, "multipart/related; boundary="+mw.Boundary(), &body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out DriveFile
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("google drive: не вернул id файла")
	}
	return out.ID, nil
}

// DriveOpen — содержимое файла потоком; закрыть должен вызывающий.
func (g *Google) DriveOpen(ctx context.Context, id string) (io.ReadCloser, error) {
	resp, err := g.raw(ctx, "GET", driveAPI+"/files/"+url.PathEscape(id)+"?alt=media", "", nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// DriveList — файлы папки, старые первыми.
func (g *Google) DriveList(ctx context.Context, folder string) ([]DriveFile, error) {
	q := fmt.Sprintf("'%s' in parents and trashed = false", driveEscape(folder))
	var out []DriveFile
	pageToken := ""
	for {
		var resp struct {
			Files         []DriveFile
			NextPageToken string
		}
		u := driveAPI + "/files?orderBy=createdTime&pageSize=1000&fields=nextPageToken,files(id,name,createdTime)&q=" +
			url.QueryEscape(q) + "&pageToken=" + url.QueryEscape(pageToken)
		if err := g.do(ctx, "GET", u, nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Files...)
		if pageToken = resp.NextPageToken; pageToken == "" {
			return out, nil
		}
	}
}

// DriveTrash убирает файл в корзину Drive (30 дней можно восстановить).
func (g *Google) DriveTrash(ctx context.Context, id string) error {
	err := g.do(ctx, "PATCH", driveAPI+"/files/"+url.PathEscape(id), map[string]bool{"trashed": true}, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// raw — запрос с произвольным телом; ответ 2xx отдаётся как есть (тело закрывает вызывающий).
func (g *Google) raw(ctx context.Context, method, rawURL, contentType string, body io.Reader) (*http.Response, error) {
	at, err := g.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+at)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// Большие файлы качаются дольше общего таймаута клиента — свой клиент без него,
	// время ограничивает контекст запроса.
	client := *g.oauth.client()
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		resp.Body.Close()
		return nil, fmt.Errorf("google %s %s: %s: %.300s", method, strings.SplitN(rawURL, "?", 2)[0], resp.Status, raw)
	}
	return resp, nil
}

// driveEscape экранирует строку для языка запросов Drive.
func driveEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}
