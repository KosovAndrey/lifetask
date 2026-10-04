package api_test

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gitlab.com/KosovAndrey/lifeplan/internal/api"
	"gitlab.com/KosovAndrey/lifeplan/internal/files"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
	"gitlab.com/KosovAndrey/lifeplan/internal/testdb"
)

func TestFilesUploadDownload(t *testing.T) {
	a := api.New(store.New(testdb.New(t)), token)
	a.Files = files.New(t.TempDir(), nil)
	h := a.Handler()
	c := client{t, h}

	var it struct{ ID string }
	c.do("POST", "/api/items", `{"title":"Договор"}`, &it)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, _ := mw.CreateFormFile("file", "договор.pdf")
	w.Write([]byte("%PDF-1.4 test"))
	w, _ = mw.CreateFormFile("file", "x.html")
	w.Write([]byte("<script>alert(1)</script>"))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/items/"+it.ID+"/files", &body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}

	var got struct{ Body []map[string]any }
	c.do("GET", "/api/items/"+it.ID, "", &got)
	if len(got.Body) != 2 {
		t.Fatalf("body: %v", got.Body)
	}
	get := func(fid string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/files/"+fid, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	pdf := get(got.Body[0]["file_id"].(string))
	if pdf.Code != 200 || pdf.Body.String() != "%PDF-1.4 test" || pdf.Header().Get("Content-Type") != "application/pdf" ||
		!strings.HasPrefix(pdf.Header().Get("Content-Disposition"), "inline") {
		t.Fatalf("pdf: %d %v", pdf.Code, pdf.Header())
	}
	// HTML не показывается в браузере, а скачивается — и в песочнице.
	html := get(got.Body[1]["file_id"].(string))
	if html.Header().Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(html.Header().Get("Content-Disposition"), "attachment") ||
		!strings.Contains(html.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("html: %v", html.Header())
	}
	// Файл, на который никто не ссылается, не отдаётся.
	if rec := get("l:00000000000000000000000000000000"); rec.Code != 404 {
		t.Fatalf("чужой id: %d", rec.Code)
	}
}

func TestSphereSettings(t *testing.T) {
	c := client{t, api.New(store.New(testdb.New(t)), token).Handler()}

	var sp struct {
		ID    int
		Slug  string
		Color string
		Style map[string]any
		Sort  int
	}
	if code := c.do("POST", "/api/spheres", `{"name":"Семья и друзья","color":"#aa3366","icon":"👪"}`, &sp); code != 201 {
		t.Fatalf("create: %d", code)
	}
	if sp.Slug != "semya_i_druzya" || sp.Color != "#AA3366" || sp.Sort <= 70 {
		t.Fatalf("новая сфера: %+v", sp)
	}
	body := `{"color_dark":"#882244","gcal_color":"4","hint":"встречи, подарки, звонки родным"}`
	c.do("PATCH", "/api/spheres/"+itoa(sp.ID), body, &sp)
	if sp.Style["color_dark"] != "#882244" || sp.Style["gcal_color"] != "4" || sp.Style["hint"] == nil {
		t.Fatalf("style: %v", sp.Style)
	}
	sp.Style = nil // иначе json дольёт ключи в старую map
	c.do("PATCH", "/api/spheres/"+itoa(sp.ID), `{"gcal_color":"","archived":true}`, &sp)
	if _, ok := sp.Style["gcal_color"]; ok {
		t.Fatal("пустое значение должно удалять настройку")
	}
	var list, all []any
	c.do("GET", "/api/spheres", "", &list)
	c.do("GET", "/api/spheres?all=1", "", &all)
	if len(all) != len(list)+2 { // + архивные «Финансы» и новая
		t.Fatalf("сферы: %d / %d", len(list), len(all))
	}
	if code := c.do("PATCH", "/api/spheres/"+itoa(sp.ID), `{"color":"red"}`, nil); code != 400 {
		t.Fatalf("плохой цвет: %d", code)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
