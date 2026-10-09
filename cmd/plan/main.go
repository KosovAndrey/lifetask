// plan — CLI к API lifeplan для разборов с Claude Code. Читает — компактным
// текстом (экономит контекст), пишет — только через план изменений.
//
// Окружение: LIFEPLAN_URL (http://127.0.0.1:8090 или unix:/путь), LIFEPLAN_TOKEN;
// или файл ~/.config/lifeplan/env с теми же строками KEY=VALUE.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	_ "time/tzdata"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/render"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
)

const usage = `plan — разбор задач lifeplan

  plan day [today|tomorrow|YYYY-MM-DD]   день: встречи, задачи, дедлайны, просрочка, загрузка, самочувствие
  plan week [дата]                        неделя с понедельника
  plan inbox                              неразобранные входящие
  plan list [open] [sphere=<slug>] [q=<текст>] [status=a,b]
  plan show <id>                          задача целиком (JSON)
  plan spheres                            сферы и проекты
  plan recur                              активные повторы
  plan stats [from] [to]                  аналитика (JSON), по умолчанию 7 дней
  plan propose <file.json | ->            предложить план → превью и id
  plan apply <plan-id>                    применить план
  plan reject <plan-id>                   отклонить план
  plan brief <evening|morning> <дата> <file | ->   свой текст брифа (уйдёт в 21:00 / 07:00)
  plan journal [дата]                     дневник дня: настроение, самочувствие, текст, итог, что сделано
  plan health [дней|from to]              сон, заряд, стресс, шаги по дням + настроение и сделанное (по умолчанию 7 дней)
  plan journal-summary <дата> <file | ->  записать итог дня (после разбора)
  plan get <путь>                         сырой GET /api/<путь>

Формат плана: {"summary": "...", "ops": [...]}, операции — docs/PLAN-OPS.md.`

var (
	baseURL = "http://127.0.0.1:8090"
	token   string
)

func main() {
	loadEnv()
	if len(os.Args) < 2 {
		fmt.Println(usage)
		return
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "day":
		err = cmdDay(arg(args, 0, "today"))
	case "week":
		err = cmdWeek(arg(args, 0, "today"))
	case "inbox":
		err = cmdInbox()
	case "list":
		err = cmdList(args)
	case "show":
		err = rawGet("items/" + arg(args, 0, ""))
	case "spheres":
		err = cmdSpheres()
	case "recur":
		err = cmdRecur()
	case "stats":
		q := ""
		if len(args) > 0 {
			q = "?from=" + args[0]
			if len(args) > 1 {
				q += "&to=" + args[1]
			}
		}
		err = rawGet("stats" + q)
	case "propose":
		err = cmdPropose(arg(args, 0, "-"))
	case "apply":
		err = cmdApply(arg(args, 0, ""))
	case "reject":
		err = call("POST", "plans/"+arg(args, 0, "")+"/reject", nil, nil)
		if err == nil {
			fmt.Println("отклонён")
		}
	case "brief":
		err = cmdBrief(arg(args, 0, ""), arg(args, 1, "tomorrow"), arg(args, 2, "-"))
	case "journal":
		err = cmdJournal(arg(args, 0, "today"))
	case "health":
		err = cmdHealth(args)
	case "journal-summary":
		var raw []byte
		if raw, err = readInput(arg(args, 1, "-")); err == nil {
			err = call("PATCH", "journal/"+arg(args, 0, "today"), map[string]string{"summary": strings.TrimSpace(string(raw))}, nil)
			if err == nil {
				fmt.Println("итог дня записан")
			}
		}
	case "get":
		err = rawGet(arg(args, 0, ""))
	default:
		fmt.Println(usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func arg(a []string, i int, def string) string {
	if len(a) > i {
		return a[i]
	}
	return def
}

func loadEnv() {
	if home, err := os.UserHomeDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(home, ".config", "lifeplan", "env")); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
				if ok && os.Getenv(k) == "" {
					os.Setenv(k, strings.Trim(v, `"'`))
				}
			}
		}
	}
	if v := os.Getenv("LIFEPLAN_URL"); v != "" {
		baseURL = strings.TrimRight(v, "/")
	}
	token = os.Getenv("LIFEPLAN_TOKEN")
}

