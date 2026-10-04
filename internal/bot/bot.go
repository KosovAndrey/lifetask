// Package bot — Telegram-инбокс. Любое сообщение сначала сохраняется во входящие
// (ничего не теряется, даже если ИИ недоступен), потом разбирается в черновик и
// показывается карточкой с кнопками. Ответ на карточку — уточнение и повторный разбор.
package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/changeplan"
	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/files"
	"gitlab.com/KosovAndrey/lifeplan/internal/parse"
	"gitlab.com/KosovAndrey/lifeplan/internal/render"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
	"gitlab.com/KosovAndrey/lifeplan/internal/stt"
)

type Button struct{ Text, Data string }

type Keyboard [][]Button

// Messenger — то, что боту нужно от Telegram (в тестах — фейк).
type Messenger interface {
	Send(chatID int64, text string, kb Keyboard) (int64, error)
	Edit(chatID, msgID int64, text string, kb Keyboard) error
	AnswerCallback(callbackID, text string) error
	Download(fileID string) ([]byte, error)
}

// File — вложение из Telegram.
type File struct {
	ID, Name, Mime string
}

// Incoming — апдейт Telegram в нужном боту виде.
type Incoming struct {
	ChatID    int64
	MessageID int64
	Text      string
	ReplyTo   int64 // id сообщения, на которое ответили
	VoiceID   string
	File      *File // фото или документ

	CallbackID    string
	CallbackData  string
	CallbackMsgID int64
}

type Bot struct {
	st     *store.Store
	plans  *changeplan.Service
	parser parse.Parser    // nil — без ИИ, всё копится во входящих до вечера
	stt    stt.Transcriber // nil — голос не расшифровывается
	m      Messenger
	owner  int64
	now    func() time.Time

	PublicURL string       // для /login: ссылка на веб
	Files     *files.Store // nil — файлы не принимаются
}

func New(st *store.Store, m Messenger, owner int64, parser parse.Parser, tr stt.Transcriber) *Bot {
	return &Bot{st: st, plans: changeplan.New(st), parser: parser, stt: tr, m: m, owner: owner, now: time.Now}
}

func (b *Bot) Handle(ctx context.Context, in Incoming) {
	if in.ChatID != b.owner {
		if b.owner == 0 {
			b.send(in.ChatID, fmt.Sprintf("Владелец не задан. Твой chat id: %d — пропиши TG_OWNER_ID и перезапусти.", in.ChatID), nil)
		} else {
			slog.Warn("tg: чужой чат", "chat", in.ChatID)
		}
		return
	}
	var err error
	switch {
	case in.CallbackID != "":
		err = b.onCallback(ctx, in)
	case in.File != nil:
		err = b.onFile(ctx, in)
	case in.VoiceID != "":
		err = b.onVoice(ctx, in)
	case strings.HasPrefix(in.Text, "/"):
		err = b.onCommand(ctx, in)
	case in.ReplyTo != 0:
		err = b.onReply(ctx, in)
	case strings.TrimSpace(in.Text) != "":
		err = b.onText(ctx, in)
	}
	if err != nil {
		slog.Error("tg: обработка", "err", err)
		b.send(in.ChatID, "⚠️ "+err.Error(), nil)
	}
}

func (b *Bot) send(chat int64, text string, kb Keyboard) int64 {
	id, err := b.m.Send(chat, text, kb)
	if err != nil {
		slog.Error("tg: send", "err", err)
	}
	return id
}

func (b *Bot) onText(ctx context.Context, in Incoming) error {
	tgID := in.MessageID
	m, err := b.st.AddInbox(ctx, domain.InboxMessage{Text: in.Text, TgMessageID: &tgID})
	if err != nil {
		return err
	}
	return b.process(ctx, m, nil, "")
}

func (b *Bot) onVoice(ctx context.Context, in Incoming) error {
	if b.stt == nil {
		return errors.New("расшифровка голоса не настроена (GROQ_API_KEY) — напиши текстом")
	}
	audio, err := b.m.Download(in.VoiceID)
	if err != nil {
		return fmt.Errorf("не скачал голосовое: %w", err)
	}
	text, err := b.stt.Transcribe(ctx, audio, "voice.ogg")
	if err != nil {
		return fmt.Errorf("не расшифровал голосовое: %w", err)
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("в голосовом не разобрал слов")
	}
	tgID := in.MessageID
	m, err := b.st.AddInbox(ctx, domain.InboxMessage{Transcript: &text, TgMessageID: &tgID})
	if err != nil {
		return err
	}
	return b.process(ctx, m, nil, "🎤 «"+text+"»\n\n")
}

