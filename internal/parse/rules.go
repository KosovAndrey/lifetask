package parse

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// Rules — разбор заметки без ИИ: даты, время, длительность, повторы, учёт
// времени, сфера по ключевым словам. Работает, пока нет ключа Claude, и как
// мгновенный разбор быстрого ввода в вебе. Неуверенность честно помечается —
// карточку всё равно подтверждаешь кнопкой или поправляешь ответом.
type Rules struct{}

var _ Parser = Rules{}

// Фрагмент, найденный правилом: вырезается из текста, остаток — заголовок.
type span struct{ from, to int }

type scan struct {
	text  string // нижний регистр, ё → е
	orig  string
	cut   []span
	now   time.Time
	today domain.Date
}

func (s *scan) find(re *regexp.Regexp) [][]int { return re.FindAllStringSubmatchIndex(s.text, -1) }
func (s *scan) sub(m []int, i int) string {
	if m[2*i] < 0 {
		return ""
	}
	return s.text[m[2*i]:m[2*i+1]]
}

// remove вырезает совпадение, но не его граничные символы: пробел после
// «по вт и чт» нужен следующему правилу («в 19»).
func (s *scan) remove(m []int) {
	from, to := m[0], m[1]
	isEdge := func(c byte) bool { return strings.IndexByte(" \t\n,.;:!?()", c) >= 0 }
	for from < to && isEdge(s.text[from]) {
		from++
	}
	for to > from && isEdge(s.text[to-1]) {
		to--
	}
	s.cut = append(s.cut, span{from, to})
}

func normalize(t string) string {
	return strings.ReplaceAll(strings.ToLower(t), "ё", "е")
}

var (
	weekdayRe = `(понедельник[аи]?|пн|вторник[аи]?|вт|сред[аеуы]|ср|четверг[аи]?|чт|пятниц[аеуы]|пт|суббот[аеуы]|сб|воскресень[еяю]|вс)`
	rruleDays = map[time.Weekday]string{time.Monday: "MO", time.Tuesday: "TU", time.Wednesday: "WE",
		time.Thursday: "TH", time.Friday: "FR", time.Saturday: "SA", time.Sunday: "SU"}
	months = map[string]time.Month{"января": 1, "февраля": 2, "марта": 3, "апреля": 4, "мая": 5, "июня": 6,
		"июля": 7, "августа": 8, "сентября": 9, "октября": 10, "ноября": 11, "декабря": 12}

	b = `(?:^|[\s,.;:!?(])` // граница слова (\b в RE2 не знает кириллицу)
	e = `(?:$|[\s,.;:!?)])`

	reNote      = regexp.MustCompile(`^\s*(заметка|мысль|идея)\s*[:—-]\s*`)
	reTimeLog   = regexp.MustCompile(b + `(?:потратил[а]?\s+)?(\d+(?:[.,]\d+)?)\s*(часов|часа|час|ч)?\s*(\d+)?\s*(минуты|минута|минут|мин|м)?` + `\s*$`)
	reHalfHour  = regexp.MustCompile(b + `(полчаса)\s*$`)
	reRecurDays = regexp.MustCompile(b + `(?:по|каждый|каждую|каждое|каждые)\s+((?:` + weekdayRe + `(?:[\s,и]+)?)+)` + e)
	reRecurWord = regexp.MustCompile(b + `(каждый день|ежедневно|по будням|по выходным|каждую неделю|раз в неделю|еженедельно|каждый месяц|раз в месяц|ежемесячно)` + e)
	reRelDay    = regexp.MustCompile(b + `(сегодня|завтра|послезавтра)` + e)
	reInDays    = regexp.MustCompile(b + `через\s+(\d+|два|три|неделю)\s*(дня|дней|день)?` + e)
	reInHours   = regexp.MustCompile(b + `через\s+(час|пару часов|полчаса|\d+\s*(?:часа|часов|час|ч|минут|мин|м))` + e)
	reWeekday   = regexp.MustCompile(b + `(?:в|во|на)?\s*` + weekdayRe + e)
	reDeadline  = regexp.MustCompile(b + `(?:до|дедлайн|к)\s+(` + weekdayRe + `|\d{1,2}[./]\d{1,2}|\d{1,2}\s+(?:января|февраля|марта|апреля|мая|июня|июля|августа|сентября|октября|ноября|декабря)|завтра)` + e)
	reDateNum   = regexp.MustCompile(b + `(\d{1,2})[./](\d{1,2})(?:[./](\d{2,4}))?` + e)
	reDateWord  = regexp.MustCompile(b + `(\d{1,2})\s+(января|февраля|марта|апреля|мая|июня|июля|августа|сентября|октября|ноября|декабря)` + e)
	reWeekend   = regexp.MustCompile(b + `на выходных` + e)
	reRange     = regexp.MustCompile(b + `(?:с\s+)?(\d{1,2})(?:[:.](\d{2}))?\s*(?:-|–|до)\s*(\d{1,2})(?:[:.](\d{2}))?` + e)
	reAt        = regexp.MustCompile(b + `в\s+(\d{1,2})(?:[:.](\d{2}))?(?:\s*(утра|дня|вечера|ночи))?` + e)
	reClock     = regexp.MustCompile(b + `(\d{1,2})[:.](\d{2})` + e)
	reDuration  = regexp.MustCompile(b + `на\s+(\d+(?:[.,]\d+)?|полтора часа|час|полчаса|пару часов)\s*(часов|часа|час|ч|минуты|минут|мин|м)?` + e)
	reUrgent    = regexp.MustCompile(b + `(срочно|asap|горит)` + e)
	reImportant = regexp.MustCompile(b + `(важно|!!?)` + e)
)

