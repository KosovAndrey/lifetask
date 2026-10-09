package store_test

import (
	"encoding/json"
	"sync"
	"testing"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
	"gitlab.com/KosovAndrey/lifeplan/internal/testdb"
)

func TestConcurrentAttachments(t *testing.T) {
	st := store.New(testdb.New(t))
	it, err := st.CreateItem(t.Context(), domain.Item{Title: "files"}, "me")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			_, err := st.AttachFiles(t.Context(), it.ID, []domain.Block{{"type": "file", "file_id": "l:test"}}, "me")
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	it, err = st.GetItem(t.Context(), it.ID)
	if err != nil || len(it.Body) != 16 {
		t.Fatalf("attachments: %d %v", len(it.Body), err)
	}
}

func TestStoreMutationsRollbackOnJournalFailure(t *testing.T) {
	pool := testdb.New(t)
	st := store.New(pool)
	it, err := st.CreateItem(t.Context(), domain.Item{Title: "original"}, "me")
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(t.Context(), `CREATE FUNCTION reject_journal() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'journal failure'; END $$;
		CREATE TRIGGER reject_journal BEFORE INSERT ON item_events FOR EACH ROW EXECUTE FUNCTION reject_journal()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateItem(t.Context(), domain.Item{Title: "partial"}, "me"); err == nil {
		t.Fatal("expected create failure")
	}
	if _, err := st.UpdateItem(t.Context(), it.ID, store.Patch{"title": json.RawMessage(`"changed"`)}, "me"); err == nil {
		t.Fatal("expected update failure")
	}
	items, err := st.ListItems(t.Context(), store.Filter{})
	if err != nil || len(items) != 1 || items[0].Title != "original" {
		t.Fatalf("partial mutation: %+v %v", items, err)
	}
}