func call(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	base := baseURL
	// LIFEPLAN_URL=unix:/путь — ходим в сокет сервера.
	if sock, ok := strings.CutPrefix(baseURL, "unix:"); ok {
		base = "http://lifeplan"
		client.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}
	}
	req, err := http.NewRequest(method, base+"/api/"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s: %s", resp.Status, e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, raw)
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func rawGet(path string) error {
	var v any
	if err := call("GET", path, nil, &v); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// ── Компактный текст ─────────────────────────────────────────────────────────

var sphereNames map[int]string

func opts() render.Opts {
	if sphereNames == nil {
		sphereNames = map[int]string{}
		var ss []domain.Sphere
		if call("GET", "spheres", nil, &ss) == nil {
			for _, s := range ss {
				sphereNames[s.ID] = s.Slug
			}
		}
	}
	return render.Opts{IDs: true, Spheres: sphereNames}
}

// cmdDay — день из /api/plan: к самому дню добавляются загрузка (что не влезает)
// и самочувствие из Garmin — на разборе видно, на что хватит сил.
func cmdDay(date string) error {
	var p store.DayPlan
	if err := call("GET", "plan/"+date, nil, &p); err != nil {
		return err
	}
	fmt.Print(render.Day(p.Day, true, opts()))
	if h := render.Health(p.Health); h != "" {
		fmt.Println("  самочувствие: " + h)
	}
	fmt.Print(render.Load(p, opts()))
	return nil
}

func cmdWeek(date string) error {
	var days []store.Day
	if err := call("GET", "week/"+date, nil, &days); err != nil {
		return err
	}
	for _, d := range days {
		fmt.Print(render.Day(d, false, opts()))
	}
	return nil
}

func cmdInbox() error {
	var msgs []struct {
		ID         string
		Text       string
		Transcript *string
		Status     string
		CreatedAt  time.Time `json:"created_at"`
	}
	if err := call("GET", "inbox", nil, &msgs); err != nil {
		return err
	}
	if len(msgs) == 0 {
		fmt.Println("инбокс пуст")
		return nil
	}
	for _, m := range msgs {
		text := m.Text
		if m.Transcript != nil {
			text = "🎤 " + *m.Transcript
		}
		fmt.Printf("• %s [%s] %s  #%s\n", m.CreatedAt.In(domain.MSK).Format("02.01 15:04"), m.Status, text, m.ID)
	}
	return nil
}

func cmdList(args []string) error {
	q := []string{}
	for _, a := range args {
		if a == "open" {
			q = append(q, "open=1")
		} else if k, v, ok := strings.Cut(a, "="); ok {
			q = append(q, k+"="+v)
		}
	}
	var items []domain.Item
	if err := call("GET", "items?"+strings.Join(q, "&"), nil, &items); err != nil {
		return err
	}
	for _, it := range items {
		fmt.Println(render.Line(it, true, opts()))
	}
	fmt.Printf("— %d шт.\n", len(items))
	return nil
}

func cmdSpheres() error {
	var ss []struct {
		ID         int
		Slug, Name string
		Icon       string
	}
	if err := call("GET", "spheres", nil, &ss); err != nil {
		return err
	}
	for _, s := range ss {
		fmt.Printf("%s %-8s %s\n", s.Icon, s.Slug, s.Name)
	}
	var ps []struct {
		ID       int
		ParentID *int `json:"parent_id"`
		Name     string
	}
	if err := call("GET", "projects", nil, &ps); err != nil {
		return err
	}
	if len(ps) > 0 {
		fmt.Println("проекты:")
		for _, p := range ps {
			fmt.Printf("  %d %s\n", p.ID, p.Name)
		}
	}
	return nil
}

type planResp struct {
	ID      string
	Status  string
	Summary string
	Preview []string
	Refs    map[string]string
}

func printPlan(p planResp) {
	if p.Summary != "" {
		fmt.Println(p.Summary)
	}
	for _, l := range p.Preview {
		fmt.Println("  " + l)
	}
	fmt.Printf("план %s · %s\n", p.ID, p.Status)
}

func cmdPropose(path string) error {
	raw, err := readInput(path)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("план не JSON: %w", err)
	}
	var p planResp
	if err := call("POST", "plans", body, &p); err != nil {
		return err
	}
	printPlan(p)
	return nil
}

func cmdApply(id string) error {
	var p planResp
	if err := call("POST", "plans/"+id+"/apply", nil, &p); err != nil {
		return err
	}
	printPlan(p)
	return nil
}

func cmdBrief(kind, date, path string) error {
	raw, err := readInput(path)
	if err != nil {
		return err
	}
	if err := call("PUT", "briefs/"+date+"/"+kind, map[string]string{"body": string(raw)}, nil); err != nil {
		return err
	}
	fmt.Println("бриф сохранён:", kind, date)
	return nil
}

func readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func cmdRecur() error {
	var rs []store.Recurrence
	if err := call("GET", "recurrences", nil, &rs); err != nil {
		return err
	}
	if len(rs) == 0 {
		fmt.Println("повторов нет")
		return nil
	}
	for _, r := range rs {
		when := r.Human
		if r.StartTime != nil {
			when += " в " + *r.StartTime
		}
		fmt.Printf("🔁 %s — %s (с %s, %s)  #%s\n", r.Template.Title, when, r.Start, r.Rule, r.ID)
	}
	return nil
}

func cmdJournal(date string) error {
	var j store.Journal
	if err := call("GET", "journal/"+date, nil, &j); err != nil {
		return err
	}
	mood := "—"
	if j.Mood != nil {
		mood = map[int]string{1: "😞 1", 2: "😕 2", 3: "😐 3", 4: "🙂 4", 5: "😄 5"}[*j.Mood]
	}
	fmt.Printf("── дневник %s · настроение %s · учтено %s\n", j.Date, mood, render.Mins(j.Minutes))
	var hs []store.HealthDay
	if err := call("GET", "health?from="+j.Date.String()+"&to="+j.Date.AddDays(1).String(), nil, &hs); err == nil && len(hs) > 0 {
		fmt.Println("самочувствие: " + render.Health(&hs[0]))
	}
	if j.Text != "" {
		fmt.Println(j.Text)
	} else {
		fmt.Println("(записей нет)")
	}
	if j.Summary != "" {
		fmt.Println("итог:", j.Summary)
	}
	if len(j.Done) > 0 {
		fmt.Println("сделано:")
		for _, it := range j.Done {
			fmt.Println("  " + render.Line(it, false, opts()))
		}
	}
	return nil
}

// cmdHealth — самочувствие по дням рядом с настроением и сделанным: для недельного
// разбора («после короткого сна план срывается?»). Аргументы: число дней или from to.
func cmdHealth(args []string) error {
	to := domain.Today().AddDays(1)
	from := to.AddDays(-7)
	switch {
	case len(args) >= 2:
		f, err1 := domain.ParseDate(args[0])
		t, err2 := domain.ParseDate(args[1])
		if err1 != nil || err2 != nil {
			return fmt.Errorf("даты — YYYY-MM-DD")
		}
		from, to = f, t
	case len(args) == 1:
		var n int
		if _, err := fmt.Sscan(args[0], &n); err != nil || n < 1 || n > 366 {
			return fmt.Errorf("число дней — от 1 до 366")
		}
		from = to.AddDays(-n)
	}
	var st store.Stats
	if err := call("GET", "stats?from="+from.String()+"&to="+to.String(), nil, &st); err != nil {
		return err
	}
	health := map[string]*store.HealthDay{}
	for i := range st.Health {
		health[st.Health[i].Date.String()] = &st.Health[i]
	}
	moods := map[string]int{}
	for _, m := range st.Moods {
		moods[m.Date.String()] = m.Mood
	}
	fmt.Printf("── самочувствие %s — %s\n", from, to.AddDays(-1))
	var sleepSum, sleepN int
	for _, d := range st.Days {
		k := d.Date.String()
		line := render.Health(health[k])
		if line == "" {
			line = "нет данных"
		}
		if h := health[k]; h != nil && h.SleepMin != nil {
			sleepSum += *h.SleepMin
			sleepN++
		}
		mood := ""
		if m, ok := moods[k]; ok {
			mood = fmt.Sprintf(" · настроение %d", m)
		}
		plan := ""
		if all := d.PlanDone + d.PlanMissed + d.PlanMoved; all > 0 {
			plan = fmt.Sprintf(" · план %d/%d", d.PlanDone, all)
		}
		fmt.Printf("  %s  %s  | сделано %d%s%s\n", render.DayLabel(d.Date.Time), line, d.Done, plan, mood)
	}
	if sleepN > 0 {
		fmt.Printf("  средний сон %s по %d ночам\n", render.Mins(sleepSum/sleepN), sleepN)
	}
	return nil
}