// Сферы по ключевым словам (основы слов). Порядок важен: «тбанк» раньше «банк».
// weekdayOf — день недели по любой форме слова («пятницу», «пятницы», «пт»).
func weekdayOf(w string) (time.Weekday, bool) {
	for _, p := range []struct {
		prefix string
		wd     time.Weekday
	}{{"понед", time.Monday}, {"вторн", time.Tuesday}, {"сред", time.Wednesday}, {"четв", time.Thursday},
		{"пятн", time.Friday}, {"суб", time.Saturday}, {"воскр", time.Sunday}} {
		if strings.HasPrefix(w, p.prefix) {
			return p.wd, true
		}
	}
	wd, ok := map[string]time.Weekday{"пн": time.Monday, "вт": time.Tuesday, "ср": time.Wednesday, "чт": time.Thursday,
		"пт": time.Friday, "сб": time.Saturday, "вс": time.Sunday}[w]
	return wd, ok
}

var sphereWords = []struct{ slug, words string }{
	{"career", "собес|собеседован|т-банк|тбанк|рекрутер|резюме|ваканси|оффер|hr"},
	{"work", "работ|стажир|яндекс|тимлид|ментор|стендап|ревью|созвон с командой"},
	{"study", "курс|учеб|лекци|1с|экзамен|домашк|семинар|aston|астон"},
	{"product", "tryberry|трайберри|lifetask|лайфтаск|бот|деплой|релиз|фича"},
	{"health", "спорт|зал|трениров|бег|пробежк|врач|стоматолог|анализ|таблетк|йог"},
	{"leisure", "кино|игр|сериал|погулять|прогулк|друз|концерт|отпуск|книг"},
	// Деньги и документы — тоже быт (отдельной сферы «Финансы» нет).
	{"home", "убор|стирк|продукт|купить|посуд|ремонт|готов|мусор|пылесос|оплат|налог|счет|перевод|кредит|ипотек|бюджет|квартплат|госуслуг|справк"},
}

