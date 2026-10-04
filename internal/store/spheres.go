package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// SpherePatch — что меняется в настройках сферы; nil — не трогать.
// GcalColor — номер цвета Google Календаря 1–11, "" — подобрать автоматически.
// Hint — подсказка ИИ-разбору, что относится к сфере.
type SpherePatch struct {
	Name      *string `json:"name"`
	Color     *string `json:"color"`
	ColorDark *string `json:"color_dark"`
	Icon      *string `json:"icon"`
	Sort      *int    `json:"sort"`
	Archived  *bool   `json:"archived"`
	GcalColor *string `json:"gcal_color"`
	Hint      *string `json:"hint"`
}

var hexColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

func (p SpherePatch) validate() error {
	if p.Name != nil && (strings.TrimSpace(*p.Name) == "" || utf8.RuneCountInString(*p.Name) > 40) {
		return errors.New("название сферы: от 1 до 40 символов")
	}
	for _, c := range []*string{p.Color, p.ColorDark} {
		if c != nil && *c != "" && !hexColor.MatchString(*c) {
			return fmt.Errorf("цвет %q: нужен вид #RRGGBB", *c)
		}
	}
	if p.Color != nil && *p.Color == "" {
		return errors.New("у сферы должен быть цвет")
	}
	if p.Icon != nil && utf8.RuneCountInString(*p.Icon) > 8 {
		return errors.New("иконка — один эмодзи")
	}
	if p.GcalColor != nil && *p.GcalColor != "" {
		ok := false
		for i := 1; i <= 11; i++ {
			ok = ok || *p.GcalColor == fmt.Sprint(i)
		}
		if !ok {
			return errors.New("цвет Google Календаря — число от 1 до 11")
		}
	}
	if p.Hint != nil && utf8.RuneCountInString(*p.Hint) > 300 {
		return errors.New("подсказка — до 300 символов")
	}
	return nil
}

// UpdateSphere меняет сферу. Пустые color_dark/gcal_color/hint удаляют настройку.
func (s *Store) UpdateSphere(ctx context.Context, id int, p SpherePatch) (domain.Sphere, error) {
	if err := p.validate(); err != nil {
		return domain.Sphere{}, err
	}
	if p.Name != nil {
		*p.Name = strings.TrimSpace(*p.Name)
	}
	if p.Color != nil {
		*p.Color = strings.ToUpper(*p.Color)
	}
	if p.ColorDark != nil {
		*p.ColorDark = strings.ToUpper(*p.ColorDark)
	}
	// style: выставить непустые, удалить пустые.
	set, del := map[string]string{}, []string{}
	for k, v := range map[string]*string{"color_dark": p.ColorDark, "gcal_color": p.GcalColor, "hint": p.Hint} {
		switch {
		case v == nil:
		case strings.TrimSpace(*v) == "":
			del = append(del, k)
		default:
			set[k] = strings.TrimSpace(*v)
		}
	}
	tag, err := s.db.Exec(ctx, `UPDATE spheres SET
		name = COALESCE($2, name), color = COALESCE($3, color), icon = COALESCE($4, icon),
		sort = COALESCE($5, sort), archived = COALESCE($6, archived),
		style = (style - $7::text[]) || $8::jsonb
		WHERE id = $1`, id, p.Name, p.Color, p.Icon, p.Sort, p.Archived, del, set)
	if err != nil {
		return domain.Sphere{}, err
	}
	if tag.RowsAffected() == 0 {
		return domain.Sphere{}, fmt.Errorf("сфера %d: %w", id, ErrNotFound)
	}
	return s.sphere(ctx, id)
}

// CreateSphere — новая сфера в конце списка. Slug (имя для ИИ и CLI) — транслит названия.
func (s *Store) CreateSphere(ctx context.Context, name, color, icon string) (domain.Sphere, error) {
	p := SpherePatch{Name: &name, Color: &color, Icon: &icon}
	if err := p.validate(); err != nil {
		return domain.Sphere{}, err
	}
	name = strings.TrimSpace(name)
	base := slugify(name)
	slug := base
	for i := 2; ; i++ {
		var taken bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM spheres WHERE slug=$1)`, slug).Scan(&taken); err != nil {
			return domain.Sphere{}, err
		}
		if !taken {
			break
		}
		slug = fmt.Sprintf("%s%d", base, i)
	}
	var id int
	err := s.db.QueryRow(ctx, `INSERT INTO spheres (slug, name, color, icon, sort)
		VALUES ($1, $2, $3, $4, (SELECT COALESCE(max(sort), 0) + 10 FROM spheres)) RETURNING id`,
		slug, name, strings.ToUpper(color), icon).Scan(&id)
	if err != nil {
		return domain.Sphere{}, err
	}
	return s.sphere(ctx, id)
}

func (s *Store) sphere(ctx context.Context, id int) (domain.Sphere, error) {
	var x domain.Sphere
	err := s.db.QueryRow(ctx, `SELECT id, slug, name, color, icon, style, sort, archived FROM spheres WHERE id=$1`, id).
		Scan(&x.ID, &x.Slug, &x.Name, &x.Color, &x.Icon, &x.Style, &x.Sort, &x.Archived)
	return x, err
}

var translit = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh", 'з': "z", 'и': "i",
	'й': "y", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t",
	'у': "u", 'ф': "f", 'х': "h", 'ц': "c", 'ч': "ch", 'ш': "sh", 'щ': "sch", 'ы': "y", 'э': "e",
	'ю': "yu", 'я': "ya",
}

func slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case translit[r] != "":
			b.WriteString(translit[r])
			dash = false
		case r == 'ъ' || r == 'ь':
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('_')
				dash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "_")
	if len(slug) > 24 {
		slug = strings.Trim(slug[:24], "_")
	}
	if slug == "" {
		slug = "sphere"
	}
	return slug
}