// onFile: ответ файлом на карточку — вложение к её задаче (или к заметке, пока
// карточка не принята). Иначе — новая заметка: подпись разбирается как текст,
// без подписи файл ждёт во входящих, что это (ответом на карточку).
func (b *Bot) onFile(ctx context.Context, in Incoming) error {
	if b.Files == nil {
		return errors.New("хранилище файлов не настроено")
	}
	data, err := b.m.Download(in.File.ID)
	if err != nil {
		return fmt.Errorf("не скачал файл (боту доступны файлы до 20 МБ): %w", err)
	}
	block, err := b.Files.Put(ctx, in.File.Name, in.File.Mime, data)
	if err != nil {
		return err
	}
	name, _ := block["name"].(string)

	if in.ReplyTo != 0 {
		m, err := b.st.InboxByBotMessage(ctx, in.ReplyTo)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// не карточка — новая заметка
		case err != nil:
			return err
		case m.Status == "accepted" && m.ItemID != nil:
			var it domain.Item
			err := b.st.InTx(ctx, func(tx *store.Store) error {
				var err error
				it, err = tx.AttachFiles(ctx, *m.ItemID, []domain.Block{block}, "bot")
				return err
			})
			if err != nil {
				return err
			}
			b.send(in.ChatID, "📎 Прикрепил «"+name+"» к «"+it.Title+"».", nil)
			return nil
		case m.Status != "rejected":
			if err := b.st.AddInboxFile(ctx, m.ID, block); err != nil {
				return err
			}
			b.send(in.ChatID, "📎 «"+name+"» приложу к задаче, когда примешь карточку.", nil)
			return nil
		}
	}

	tgID := in.MessageID
	text := strings.TrimSpace(in.Text)
	if text == "" {
		m, err := b.st.AddInbox(ctx, domain.InboxMessage{Text: "📎 " + name, TgMessageID: &tgID, Files: []domain.Block{block}})
		if err != nil {
			return err
		}
		id := b.send(b.owner, "📎 «"+name+"» во входящих.\n\n↩ Ответь на это сообщение, что это за файл или к какой задаче, — оформлю.", nil)
		return b.st.SaveParse(ctx, m.ID, nil, nil, &id, "new")
	}
	m, err := b.st.AddInbox(ctx, domain.InboxMessage{Text: text, TgMessageID: &tgID, Files: []domain.Block{block}})
	if err != nil {
		return err
	}
	return b.process(ctx, m, nil, "📎 "+name+"\n")
}

// onReply — ответ на карточку = уточнение разбора. Ответ на что-то другое — новая заметка.
func (b *Bot) onReply(ctx context.Context, in Incoming) error {
	// Ответ на бриф — запись в дневник (вечерний бриф — про прошедший день).
	if d, kind, ok, err := b.st.BriefByMessage(ctx, in.ReplyTo); err != nil {
		return err
	} else if ok {
		if kind == "evening" {
			d = d.AddDays(-1)
		}
		text := in.Text
		if _, err := b.st.SaveJournal(ctx, d, store.JournalPatch{AppendText: &text}); err != nil {
			return err
		}
		b.send(in.ChatID, "📔 Записал в дневник за "+render.DayLabel(d.Time)+".", nil)
		return nil
	}
	m, err := b.st.InboxByBotMessage(ctx, in.ReplyTo)
	if errors.Is(err, store.ErrNotFound) {
		return b.onText(ctx, in)
	}
	if err != nil {
		return err
	}
	if m.Status == "accepted" || m.Status == "rejected" {
		b.send(in.ChatID, "Эта заметка уже разобрана — пишу как новую.", nil)
		return b.onText(ctx, in)
	}
	var prev *parse.Result
	if len(m.Parsed) > 0 {
		var r parse.Result
		if json.Unmarshal(m.Parsed, &r) == nil {
			prev = &r
		}
	}
	_ = b.m.Edit(b.owner, in.ReplyTo, "↪ уточнено ниже", nil)
	m.Text = m.Content() + "\nУточнение: " + in.Text
	m.Transcript = nil
	return b.process(ctx, m, prev, "")
}