var eventWords = regexp.MustCompile(`созвон|встреч|собес|звонок|позвонить|прием|приём|лекци|занятие|тренировк|врач|стоматолог|семинар`)

func (Rules) Parse(_ context.Context, in Input) (Result, error) {
	now := in.Now.In(domain.MSK)
	if now.IsZero() {
		now = time.Now().In(domain.MSK)
	}
	y, mo, d := now.Date()
	s := &scan{text: normalize(in.Text), orig: in.Text, now: now, today: domain.Date{Time: time.Date(y, mo, d, 0, 0, 0, 0, domain.MSK)}}
	// Уточнение ответом: «уборка в субботу\nУточнение: в воскресенье» — более
	// поздние совпадения перекрывают ранние, поэтому везде берём последнее.
	r := Result{Intent: IntentTask, Confident: true, Tags: []string{}, Checklist: []string{}}

	if m := reNote.FindStringSubmatchIndex(s.text); m != nil {
		r.Intent = IntentNote
		s.remove(m)
	}

	// Учёт времени: «уборка 40м», «собес 1ч20», «чтение 1.5ч», «уборка полчаса».
	if r.Intent != IntentNote {
		if mins, m := s.timeLog(); mins > 0 && !strings.Contains(s.text, "через") && !strings.Contains(s.text, " на ") {
			r.Intent, r.TimeMinutes = IntentTimeLog, &mins
			s.remove(m)
			r.TimeItemID = bestCandidate(s.rest(), in.Candidates)
		}
	}

	if r.Intent != IntentTimeLog {
		s.recurrence(&r)
		s.deadline(&r)
		date, hasDate := s.date()
		start, end, hasTime := s.clock()
		if after, ok := s.inHours(); ok {
			date, hasDate = domain.Date{Time: time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, domain.MSK)}, true
			start, hasTime = after, true
		}
		if mins := s.duration(); mins > 0 {
			r.EstimateMin = &mins
		}
		switch {
		case hasTime:
			if !hasDate {
				date = s.today
				// «в 9» вечером — скорее всего про завтра.
				if start.Hour() < now.Hour() || start.Hour() == now.Hour() && start.Minute() <= now.Minute() {
					date = s.today.AddDays(1)
					r.Confident = false
					r.Question = ptrStr("Это на завтра? Время уже прошло.")
				}
			}
			st := time.Date(date.Year(), date.Month(), date.Day(), start.Hour(), start.Minute(), 0, 0, domain.MSK)
			r.StartAt = ptrStr(st.Format(time.RFC3339))
			switch {
			case !end.IsZero():
				en := time.Date(date.Year(), date.Month(), date.Day(), end.Hour(), end.Minute(), 0, 0, domain.MSK)
				if en.After(st) {
					r.EndAt = ptrStr(en.Format(time.RFC3339))
				}
			case r.EstimateMin != nil:
				r.EndAt = ptrStr(st.Add(time.Duration(*r.EstimateMin) * time.Minute).Format(time.RFC3339))
			}
		case hasDate:
			r.PlannedDate = ptrStr(date.String())
		}
		if r.Intent != IntentNote && (r.EndAt != nil || hasTime && eventWords.MatchString(s.text)) {
			r.Intent = IntentEvent
		}
	}

	if m := s.find(reUrgent); m != nil {
		r.Urgent = true
		s.remove(m[len(m)-1])
	}
	if m := s.find(reImportant); m != nil {
		r.Important = true
		s.remove(m[len(m)-1])
	}
	if sp := sphereOf(s.text); sp != "" {
		r.Sphere = &sp
	}
	r.Title = s.title()
	if r.Title == "" {
		r.Title = strings.TrimSpace(in.Text)
		r.Confident = false
	}
	if r.Intent == IntentTimeLog && r.TimeItemID == nil && len(in.Candidates) > 0 {
		r.Confident = false
		r.Question = ptrStr("Не нашёл подходящую задачу — заведу выполненную новую. Ок?")
	}
	return r, nil
}

