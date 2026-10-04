// lifeplan — сервер планировщика.
//
//	lifeplan migrate      — применить миграции
//	lifeplan serve        — миграции + HTTP API + бот, брифы, повторы, синк с Google
//	lifeplan google-auth  — один раз: выдать доступ к Google Calendar/Tasks
//
// Окружение: DATABASE_URL, API_TOKEN, LISTEN_ADDR (127.0.0.1:8090 или unix:/путь).
// PUBLIC_URL — адрес веба (https://lifetask.ru) для ссылок входа из бота.
// Бот (необязательно): TG_TOKEN, TG_OWNER_ID, ANTHROPIC_API_KEY, GROQ_API_KEY;
// AI_PROXY_URL — прокси для Anthropic/Groq (из РФ напрямую недоступны), TG_PROXY_URL — для Telegram.
// Google (необязательно): GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, GOOGLE_PROXY_URL; refresh-токен — в БД после google-auth.
// Файлы: в Google Drive (папка LifeTask), без Drive — в FILES_DIR (по умолчанию ./files).
// BACKUP_DIR — откуда брать дампы базы для копии в Drive (в docker: /backups).
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // Europe/Moscow в distroless/alpine-образе

	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/lifeplan/internal/api"
	"gitlab.com/KosovAndrey/lifeplan/internal/bot"
	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/files"
	"gitlab.com/KosovAndrey/lifeplan/internal/gcal"
	"gitlab.com/KosovAndrey/lifeplan/internal/migrate"
	"gitlab.com/KosovAndrey/lifeplan/internal/parse"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
	"gitlab.com/KosovAndrey/lifeplan/internal/stt"
	"gitlab.com/KosovAndrey/lifeplan/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL не задан")
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	applied, err := migrate.Up(ctx, pool)
	if err != nil {
		return fmt.Errorf("миграции: %w", err)
	}
	for _, name := range applied {
		slog.Info("migration applied", "name", name)
	}

	switch cmd {
	case "migrate":
		return nil
	case "serve":
		return serve(ctx, pool)
	case "google-auth":
		return googleAuth(ctx, store.New(pool))
	default:
		return fmt.Errorf("неизвестная команда %q (migrate | serve | google-auth)", cmd)
	}
}

func serve(ctx context.Context, pool *pgxpool.Pool) error {
	token := os.Getenv("API_TOKEN")
	if len(token) < 24 {
		return errors.New("API_TOKEN не задан или короче 24 символов")
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8090"
	}
	st := store.New(pool)
	publicURL := strings.TrimRight(os.Getenv("PUBLIC_URL"), "/")
	go runRecurrences(ctx, st)
	g, err := startGoogle(ctx, st)
	if err != nil {
		return err
	}
	fs := startFiles(ctx, g)
	if err := startBot(ctx, st, publicURL, fs); err != nil {
		return err
	}
	a := api.New(st, token)
	a.Static = web.Handler()
	a.Secure = strings.HasPrefix(publicURL, "https://")
	a.Files = fs
	srv := &http.Server{
		Addr:              addr,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	// LISTEN_ADDR=unix:/path — сокет вместо TCP (nginx → сокет, локальная разработка в WSL).
	network, address := "tcp", addr
	if path, ok := strings.CutPrefix(addr, "unix:"); ok {
		network, address = "unix", path
		_ = os.Remove(path)
	}
	ln, err := net.Listen(network, address)
	if err != nil {
		return err
	}
	if network == "unix" {
		_ = os.Chmod(address, 0o660)
	}
	slog.Info("listening", "addr", addr)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// startBot поднимает Telegram-бота и брифы, если задан TG_TOKEN. Без ключей ИИ
// бот всё равно работает — просто складывает всё во входящие до вечера.
func startBot(ctx context.Context, st *store.Store, publicURL string, fs *files.Store) error {
	tgToken := os.Getenv("TG_TOKEN")
	if tgToken == "" {
		slog.Info("TG_TOKEN не задан — бот выключен")
		return nil
	}
	owner, _ := strconv.ParseInt(os.Getenv("TG_OWNER_ID"), 10, 64)

	aiClient, err := httpClient(os.Getenv("AI_PROXY_URL"), 60*time.Second)
	if err != nil {
		return fmt.Errorf("AI_PROXY_URL: %w", err)
	}
	// Без ключа — разбор по правилам; с ключом — Claude, а правила как запасной.
	var parser parse.Parser = parse.Rules{}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		parser = parse.Fallback{Primary: parse.NewClaude(key, aiClient), Secondary: parse.Rules{}}
	}
	var tr stt.Transcriber
	if key := os.Getenv("GROQ_API_KEY"); key != "" {
		tr = stt.NewGroq(key, aiClient)
	}

	tgClient, err := httpClient(os.Getenv("TG_PROXY_URL"), 70*time.Second)
	if err != nil {
		return fmt.Errorf("TG_PROXY_URL: %w", err)
	}
	tg, err := bot.NewTelegram(tgToken, tgClient)
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	b := bot.New(st, tg, owner, parser, tr)
	b.PublicURL = publicURL
	b.Files = fs
	go tg.Run(ctx, b)
	go b.RunBriefs(ctx)
	slog.Info("бот", "owner_set", owner != 0, "parser", parser != nil, "voice", tr != nil)
	return nil
}

func httpClient(proxy string, timeout time.Duration) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Timeout: timeout, Transport: tr}, nil
}

