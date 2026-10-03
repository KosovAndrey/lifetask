// Package recur — правила повторов в подмножестве RFC 5545 RRULE (тот же формат
// понимает Google Calendar): FREQ=DAILY|WEEKLY|MONTHLY, INTERVAL, BYDAY, BYMONTHDAY.
//
//	FREQ=WEEKLY;BYDAY=SA            каждую субботу
//	FREQ=WEEKLY;BYDAY=TU,TH         по вторникам и четвергам
//	FREQ=DAILY;INTERVAL=2           через день
//	FREQ=MONTHLY;BYMONTHDAY=-1      в последний день месяца
package recur

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

type Freq string

const (
	Daily   Freq = "DAILY"
	Weekly  Freq = "WEEKLY"
	Monthly Freq = "MONTHLY"
)

type Rule struct {
	Freq       Freq
	Interval   int
	ByDay      []time.Weekday
	ByMonthDay []int // 1..31 или -1..-31 (с конца месяца)
}

var dayCodes = map[string]time.Weekday{
	"MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday, "TH": time.Thursday,
	"FR": time.Friday, "SA": time.Saturday, "SU": time.Sunday,
}

var dayRU = map[time.Weekday]string{
	time.Monday: "пн", time.Tuesday: "вт", time.Wednesday: "ср", time.Thursday: "чт",
	time.Friday: "пт", time.Saturday: "сб", time.Sunday: "вс",
}

func Parse(s string) (Rule, error) {
	r := Rule{Interval: 1}
	s = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(s)), "RRULE:")
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return r, fmt.Errorf("правило %q: ждём KEY=VALUE через ;", s)
		}
		switch k {
		case "FREQ":
			r.Freq = Freq(v)
			if r.Freq != Daily && r.Freq != Weekly && r.Freq != Monthly {
				return r, fmt.Errorf("FREQ=%s не поддерживается (DAILY, WEEKLY, MONTHLY)", v)
			}
		case "INTERVAL":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 365 {
				return r, fmt.Errorf("INTERVAL=%s", v)
			}
			r.Interval = n
		case "BYDAY":
			for _, d := range strings.Split(v, ",") {
				wd, ok := dayCodes[d]
				if !ok {
					return r, fmt.Errorf("BYDAY: %q (MO,TU,WE,TH,FR,SA,SU)", d)
				}
				r.ByDay = append(r.ByDay, wd)
			}
		case "BYMONTHDAY":
			for _, d := range strings.Split(v, ",") {
				n, err := strconv.Atoi(d)
				if err != nil || n == 0 || n < -31 || n > 31 {
					return r, fmt.Errorf("BYMONTHDAY: %q", d)
				}
				r.ByMonthDay = append(r.ByMonthDay, n)
			}
		default:
			return r, fmt.Errorf("%s не поддерживается", k)
		}
	}
	if r.Freq == "" {
		return r, fmt.Errorf("в правиле %q нет FREQ", s)
	}
	if len(r.ByDay) > 0 && r.Freq == Monthly {
		return r, fmt.Errorf("BYDAY с MONTHLY не поддерживается — используй BYMONTHDAY")
	}
	return r, nil
}

// Between — даты повторов в [from, to] для серии, начатой в start.
func (r Rule) Between(start, from, to domain.Date) []domain.Date {
	if from.Before(start.Time) {
		from = start
	}
	var out []domain.Date
	for d := from; !d.After(to.Time); d = d.AddDays(1) {
		if r.matches(start, d) {
			out = append(out, d)
		}
	}
	return out
}

func (r Rule) matches(start, d domain.Date) bool {
	switch r.Freq {
	case Daily:
		return daysBetween(start, d)%r.Interval == 0
	case Weekly:
		days := r.ByDay
		if len(days) == 0 {
			days = []time.Weekday{start.Weekday()}
		}
		if !slices.Contains(days, d.Weekday()) {
			return false
		}
		// Номер недели считаем от понедельника недели старта.
		return (daysBetween(mondayOf(start), d)/7)%r.Interval == 0
	case Monthly:
		months := (d.Year()-start.Year())*12 + int(d.Month()) - int(start.Month())
		if months%r.Interval != 0 {
			return false
		}
		mdays := r.ByMonthDay
		if len(mdays) == 0 {
			mdays = []int{start.Day()}
		}
		last := time.Date(d.Year(), d.Month()+1, 0, 0, 0, 0, 0, domain.MSK).Day()
		for _, md := range mdays {
			if md > 0 && d.Day() == md || md < 0 && d.Day() == last+md+1 {
				return true
			}
		}
	}
	return false
}

func daysBetween(a, b domain.Date) int {
	// Через UTC-полночь: в MSK нет перехода на летнее время, но так надёжнее.
	ua := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	ub := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(ub.Sub(ua).Hours() / 24)
}

func mondayOf(d domain.Date) domain.Date {
	return d.AddDays(-((int(d.Weekday()) + 6) % 7))
}

// Human — правило словами для превью и карточек.
func (r Rule) Human() string {
	every := ""
	if r.Interval > 1 {
		every = fmt.Sprintf(" (раз в %d)", r.Interval)
	}
	switch r.Freq {
	case Daily:
		if r.Interval > 1 {
			return fmt.Sprintf("каждые %d дн.", r.Interval)
		}
		return "каждый день"
	case Weekly:
		if len(r.ByDay) == 0 {
			return "каждую неделю" + every
		}
		names := make([]string, 0, len(r.ByDay))
		for _, d := range r.ByDay {
			names = append(names, dayRU[d])
		}
		return "по " + strings.Join(names, ", ") + every
	case Monthly:
		days := make([]string, 0, len(r.ByMonthDay))
		for _, d := range r.ByMonthDay {
			if d == -1 {
				days = append(days, "последнее")
			} else {
				days = append(days, strconv.Itoa(d))
			}
		}
		if len(days) == 0 {
			return "каждый месяц" + every
		}
		return strings.Join(days, ", ") + " числа" + every
	}
	return string(r.Freq)
}