func ptrStr(s string) *string { return &s }

func (s *scan) timeLog() (int, []int) {
	if m := reHalfHour.FindStringSubmatchIndex(s.text); m != nil {
		return 30, m
	}
	m := reTimeLog.FindStringSubmatchIndex(s.text)
	if m == nil {
		return 0, nil
	}
	num, hour, extra, minute := s.sub(m, 1), s.sub(m, 2), s.sub(m, 3), s.sub(m, 4)
	if hour == "" && minute == "" {
		return 0, nil // просто число в конце — не время
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(num, ",", "."), 64)
	if err != nil {
		return 0, nil
	}
	total := 0.0
	if hour != "" {
		total = v * 60
		if extra != "" {
			x, _ := strconv.Atoi(extra)
			total += float64(x)
		}
	} else {
		total = v
	}
	if total <= 0 || total > 16*60 {
		return 0, nil
	}
	return int(total + 0.5), m
}

// BestMatch — задача из cands, больше всего похожая на текст по основам слов (nil — нет похожих).
func BestMatch(text string, cands []Candidate) *string { return bestCandidate(text, cands) }

// bestCandidate — задача, у которой больше всего общих основ слов с текстом.
func bestCandidate(text string, cands []Candidate) *string {
	words := stems(text)
	if len(words) == 0 {
		return nil
	}
	best, bestScore := "", 0
	for _, c := range cands {
		score := 0
		for w := range stems(c.Title) {
			if words[w] {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = c.ID, score
		}
	}
	if bestScore == 0 {
		return nil
	}
	return &best
}

func stems(t string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(normalize(t), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		r := []rune(w)
		if len(r) < 3 {
			continue
		}
		if len(r) > 5 {
			r = r[:5] // грубая основа: «уборка», «уборку», «уборкой» → «убор…»
		}
		out[string(r)] = true
	}
	return out
}

func (s *scan) recurrence(r *Result) {
	if ms := s.find(reRecurWord); ms != nil {
		m := ms[len(ms)-1]
		rule := map[string]string{
			"каждый день": "FREQ=DAILY", "ежедневно": "FREQ=DAILY",
			"по будням": "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR", "по выходным": "FREQ=WEEKLY;BYDAY=SA,SU",
			"каждую неделю": "FREQ=WEEKLY", "раз в неделю": "FREQ=WEEKLY", "еженедельно": "FREQ=WEEKLY",
			"каждый месяц": "FREQ=MONTHLY", "раз в месяц": "FREQ=MONTHLY", "ежемесячно": "FREQ=MONTHLY",
		}[s.sub(m, 1)]
		r.RRule = &rule
		s.remove(m)
		return
	}
	ms := s.find(reRecurDays)
	if ms == nil {
		return
	}
	m := ms[len(ms)-1]
	var days []string
	seen := map[string]bool{}
	for _, w := range regexp.MustCompile(weekdayRe).FindAllString(s.sub(m, 1), -1) {
		wd, ok := weekdayOf(w)
		if !ok {
			continue
		}
		code := rruleDays[wd]
		if !seen[code] {
			days, seen[code] = append(days, code), true
		}
	}
	if len(days) == 0 {
		return
	}
	rule := "FREQ=WEEKLY;BYDAY=" + strings.Join(days, ",")
	r.RRule = &rule
	s.remove(m)
}

func (s *scan) nextWeekday(wd time.Weekday) domain.Date {
	diff := (int(wd) - int(s.today.Weekday()) + 7) % 7
	return s.today.AddDays(diff) // сегодня, если совпало
}

func (s *scan) parseDayWord(w string) (domain.Date, bool) {
	switch w {
	case "сегодня":
		return s.today, true
	case "завтра":
		return s.today.AddDays(1), true
	case "послезавтра":
		return s.today.AddDays(2), true
	}
	if wd, ok := weekdayOf(w); ok {
		return s.nextWeekday(wd), true
	}
	if m := reDateNum.FindStringSubmatch(" " + w + " "); m != nil {
		return s.numDate(m[1], m[2], m[3])
	}
	if m := reDateWord.FindStringSubmatch(" " + w + " "); m != nil {
		day, _ := strconv.Atoi(m[1])
		return s.monthDate(day, months[m[2]])
	}
	return domain.Date{}, false
}

func (s *scan) numDate(dd, mm, yy string) (domain.Date, bool) {
	day, _ := strconv.Atoi(dd)
	mon, _ := strconv.Atoi(mm)
	if mon < 1 || mon > 12 || day < 1 || day > 31 {
		return domain.Date{}, false
	}
	if yy != "" {
		year, _ := strconv.Atoi(yy)
		if year < 100 {
			year += 2000
		}
		return domain.Date{Time: time.Date(year, time.Month(mon), day, 0, 0, 0, 0, domain.MSK)}, true
	}
	return s.monthDate(day, time.Month(mon))
}

// monthDate — ближайшая будущая такая дата (5.01 в октябре — это январь следующего года).
func (s *scan) monthDate(day int, mon time.Month) (domain.Date, bool) {
	d := time.Date(s.today.Year(), mon, day, 0, 0, 0, 0, domain.MSK)
	if d.Month() != mon {
		return domain.Date{}, false // 31 февраля
	}
	if d.Before(s.today.Time) {
		d = d.AddDate(1, 0, 0)
	}
	return domain.Date{Time: d}, true
}

func (s *scan) deadline(r *Result) {
	ms := s.find(reDeadline)
	if ms == nil {
		return
	}
	m := ms[len(ms)-1]
	d, ok := s.parseDayWord(strings.TrimSpace(s.sub(m, 1)))
	if !ok {
		return
	}
	dl := time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 0, 0, domain.MSK)
	r.Deadline = ptrStr(dl.Format(time.RFC3339))
	s.remove(m)
}

