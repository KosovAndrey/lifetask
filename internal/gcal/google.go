// Package gcal — синхронизация с Google Calendar и Google Tasks. REST на голом
// HTTP: нужно с десяток вызовов, тянуть google-api-go ради них незачем.
package gcal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	ScopeCalendar = "https://www.googleapis.com/auth/calendar"
	ScopeTasks    = "https://www.googleapis.com/auth/tasks"
	RedirectURI   = "http://localhost:8765"
)

var ErrNotFound = errors.New("google: не найдено")

// Event — событие в нужном нам виде. Start/End — для событий со временем,
// Date — для событий на весь день.
type Event struct {
	ID          string
	Status      string // confirmed | cancelled
	Summary     string
	Description string
	Start, End  *time.Time
	Date        string // YYYY-MM-DD, если на весь день
	ColorID     string
	Etag        string
	LifeplanID  string // extendedProperties.private.lifeplan_id
}

type Task struct {
	ID, Title, Notes, Due string
}

// API — то, что синку нужно от Google (в тестах — фейк в памяти).
type API interface {
	FindCalendar(ctx context.Context, summary string) (string, error)
	CreateCalendar(ctx context.Context, summary string) (string, error)
	ListEvents(ctx context.Context, calendarID string, from, to time.Time) ([]Event, error)
	InsertEvent(ctx context.Context, calendarID string, e Event) (Event, error)
	UpdateEvent(ctx context.Context, calendarID string, e Event) (Event, error)
	DeleteEvent(ctx context.Context, calendarID, eventID string) error
	FindTaskList(ctx context.Context, title string) (string, error)
	CreateTaskList(ctx context.Context, title string) (string, error)
	OpenTasks(ctx context.Context, listID string) ([]Task, error)
	CompleteTask(ctx context.Context, listID, taskID string) error
}

// ── OAuth ────────────────────────────────────────────────────────────────────

type OAuth struct {
	ClientID, ClientSecret string
	HTTP                   *http.Client
}

func (o OAuth) AuthURL() string {
	q := url.Values{
		"client_id": {o.ClientID}, "redirect_uri": {RedirectURI}, "response_type": {"code"},
		"scope": {ScopeCalendar + " " + ScopeTasks + " " + ScopeDrive}, "access_type": {"offline"}, "prompt": {"consent"},
	}
	return "https://accounts.google.com/o/oauth2/v2/auth?" + q.Encode()
}

// Exchange принимает код или весь URL, на который перекинул Google, и возвращает refresh-токен.
func (o OAuth) Exchange(ctx context.Context, codeOrURL string) (string, error) {
	code := strings.TrimSpace(codeOrURL)
	if u, err := url.Parse(code); err == nil && u.Query().Get("code") != "" {
		code = u.Query().Get("code")
	}
	var tok struct {
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error_description"`
	}
	if err := o.token(ctx, url.Values{"code": {code}, "grant_type": {"authorization_code"}, "redirect_uri": {RedirectURI}}, &tok); err != nil {
		return "", err
	}
	if tok.RefreshToken == "" {
		return "", fmt.Errorf("Google не вернул refresh_token: %s", tok.Error)
	}
	return tok.RefreshToken, nil
}

func (o OAuth) token(ctx context.Context, form url.Values, out any) error {
	form.Set("client_id", o.ClientID)
	form.Set("client_secret", o.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := o.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("google oauth %s: %.300s", resp.Status, raw)
	}
	return json.Unmarshal(raw, out)
}

func (o OAuth) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// ── REST-клиент ──────────────────────────────────────────────────────────────

type Google struct {
	oauth   OAuth
	refresh string

	mu      sync.Mutex
	access  string
	expires time.Time
}

func NewGoogle(o OAuth, refreshToken string) *Google { return &Google{oauth: o, refresh: refreshToken} }

func (g *Google) accessToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.access != "" && time.Until(g.expires) > time.Minute {
		return g.access, nil
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := g.oauth.token(ctx, url.Values{"refresh_token": {g.refresh}, "grant_type": {"refresh_token"}}, &tok); err != nil {
		return "", err
	}
	g.access, g.expires = tok.AccessToken, time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second)
	return g.access, nil
}

