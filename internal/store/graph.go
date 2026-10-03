package store

import (
	"context"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// GraphEdge — ребро графа: связь из relations или «parent» (родитель → подзадача).
type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

type Graph struct {
	Nodes []domain.Item `json:"nodes"`
	Edges []GraphEdge   `json:"edges"`
}

// GraphMaxNodes — больше узлов на телефоне всё равно не разобрать.
const GraphMaxNodes = 300

// Graph — задачи, у которых есть связи или подзадачи: открытые и закрытые за
// последние 30 дней (чтобы цель не теряла сделанные части). Отменённые — нет.
func (s *Store) Graph(ctx context.Context, sphereID int) (Graph, error) {
	g := Graph{Nodes: []domain.Item{}, Edges: []GraphEdge{}}
	where := `i.id IN (
			SELECT from_id FROM relations UNION SELECT to_id FROM relations
			UNION SELECT id FROM items WHERE parent_id IS NOT NULL
			UNION SELECT parent_id FROM items WHERE parent_id IS NOT NULL)
		AND i.status <> 'cancelled' AND (i.status <> 'done' OR i.done_at > now() - interval '30 days')`
	args := []any{}
	if sphereID != 0 {
		where += ` AND i.sphere_id = $1`
		args = append(args, sphereID)
	}
	nodes, err := s.queryItems(ctx, where+` ORDER BY i.created_at LIMIT 300`, args...)
	if err != nil {
		return g, err
	}
	g.Nodes = nodes
	in := make(map[string]bool, len(nodes))
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		in[n.ID] = true
		ids = append(ids, n.ID)
	}
	rows, err := s.db.Query(ctx, `
		SELECT from_id::text, to_id::text, type FROM relations WHERE from_id = ANY($1::uuid[]) AND to_id = ANY($1::uuid[])
		UNION ALL
		SELECT parent_id::text, id::text, 'parent' FROM items WHERE id = ANY($1::uuid[]) AND parent_id = ANY($1::uuid[])`, ids)
	if err != nil {
		return g, err
	}
	defer rows.Close()
	for rows.Next() {
		var e GraphEdge
		if err := rows.Scan(&e.From, &e.To, &e.Type); err != nil {
			return g, err
		}
		if in[e.From] && in[e.To] {
			g.Edges = append(g.Edges, e)
		}
	}
	return g, rows.Err()
}

// RelatedItems — задачи на другом конце связей (для карточки: показать названия, а не id).
func (s *Store) RelatedItems(ctx context.Context, rels []domain.Relation, self string) ([]domain.Item, error) {
	var ids []string
	for _, r := range rels {
		if r.FromID != self {
			ids = append(ids, r.FromID)
		}
		if r.ToID != self {
			ids = append(ids, r.ToID)
		}
	}
	if len(ids) == 0 {
		return []domain.Item{}, nil
	}
	return s.queryItems(ctx, `i.id = ANY($1::uuid[])`, ids)
}