// date — последняя упомянутая дата (кроме уже съеденного дедлайна).
func (s *scan) date() (domain.Date, bool) {
	type hit struct {
		pos int
		d   domain.Date
		m   []int
	}
	var best *hit
	var seen [][]int
	consider := func(pos int, d domain.Date, m []int) {
		if s.isCut(m) {
			return
		}
		seen = append(seen, m)
		if best == nil || pos >= best.pos {
			best = &hit{pos, d, m}
		}
	}
	for _, m := range s.find(reRelDay) {
		d, _ := s.parseDayWord(s.sub(m, 1))
		consider(m[0], d, m)
	}
	for _, m := range s.find(reInDays) {
		n := map[string]int{"два": 2, "три": 3, "неделю": 7}[s.sub(m, 1)]
		if n == 0 {
			n, _ = strconv.Atoi(s.sub(m, 1))
		}
		if n > 0 && n < 400 {
			consider(m[0], s.today.AddDays(n), m)
		}
	}
	for _, m := range s.find(reWeekend) {
		consider(m[0], s.nextWeekday(time.Saturday), m)
	}
	for _, m := range s.find(reWeekday) {
		if d, ok := s.parseDayWord(s.sub(m, 1)); ok {
			consider(m[0], d, m)
		}
	}
	for _, m := range s.find(reDateNum) {
		if d, ok := s.numDate(s.sub(m, 1), s.sub(m, 2), s.sub(m, 3)); ok {
			consider(m[0], d, m)
		}
	}
	for _, m := range s.find(reDateWord) {
		day, _ := strconv.Atoi(s.sub(m, 1))
		if d, ok := s.monthDate(day, months[s.sub(m, 2)]); ok {
			consider(m[0], d, m)
		}
	}
	if best == nil {
		return domain.Date{}, false
	}
	// Вырезаем все упоминания: при уточнении старая дата не должна остаться в заголовке.
	for _, m := range seen {
		s.remove(m)
	}
	return best.d, true
}