// runRecurrences держит экземпляры повторов на RecurHorizonDays вперёд: при старте
// и раз в час (после полуночи горизонт сдвигается на день).
func runRecurrences(ctx context.Context, st *store.Store) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		err := st.InTx(ctx, func(tx *store.Store) error {
			n, err := tx.GenerateRecurrences(ctx, domain.Today().AddDays(store.RecurHorizonDays))
			if n > 0 {
				slog.Info("повторы: созданы экземпляры", "n", n)
			}
			return err
		})
		if err != nil && ctx.Err() == nil {
			slog.Error("повторы", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

const kvGoogleRefresh = "google_refresh_token"

func googleOAuth() (gcal.OAuth, error) {
	id, secret := os.Getenv("GOOGLE_CLIENT_ID"), os.Getenv("GOOGLE_CLIENT_SECRET")
	if id == "" || secret == "" {
		return gcal.OAuth{}, errors.New("GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET не заданы")
	}
	client, err := httpClient(os.Getenv("GOOGLE_PROXY_URL"), 30*time.Second)
	if err != nil {
		return gcal.OAuth{}, err
	}
	return gcal.OAuth{ClientID: id, ClientSecret: secret, HTTP: client}, nil
}

// googleAuth — разовая выдача доступа: открыть ссылку, разрешить, браузер уйдёт на
// localhost:8765 (страница не откроется — так и надо), скопировать адрес из строки браузера сюда.
func googleAuth(ctx context.Context, st *store.Store) error {
	o, err := googleOAuth()
	if err != nil {
		return err
	}
	fmt.Println("1. Открой ссылку и разреши доступ:\n\n" + o.AuthURL())
	fmt.Print("\n2. Вставь адрес, на который перекинул браузер (http://localhost:8765/?code=...):\n> ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return err
	}
	refresh, err := o.Exchange(ctx, line)
	if err != nil {
		return err
	}
	if err := st.KVSet(ctx, kvGoogleRefresh, refresh); err != nil {
		return err
	}
	fmt.Println("✅ Доступ сохранён. Перезапусти сервер: синк стартует сам.")
	return nil
}

// startGoogle запускает синк календаря и задач; возвращает клиент (nil — Google выключен).
func startGoogle(ctx context.Context, st *store.Store) (*gcal.Google, error) {
	o, err := googleOAuth()
	if err != nil {
		slog.Info("Google выключен", "reason", err.Error())
		return nil, nil
	}
	refresh, err := st.KVGet(ctx, kvGoogleRefresh)
	if err != nil {
		return nil, err
	}
	if refresh == "" {
		slog.Info("Google: нет доступа — выполни `lifeplan google-auth`")
		return nil, nil
	}
	g := gcal.NewGoogle(o, refresh)
	go gcal.NewSyncer(st, g).Run(ctx, 5*time.Minute)
	slog.Info("Google: синк каждые 5 минут")
	return g, nil
}

// startFiles — хранилище вложений; с Google — ещё и копия бэкапов в Drive.
func startFiles(ctx context.Context, g *gcal.Google) *files.Store {
	dir := os.Getenv("FILES_DIR")
	if dir == "" {
		dir = "files"
	}
	if g == nil {
		slog.Info("файлы: на диске", "dir", dir)
		return files.New(dir, nil)
	}
	fs := files.New(dir, driveAdapter{g})
	if bdir := os.Getenv("BACKUP_DIR"); bdir != "" {
		go fs.RunBackups(ctx, bdir, 6*time.Hour)
	}
	slog.Info("файлы: Google Drive", "fallback_dir", dir, "backups", os.Getenv("BACKUP_DIR") != "")
	return fs
}

// driveAdapter приводит список файлов gcal к типу files (пакеты не зависят друг от друга).
type driveAdapter struct{ *gcal.Google }

func (d driveAdapter) DriveList(ctx context.Context, folder string) ([]files.DriveFileInfo, error) {
	list, err := d.Google.DriveList(ctx, folder)
	out := make([]files.DriveFileInfo, len(list))
	for i, f := range list {
		out[i] = files.DriveFileInfo{ID: f.ID, Name: f.Name}
	}
	return out, err
}
