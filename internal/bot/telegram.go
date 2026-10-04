package bot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// Telegram — Messenger поверх Bot API (long polling: вебхук и домен не нужны).
type Telegram struct {
	api    *tgbotapi.BotAPI
	client *http.Client
}

func NewTelegram(token string, client *http.Client) (*Telegram, error) {
	if client == nil {
		client = &http.Client{Timeout: 70 * time.Second}
	}
	api, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, client)
	if err != nil {
		return nil, err
	}
	return &Telegram{api: api, client: client}, nil
}

func markup(kb Keyboard) *tgbotapi.InlineKeyboardMarkup {
	if len(kb) == 0 {
		return nil
	}
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(kb))
	for _, r := range kb {
		row := make([]tgbotapi.InlineKeyboardButton, 0, len(r))
		for _, b := range r {
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(b.Text, b.Data))
		}
		rows = append(rows, row)
	}
	m := tgbotapi.NewInlineKeyboardMarkup(rows...)
	return &m
}

func (t *Telegram) Send(chatID int64, text string, kb Keyboard) (int64, error) {
	msg := tgbotapi.NewMessage(chatID, text)
	if m := markup(kb); m != nil {
		msg.ReplyMarkup = *m
	}
	sent, err := t.api.Send(msg)
	return int64(sent.MessageID), err
}

func (t *Telegram) Edit(chatID, msgID int64, text string, kb Keyboard) error {
	edit := tgbotapi.NewEditMessageText(chatID, int(msgID), text)
	edit.ReplyMarkup = markup(kb) // nil убирает кнопки
	_, err := t.api.Request(edit)
	return err
}

func (t *Telegram) AnswerCallback(id, text string) error {
	_, err := t.api.Request(tgbotapi.NewCallback(id, text))
	return err
}

func (t *Telegram) Download(fileID string) ([]byte, error) {
	url, err := t.api.GetFileDirectURL(fileID)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("скачивание файла: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 20<<20))
}

// Run — цикл long polling. Апдейты обрабатываются по одному: так порядок
// заметок и кнопок сохраняется, а нагрузка у одного пользователя мизерная.
func (t *Telegram) Run(ctx context.Context, b *Bot) {
	cfg := tgbotapi.NewUpdate(0)
	cfg.Timeout = 50
	cfg.AllowedUpdates = []string{"message", "callback_query"}
	updates := t.api.GetUpdatesChan(cfg)
	slog.Info("tg: бот запущен", "username", t.api.Self.UserName)
	for {
		select {
		case <-ctx.Done():
			t.api.StopReceivingUpdates()
			return
		case u := <-updates:
			if in, ok := convert(u); ok {
				hctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				b.Handle(hctx, in)
				cancel()
			}
		}
	}
}

func convert(u tgbotapi.Update) (Incoming, bool) {
	switch {
	case u.CallbackQuery != nil:
		q := u.CallbackQuery
		in := Incoming{CallbackID: q.ID, CallbackData: q.Data}
		if q.Message != nil {
			in.ChatID = q.Message.Chat.ID
			in.CallbackMsgID = int64(q.Message.MessageID)
		}
		return in, true
	case u.Message != nil:
		m := u.Message
		in := Incoming{ChatID: m.Chat.ID, MessageID: int64(m.MessageID), Text: m.Text}
		if m.Text == "" {
			in.Text = m.Caption
		}
		if m.ReplyToMessage != nil {
			in.ReplyTo = int64(m.ReplyToMessage.MessageID)
		}
		switch {
		case len(m.Photo) > 0:
			// Telegram присылает несколько размеров — берём самый крупный.
			ph := m.Photo[len(m.Photo)-1]
			in.File = &File{ID: ph.FileID, Name: "photo_" + time.Unix(int64(m.Date), 0).In(domain.MSK).Format("2006-01-02_15-04-05") + ".jpg", Mime: "image/jpeg"}
		case m.Document != nil:
			in.File = &File{ID: m.Document.FileID, Name: m.Document.FileName, Mime: m.Document.MimeType}
		case m.Video != nil:
			in.File = &File{ID: m.Video.FileID, Name: m.Video.FileName, Mime: m.Video.MimeType}
		case m.Voice != nil:
			in.VoiceID = m.Voice.FileID
		case m.Audio != nil:
			in.VoiceID = m.Audio.FileID
		}
		return in, true
	}
	return Incoming{}, false
}
