package api_test

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gitlab.com/KosovAndrey/lifeplan/internal/api"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
	"gitlab.com/KosovAndrey/lifeplan/internal/testdb"
)

func TestAtomicIdempotency(t *testing.T) {
	pool := testdb.New(t)
	st := store.New(pool)
	h := api.New(st, token).Handler()
	post := func(path, body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// Simultaneous requests must return the same created object, not a synthetic success.
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 8)
	for i := range results {
		wg.Go(func() { results[i] = post("/api/items", `{"title":"one"}`, "concurrent") })
	}
	wg.Wait()
	for _, w := range results {
		if w.Code != 201 || w.Body.String() != results[0].Body.String() {
			t.Fatalf("replay: %d %s", w.Code, w.Body)
		}
	}
	if w := post("/api/items", `{"title":"different"}`, "concurrent"); w.Code != 409 {
		t.Fatalf("payload mismatch: %d %s", w.Code, w.Body)
	}
	// A late database failure must roll back both the object and key.
	_, err := pool.Exec(t.Context(), `CREATE FUNCTION reject_item_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test failure'; END $$;
		CREATE TRIGGER reject_item_event BEFORE INSERT ON item_events FOR EACH ROW EXECUTE FUNCTION reject_item_event()`)
	if err != nil {
		t.Fatal(err)
	}
	if w := post("/api/items", `{"title":"retry"}`, "retry"); w.Code < 500 {
		t.Fatalf("expected DB failure: %d %s", w.Code, w.Body)
	}
	items, err := st.ListItems(t.Context(), store.Filter{})
	if err != nil || len(items) != 1 {
		t.Fatalf("partial write: %d %v", len(items), err)
	}
	if _, err := pool.Exec(t.Context(), `DROP TRIGGER reject_item_event ON item_events`); err != nil {
		t.Fatal(err)
	}
	if w := post("/api/items", `{"title":"retry"}`, "retry"); w.Code != 201 {
		t.Fatalf("retry: %d %s", w.Code, w.Body)
	}
	// Validation failures remain retryable; corrected input may reuse uncommitted key.
	if w := post("/api/items", `{"title":""}`, "validation"); w.Code != 400 {
		t.Fatalf("validation: %d", w.Code)
	}
	if w := post("/api/items", `{"title":"corrected"}`, "validation"); w.Code != 201 {
		t.Fatalf("corrected: %d %s", w.Code, w.Body)
	}
	// Quick input uses dry-run inside the request transaction: savepoint rollback
	// must not accidentally retain the preview's created task.
	if w := post("/api/quick", `{"text":"прочитать книгу"}`, "quick"); w.Code != 200 {
		t.Fatalf("quick: %d %s", w.Code, w.Body)
	}
	items, err = st.ListItems(t.Context(), store.Filter{})
	if err != nil || len(items) != 4 {
		t.Fatalf("dry run leaked: %d %v", len(items), err)
	}
}