// process разбирает входящее и показывает карточку. Ошибка разбора не теряет
// заметку: она остаётся во входящих до вечернего разбора.
func (b *Bot) process(ctx context.Context, m domain.InboxMessage, prev *parse.Result, prefix string) error {
	if b.parser == nil {
		id := b.send(b.owner, prefix+"📥 Записал во входящие — разберём вечером.", nil)
		return b.st.SaveParse(ctx, m.ID, nil, nil, &id, "new")
	}
	in := parse.Input{Text: m.Content(), Now: b.now(), Previous: prev}
	var err error
	if in.Spheres, err = b.st.Spheres(ctx); err != nil {
		return err
	}
	cands, err := b.st.TimeCandidates(ctx, 30)
	if err != nil {
		return err
	}
	for _, c := range cands {
		in.Candidates = append(in.Candidates, parse.Candidate{ID: c.ID, Title: c.Title})
	}

	res, err := b.parser.Parse(ctx, in)
	if err != nil {
		slog.Error("parse", "err", err)
		msg := err.Error()
		id := b.send(b.owner, prefix+"📥 Записал, но разобрать не вышло — разберём вечером.", nil)
		return b.st.SaveParse(ctx, m.ID, nil, &msg, &id, "new")
	}
	parsed, _ := json.Marshal(res)

	var lines []string
	ops, opsErr := res.Ops(m.ID)
	if opsErr == nil {
		lines, opsErr = b.plans.Preview(ctx, "bot", ops)
	}
	var text strings.Builder
	text.WriteString(prefix)
	kb := Keyboard{{{"📥 На вечер", "d:" + m.ID}, {"✗ Удалить", "x:" + m.ID}}}
	if opsErr != nil {
		text.WriteString("🤔 Не смог оформить: " + opsErr.Error())
	} else {
		text.WriteString(cardTitle(res.Intent) + "\n")
		for _, l := range lines {
			if !strings.HasPrefix(l, "📥") {
				text.WriteString(l + "\n")
			}
		}
		kb = Keyboard{{{"✅ Добавить", "a:" + m.ID}}, kb[0]}
	}
	if !res.Confident && res.Question != nil {
		text.WriteString("\n❓ " + *res.Question)
	}
	text.WriteString("\n\n↩ Ответь на это сообщение, чтобы поправить.")
	id := b.send(b.owner, text.String(), kb)
	return b.st.SaveParse(ctx, m.ID, parsed, nil, &id, "proposed")
}

var moods = map[int]string{1: "😞", 2: "😕", 3: "😐", 4: "🙂", 5: "😄"}

func cardTitle(i parse.Intent) string {
	switch i {
	case parse.IntentEvent:
		return "📅 Событие"
	case parse.IntentNote:
		return "🗒 Заметка"
	case parse.IntentTimeLog:
		return "⏱ Время"
	}
	return "📝 Задача"
}

func (b *Bot) onCallback(ctx context.Context, in Incoming) error {
	action, id, ok := strings.Cut(in.CallbackData, ":")
	if !ok {
		return b.m.AnswerCallback(in.CallbackID, "?")
	}
	if action == "m" { // настроение из вечернего брифа: m:<дата>:<1..5>
		ds, ms, _ := strings.Cut(id, ":")
		d, err := domain.ParseDate(ds)
		mood, convErr := strconv.Atoi(ms)
		if err != nil || convErr != nil {
			return b.m.AnswerCallback(in.CallbackID, "?")
		}
		if _, err := b.st.SaveJournal(ctx, d, store.JournalPatch{Mood: &mood}); err != nil {
			_ = b.m.AnswerCallback(in.CallbackID, "ошибка")
			return err
		}
		return b.m.AnswerCallback(in.CallbackID, "настроение записано "+moods[mood])
	}
	m, err := b.st.GetInbox(ctx, id)
	if err != nil {
		_ = b.m.AnswerCallback(in.CallbackID, "не нашёл")
		return err
	}
	if m.Status == "accepted" || m.Status == "rejected" {
		return b.m.AnswerCallback(in.CallbackID, "уже разобрано")
	}
	switch action {
	case "a":
		var res parse.Result
		if err := json.Unmarshal(m.Parsed, &res); err != nil {
			_ = b.m.AnswerCallback(in.CallbackID, "нет черновика")
			return err
		}
		ops, err := res.Ops(m.ID)
		if err != nil {
			_ = b.m.AnswerCallback(in.CallbackID, "не оформить")
			return err
		}
		p, err := b.plans.ApplyNow(ctx, "bot", "TG: "+truncate(m.Content(), 80), ops)
		if err != nil {
			_ = b.m.AnswerCallback(in.CallbackID, "ошибка")
			return err
		}
		var lines []string
		for _, l := range p.Preview {
			if !strings.HasPrefix(l, "📥") {
				lines = append(lines, l)
			}
		}
		_ = b.m.AnswerCallback(in.CallbackID, "добавлено")
		return b.m.Edit(b.owner, in.CallbackMsgID, "✅ Добавлено\n"+strings.Join(lines, "\n"), nil)
	case "d":
		if err := b.st.ResolveInbox(ctx, m.ID, "deferred", nil); err != nil {
			return err
		}
		_ = b.m.AnswerCallback(in.CallbackID, "на вечер")
		return b.m.Edit(b.owner, in.CallbackMsgID, "📥 На вечерний разбор: "+m.Content(), nil)
	case "x":
		if err := b.st.ResolveInbox(ctx, m.ID, "rejected", nil); err != nil {
			return err
		}
		_ = b.m.AnswerCallback(in.CallbackID, "удалено")
		return b.m.Edit(b.owner, in.CallbackMsgID, "✗ "+m.Content(), nil)
	}
	return b.m.AnswerCallback(in.CallbackID, "?")
}