func hm(h, m string, mod string) (time.Time, bool) {
	hh, err := strconv.Atoi(h)
	if err != nil {
		return time.Time{}, false
	}
	mm := 0
	if m != "" {
		mm, _ = strconv.Atoi(m)
	}
	switch mod {
	case "дня", "вечера":
		if hh < 12 {
			hh += 12
		}
	case "ночи":
		if hh == 12 {
			hh = 0
		}
	}
	if hh > 23 || mm > 59 {
		return time.Time{}, false
	}
	return time.Date(2000, 1, 1, hh, mm, 0, 0, domain.MSK), true
}

// clock — время: диапазон «16-17», «с 16 до 17:30» или точка «в 16», «16:00».
func (s *scan) clock() (start, end time.Time, ok bool) {
	if ms := s.find(reRange); ms != nil {
		for i := len(ms) - 1; i >= 0; i-- {
			m := ms[i]
			if s.isCut(m) {
				continue
			}
			a, okA := hm(s.sub(m, 1), s.sub(m, 2), "")
			z, okZ := hm(s.sub(m, 3), s.sub(m, 4), "")
			// «5-7 дней» и даты «10.10» сюда не должны попасть: часы до 23 и конец после начала.
			if okA && okZ && z.After(a) && (s.sub(m, 2) != "" || s.sub(m, 4) != "" || strings.Contains(s.text[m[0]:m[1]], "с ") || a.Hour() >= 7) {
				s.remove(m)
				return a, z, true
			}
		}
	}
	for _, re := range []*regexp.Regexp{reAt, reClock} {
		ms := s.find(re)
		for i := len(ms) - 1; i >= 0; i-- {
			m := ms[i]
			if s.isCut(m) {
				continue
			}
			mod := ""
			if re == reAt {
				mod = s.sub(m, 3)
			}
			if t, ok := hm(s.sub(m, 1), s.sub(m, 2), mod); ok {
				s.remove(m)
				return t, time.Time{}, true
			}
		}
	}
	return time.Time{}, time.Time{}, false
}

func (s *scan) inHours() (time.Time, bool) {
	ms := s.find(reInHours)
	if ms == nil {
		return time.Time{}, false
	}
	m := ms[len(ms)-1]
	w := s.sub(m, 1)
	var d time.Duration
	switch {
	case w == "час":
		d = time.Hour
	case w == "пару часов":
		d = 2 * time.Hour
	case w == "полчаса":
		d = 30 * time.Minute
	default:
		n, _ := strconv.Atoi(strings.TrimFunc(w, func(r rune) bool { return !unicode.IsDigit(r) }))
		if strings.Contains(w, "м") && !strings.Contains(w, "ч") {
			d = time.Duration(n) * time.Minute
		} else {
			d = time.Duration(n) * time.Hour
		}
	}
	if d <= 0 {
		return time.Time{}, false
	}
	s.remove(m)
	// Округляем до 5 минут вверх: «через час» в 14:07 → 15:10.
	t := s.now.Add(d)
	t = t.Add(time.Duration((5-t.Minute()%5)%5) * time.Minute).Truncate(time.Minute)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, domain.MSK), true
}

func (s *scan) duration() int {
	ms := s.find(reDuration)
	if ms == nil {
		return 0
	}
	m := ms[len(ms)-1]
	num, unit := s.sub(m, 1), s.sub(m, 2)
	var mins float64
	switch num {
	case "полтора часа":
		mins = 90
	case "час":
		mins = 60
	case "полчаса":
		mins = 30
	case "пару часов":
		mins = 120
	default:
		v, err := strconv.ParseFloat(strings.ReplaceAll(num, ",", "."), 64)
		if err != nil || unit == "" {
			return 0 // «на 5» без единиц — не длительность
		}
		if strings.HasPrefix(unit, "ч") {
			mins = v * 60
		} else {
			mins = v
		}
	}
	if mins <= 0 || mins > 24*60 {
		return 0
	}
	s.remove(m)
	return int(mins + 0.5)
}

