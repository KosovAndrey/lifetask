package parse

import "testing"

func TestRecurOps(t *testing.T) {
	rule := "FREQ=WEEKLY;BYDAY=TU,TH"
	r := Result{Intent: IntentTask, Title: "Спорт", RRule: &rule,
		StartAt: ptr("2026-10-06T19:00:00+03:00"), EndAt: ptr("2026-10-06T20:30:00+03:00"), Confident: true}
	ops, err := r.Ops("in-1")
	if err != nil {
		t.Fatal(err)
	}
	op := ops[0]
	if op.Op != "recur" || op.Start != "2026-10-06" || op.Time != "19:00" || op.DurationMin != 90 {
		t.Fatalf("recur: %+v", op)
	}
	if _, ok := op.Item["start_at"]; ok {
		t.Fatal("в шаблоне не должно быть дат")
	}
	if ops[1].Op != "inbox" || ops[1].To != "" {
		t.Fatalf("inbox: %+v", ops[1])
	}
}

func TestTimeLogWithoutItemCreatesDoneTask(t *testing.T) {
	r := Result{Intent: IntentTimeLog, Title: "Разбор почты", TimeMinutes: ptr(25)}
	ops, err := r.Ops("in-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 || string(ops[0].Item["status"]) != `"done"` || ops[1].Minutes != 25 {
		t.Fatalf("ops: %+v", ops)
	}
}

func ptr[T any](v T) *T { return &v }
