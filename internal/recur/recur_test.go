package recur

import (
	"strings"
	"testing"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

func d(s string) domain.Date {
	x, err := domain.ParseDate(s)
	if err != nil {
		panic(err)
	}
	return x
}

func dates(ds []domain.Date) string {
	var out []string
	for _, x := range ds {
		out = append(out, x.String()[5:])
	}
	return strings.Join(out, " ")
}

func TestBetween(t *testing.T) {
	cases := []struct {
		rule, start, from, to, want string
	}{
		{"FREQ=WEEKLY;BYDAY=SA", "2026-10-03", "2026-10-01", "2026-10-20", "10-03 10-10 10-17"},
		{"FREQ=WEEKLY;BYDAY=TU,TH", "2026-10-05", "2026-10-05", "2026-10-11", "10-06 10-08"},
		{"FREQ=WEEKLY;INTERVAL=2;BYDAY=MO", "2026-10-05", "2026-10-05", "2026-10-31", "10-05 10-19"},
		{"FREQ=WEEKLY", "2026-10-07", "2026-10-01", "2026-10-21", "10-07 10-14 10-21"},
		{"FREQ=DAILY;INTERVAL=3", "2026-10-01", "2026-10-03", "2026-10-10", "10-04 10-07 10-10"},
		{"FREQ=MONTHLY;BYMONTHDAY=-1", "2026-01-01", "2026-02-01", "2026-04-30", "02-28 03-31 04-30"},
		{"FREQ=MONTHLY;BYMONTHDAY=31", "2026-01-31", "2026-02-01", "2026-05-31", "03-31 05-31"},
		{"RRULE:FREQ=MONTHLY;INTERVAL=2", "2026-01-15", "2026-01-01", "2026-06-30", "01-15 03-15 05-15"},
	}
	for _, c := range cases {
		r, err := Parse(c.rule)
		if err != nil {
			t.Fatalf("%s: %v", c.rule, err)
		}
		if got := dates(r.Between(d(c.start), d(c.from), d(c.to))); got != c.want {
			t.Errorf("%s: got %q want %q", c.rule, got, c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{"", "FREQ=HOURLY", "FREQ=WEEKLY;BYDAY=XX", "FREQ=DAILY;INTERVAL=0", "FREQ=MONTHLY;BYDAY=MO", "каждую субботу"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("%q: ждали ошибку", s)
		}
	}
}

func TestHuman(t *testing.T) {
	r, _ := Parse("FREQ=WEEKLY;BYDAY=TU,TH")
	if r.Human() != "по вт, чт" {
		t.Fatal(r.Human())
	}
}