func (s *scan) isCut(m []int) bool {
	for _, c := range s.cut {
		if m[0] < c.to && c.from < m[1] {
			return true
		}
	}
	return false
}

// rest — текст без вырезанных фрагментов (в нижнем регистре).
func (s *scan) rest() string {
	keep := make([]bool, len(s.text))
	for i := range keep {
		keep[i] = true
	}
	for _, c := range s.cut {
		for i := c.from; i < c.to && i < len(keep); i++ {
			keep[i] = false
		}
	}
	var b strings.Builder
	for i := 0; i < len(s.text); i++ {
		if keep[i] {
			b.WriteByte(s.text[i])
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// title — оригинальный текст (с регистром) без вырезанного, без «уточнение: …»,
// мусорных предлогов по краям; с заглавной буквы.
func (s *scan) title() string {
	// normalize не меняет длину в байтах (ё→е — оба по 2 байта), поэтому позиции совпадают.
	orig := s.orig
	if len(orig) != len(s.text) {
		orig = s.text
	}
	rest := s.rest()
	var b strings.Builder
	for i := 0; i < len(orig); i++ {
		if rest[i] == ' ' && s.text[i] != ' ' {
			b.WriteByte(' ')
		} else {
			b.WriteByte(orig[i])
		}
	}
	t := b.String()
	if i := strings.Index(normalize(t), "уточнение:"); i >= 0 {
		t = t[:i]
	}
	t = strings.Join(strings.Fields(t), " ")
	junk := map[string]bool{"в": true, "во": true, "на": true, "с": true, "до": true, "и": true, "к": true, "по": true, "-": true, "—": true, ",": true}
	words := strings.Fields(t)
	for len(words) > 0 && junk[normalize(words[len(words)-1])] {
		words = words[:len(words)-1]
	}
	for len(words) > 0 && junk[normalize(words[0])] {
		words = words[1:]
	}
	t = strings.Trim(strings.Join(words, " "), " ,.;:-—")
	if t == "" {
		return ""
	}
	r := []rune(t)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// sphereOf — сфера по основам слов. Сравниваем с началом слова, а не подстрокой:
// иначе «суББОТу» попадала в «Продукты» из-за «бот».
func sphereOf(text string) string {
	tokens := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' })
	for _, sw := range sphereWords {
		for _, w := range strings.Split(sw.words, "|") {
			if strings.Contains(w, " ") {
				if strings.Contains(text, w) {
					return sw.slug
				}
				continue
			}
			for _, t := range tokens {
				if strings.HasPrefix(t, w) {
					return sw.slug
				}
			}
		}
	}
	return ""
}

// String — для логов и отладки.
func (r Result) String() string {
	return fmt.Sprintf("%s «%s» date=%v start=%v end=%v rrule=%v", r.Intent, r.Title, deref(r.PlannedDate), deref(r.StartAt), deref(r.EndAt), deref(r.RRule))
}

func deref(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

// Fallback — основной разбор (Claude), а при его ошибке (сеть, лимиты) — запасной
// (правила): заметка всё равно получает карточку, а не «разберём вечером».
type Fallback struct {
	Primary, Secondary Parser
}

func (f Fallback) Parse(ctx context.Context, in Input) (Result, error) {
	r, err := f.Primary.Parse(ctx, in)
	if err == nil {
		return r, nil
	}
	r2, err2 := f.Secondary.Parse(ctx, in)
	if err2 != nil {
		return r, err
	}
	r2.Confident = false
	note := "ИИ недоступен — разобрал по правилам, проверь."
	if r2.Question != nil {
		note += " " + *r2.Question
	}
	r2.Question = &note
	return r2, nil
}