func (g *Google) do(ctx context.Context, method, rawURL string, body, out any) error {
	at, err := g.accessToken(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+at)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.oauth.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrNotFound
	case resp.StatusCode >= 300:
		return fmt.Errorf("google %s %s: %s: %.300s", method, strings.SplitN(rawURL, "?", 2)[0], resp.Status, raw)
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

const calAPI = "https://www.googleapis.com/calendar/v3"
const tasksAPI = "https://tasks.googleapis.com/tasks/v1"

func (g *Google) FindCalendar(ctx context.Context, summary string) (string, error) {
	pageToken := ""
	for {
		var resp struct {
			Items []struct {
				ID, Summary string
			}
			NextPageToken string
		}
		if err := g.do(ctx, "GET", calAPI+"/users/me/calendarList?pageToken="+url.QueryEscape(pageToken), nil, &resp); err != nil {
			return "", err
		}
		for _, c := range resp.Items {
			if c.Summary == summary {
				return c.ID, nil
			}
		}
		if pageToken = resp.NextPageToken; pageToken == "" {
			return "", ErrNotFound
		}
	}
}

func (g *Google) CreateCalendar(ctx context.Context, summary string) (string, error) {
	var resp struct{ ID string }
	err := g.do(ctx, "POST", calAPI+"/calendars", map[string]string{"summary": summary, "timeZone": "Europe/Moscow"}, &resp)
	return resp.ID, err
}

type apiEvent struct {
	ID          string   `json:"id,omitempty"`
	Status      string   `json:"status,omitempty"`
	Summary     string   `json:"summary"`
	Description string   `json:"description,omitempty"`
	ColorID     string   `json:"colorId,omitempty"`
	Etag        string   `json:"etag,omitempty"`
	Start       *apiTime `json:"start,omitempty"`
	End         *apiTime `json:"end,omitempty"`
	Extended    *struct {
		Private map[string]string `json:"private,omitempty"`
	} `json:"extendedProperties,omitempty"`
}

type apiTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

func toAPI(e Event) apiEvent {
	a := apiEvent{ID: e.ID, Summary: e.Summary, Description: e.Description, ColorID: e.ColorID}
	if e.Start != nil {
		end := e.End
		if end == nil {
			t := e.Start.Add(30 * time.Minute) // у события в Google обязателен конец
			end = &t
		}
		a.Start = &apiTime{DateTime: e.Start.Format(time.RFC3339), TimeZone: "Europe/Moscow"}
		a.End = &apiTime{DateTime: end.Format(time.RFC3339), TimeZone: "Europe/Moscow"}
	}
	if e.LifeplanID != "" {
		a.Extended = &struct {
			Private map[string]string `json:"private,omitempty"`
		}{Private: map[string]string{"lifeplan_id": e.LifeplanID}}
	}
	return a
}

func fromAPI(a apiEvent) Event {
	e := Event{ID: a.ID, Status: a.Status, Summary: a.Summary, Description: a.Description, ColorID: a.ColorID, Etag: a.Etag}
	if a.Start != nil {
		if a.Start.DateTime != "" {
			if t, err := time.Parse(time.RFC3339, a.Start.DateTime); err == nil {
				e.Start = &t
			}
			if a.End != nil {
				if t, err := time.Parse(time.RFC3339, a.End.DateTime); err == nil {
					e.End = &t
				}
			}
		} else {
			e.Date = a.Start.Date
		}
	}
	if a.Extended != nil {
		e.LifeplanID = a.Extended.Private["lifeplan_id"]
	}
	return e
}

// ListEvents — события окна с развёрнутыми повторами; отменённые не возвращаются.
func (g *Google) ListEvents(ctx context.Context, calendarID string, from, to time.Time) ([]Event, error) {
	var out []Event
	pageToken := ""
	for {
		q := url.Values{"timeMin": {from.Format(time.RFC3339)}, "timeMax": {to.Format(time.RFC3339)},
			"singleEvents": {"true"}, "maxResults": {"250"}, "pageToken": {pageToken}}
		var resp struct {
			Items         []apiEvent
			NextPageToken string
		}
		if err := g.do(ctx, "GET", calAPI+"/calendars/"+url.PathEscape(calendarID)+"/events?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		for _, a := range resp.Items {
			if a.Status != "cancelled" {
				out = append(out, fromAPI(a))
			}
		}
		if pageToken = resp.NextPageToken; pageToken == "" {
			return out, nil
		}
	}
}

func (g *Google) InsertEvent(ctx context.Context, calendarID string, e Event) (Event, error) {
	var a apiEvent
	err := g.do(ctx, "POST", calAPI+"/calendars/"+url.PathEscape(calendarID)+"/events", toAPI(e), &a)
	return fromAPI(a), err
}

func (g *Google) UpdateEvent(ctx context.Context, calendarID string, e Event) (Event, error) {
	var a apiEvent
	err := g.do(ctx, "PUT", calAPI+"/calendars/"+url.PathEscape(calendarID)+"/events/"+url.PathEscape(e.ID), toAPI(e), &a)
	return fromAPI(a), err
}

func (g *Google) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	err := g.do(ctx, "DELETE", calAPI+"/calendars/"+url.PathEscape(calendarID)+"/events/"+url.PathEscape(eventID), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil // уже удалено руками — цель достигнута
	}
	return err
}

func (g *Google) FindTaskList(ctx context.Context, title string) (string, error) {
	var resp struct {
		Items []struct{ ID, Title string }
	}
	if err := g.do(ctx, "GET", tasksAPI+"/users/@me/lists?maxResults=100", nil, &resp); err != nil {
		return "", err
	}
	for _, l := range resp.Items {
		if l.Title == title {
			return l.ID, nil
		}
	}
	return "", ErrNotFound
}

func (g *Google) CreateTaskList(ctx context.Context, title string) (string, error) {
	var resp struct{ ID string }
	err := g.do(ctx, "POST", tasksAPI+"/users/@me/lists", map[string]string{"title": title}, &resp)
	return resp.ID, err
}

func (g *Google) OpenTasks(ctx context.Context, listID string) ([]Task, error) {
	var resp struct {
		Items []struct{ ID, Title, Notes, Due, Status string }
	}
	if err := g.do(ctx, "GET", tasksAPI+"/lists/"+url.PathEscape(listID)+"/tasks?showCompleted=false&maxResults=100", nil, &resp); err != nil {
		return nil, err
	}
	var out []Task
	for _, t := range resp.Items {
		if t.Status != "completed" && strings.TrimSpace(t.Title) != "" {
			out = append(out, Task{ID: t.ID, Title: t.Title, Notes: t.Notes, Due: t.Due})
		}
	}
	return out, nil
}

func (g *Google) CompleteTask(ctx context.Context, listID, taskID string) error {
	return g.do(ctx, "PATCH", tasksAPI+"/lists/"+url.PathEscape(listID)+"/tasks/"+url.PathEscape(taskID),
		map[string]string{"status": "completed"}, nil)
}
