package bot

import (
	"context"
	"strings"
	"testing"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/files"
	"gitlab.com/KosovAndrey/lifeplan/internal/parse"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
)

func fileBlocks(it domain.Item) []domain.Block {
	var out []domain.Block
	for _, b := range it.Body {
		if b["type"] == "file" {
			out = append(out, b)
		}
	}
	return out
}

func TestFilesFromTelegram(t *testing.T) {
	ctx := context.Background()
	b, tg, _, st := setup(t, func(in parse.Input) (parse.Result, error) {
		return parse.Result{Intent: parse.IntentTask, Title: "Оплатить квитанцию", Sphere: ptr("home"), Confident: true}, nil
	})
	b.Files = files.New(t.TempDir(), nil)

	// Фото с подписью: подпись разбирается, файл ждёт принятия карточки.
	b.Handle(ctx, Incoming{ChatID: owner, MessageID: 1, Text: "оплатить квитанцию",
		File: &File{ID: "f1", Name: "photo.jpg", Mime: "image/jpeg"}})
	card := tg.last()
	if !strings.Contains(card.text, "📎 photo.jpg") || !strings.Contains(card.text, "Оплатить квитанцию") {
		t.Fatalf("карточка: %q", card.text)
	}
	// Ещё один файл ответом на ту же (не принятую) карточку.
	b.Handle(ctx, Incoming{ChatID: owner, MessageID: 2, ReplyTo: card.id, File: &File{ID: "f2", Name: "scan.pdf"}})
	if !strings.Contains(tg.last().text, "когда примешь") {
		t.Fatalf("ответ файлом до принятия: %q", tg.last().text)
	}
	b.Handle(ctx, Incoming{ChatID: owner, CallbackID: "c1", CallbackData: button(t, card, "a:"), CallbackMsgID: card.id})
	items, _ := st.ListItems(ctx, store.Filter{})
	if len(items) != 1 || len(fileBlocks(items[0])) != 2 {
		t.Fatalf("вложения в задаче: %+v", items)
	}
	fid := fileBlocks(items[0])[0]["file_id"].(string)
	if blk, err := st.FileBlock(ctx, fid); err != nil || blk["name"] != "photo.jpg" {
		t.Fatalf("FileBlock: %v %v", blk, err)
	}

	// Ответ файлом на принятую карточку — сразу в задачу.
	b.Handle(ctx, Incoming{ChatID: owner, MessageID: 3, ReplyTo: card.id, File: &File{ID: "f3", Name: "чек.png"}})
	if !strings.Contains(tg.last().text, "Прикрепил «чек.png» к «Оплатить квитанцию»") {
		t.Fatalf("после принятия: %q", tg.last().text)
	}
	it, _ := st.GetItem(ctx, items[0].ID)
	if len(fileBlocks(it)) != 3 {
		t.Fatalf("файлов: %d", len(fileBlocks(it)))
	}

	// Файл без подписи — во входящие без разбора, с файлом.
	b.Handle(ctx, Incoming{ChatID: owner, MessageID: 4, File: &File{ID: "f4", Name: "doc.docx"}})
	if !strings.Contains(tg.last().text, "что это за файл") {
		t.Fatalf("без подписи: %q", tg.last().text)
	}
	inbox, _ := st.OpenInbox(ctx)
	if len(inbox) != 1 || len(inbox[0].Files) != 1 || inbox[0].Text != "📎 doc.docx" {
		t.Fatalf("входящие: %+v", inbox)
	}
}