const help = `Пиши или наговаривай что угодно — задачу, встречу, мысль, «уборка 40м».
Я разберу и покажу карточку: ✅ добавить, 📥 оставить на вечер, ✗ удалить.
Ответь на карточку, чтобы поправить разбор.

/today — сегодня
/tomorrow — завтра
/inbox — что ждёт разбора
/d текст — запись в дневник
/login — ссылка для входа в веб`

func (b *Bot) onCommand(ctx context.Context, in Incoming) error {
	cmd, _, _ := strings.Cut(strings.TrimSpace(in.Text), " ")
	cmd, _, _ = strings.Cut(cmd, "@") // /today@lifetask_bot в группах
	switch cmd {
	case "/start", "/help":
		b.send(in.ChatID, help, nil)
	case "/today", "/tomorrow":
		d := domain.Today()
		if cmd == "/tomorrow" {
			d = d.AddDays(1)
		}
		day, err := b.st.Day(ctx, d)
		if err != nil {
			return err
		}
		o, err := b.opts(ctx)
		if err != nil {
			return err
		}
		b.send(in.ChatID, render.Day(day, cmd == "/today", o), nil)
	case "/d", "/diary":
		_, text, _ := strings.Cut(strings.TrimSpace(in.Text), " ")
		if strings.TrimSpace(text) == "" {
			b.send(in.ChatID, "Напиши так: /d текст — допишу в дневник за сегодня.", nil)
			return nil
		}
		d := b.journalDay()
		if _, err := b.st.SaveJournal(ctx, d, store.JournalPatch{AppendText: &text}); err != nil {
			return err
		}
		b.send(in.ChatID, "📔 Записал в дневник за "+render.DayLabel(d.Time)+".", nil)
	case "/login":
		if b.PublicURL == "" {
			b.send(in.ChatID, "Веб не настроен: нет PUBLIC_URL.", nil)
			return nil
		}
		tok, err := b.st.NewLoginToken(ctx)
		if err != nil {
			return err
		}
		b.send(in.ChatID, "🔑 Вход в LifeTask — ссылка одноразовая, действует 10 минут:\n"+b.PublicURL+"/login?t="+tok, nil)
	case "/inbox":
		msgs, err := b.st.OpenInbox(ctx)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			b.send(in.ChatID, "📥 Входящие пусты", nil)
			return nil
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "📥 Ждут разбора: %d\n", len(msgs))
		for _, m := range msgs {
			sb.WriteString("• " + truncate(m.Content(), 120) + "\n")
		}
		b.send(in.ChatID, sb.String(), nil)
	default:
		b.send(in.ChatID, help, nil)
	}
	return nil
}

// opts — подписи сфер иконками: в телефоне короче и нагляднее slug.
func (b *Bot) opts(ctx context.Context) (render.Opts, error) {
	ss, err := b.st.Spheres(ctx)
	if err != nil {
		return render.Opts{}, err
	}
	names := make(map[int]string, len(ss))
	for _, s := range ss {
		names[s.ID] = s.Icon
	}
	return render.Opts{Spheres: names}, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
