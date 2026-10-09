package parse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClaudeRequestAndResponse(t *testing.T) {
	var body map[string]any
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		answer, _ := json.Marshal(map[string]any{
			"intent": "event", "title": "Созвон с Т-Банком", "sphere": "career", "important": true, "urgent": false,
			"planned_date": nil, "start_at": "2026-10-06T16:00:00+03:00", "end_at": "2026-10-06T17:00:00+03:00",
			"deadline": nil, "estimate_min": nil, "tags": []string{"@звонок"}, "details": nil, "checklist": []string{},
			"rrule": nil, "time_minutes": nil, "time_item_id": nil, "confident": true, "question": nil,
		})
		resp, _ := json.Marshal(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": Model, "stop_reason": "end_turn",
			// Haiku 5.5 думает всегда: ответ начинается с thinking-блока (текст скрыт).
			"content": []map[string]any{{"type": "thinking", "thinking": "", "signature": "sig"}, {"type": "text", "text": string(answer)}},
			"usage":   map[string]any{"input_tokens": 10, "output_tokens": 10},
		})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(string(resp)))}, nil
	})}

	c := NewClaude("test-key", client)
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, domain.MSK)
	res, err := c.Parse(context.Background(), Input{Text: "во вторник созвон с тбанком 16-17", Now: now,
		Spheres: []domain.Sphere{{ID: 1, Slug: "work"}, {ID: 2, Slug: "career"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Intent != IntentEvent || res.StartAt == nil || *res.Sphere != "career" {
		t.Fatalf("разбор: %+v", res)
	}

	if body["model"] != Model {
		t.Fatalf("model: %v", body["model"])
	}
	if body["output_config"].(map[string]any)["effort"] != "low" {
		t.Fatalf("effort: %v", body["output_config"])
	}
	for _, k := range []string{"temperature", "top_p", "top_k", "thinking"} {
		if _, ok := body[k]; ok {
			t.Fatalf("%s в запросе: Haiku 5.5 отвечает на него 400", k)
		}
	}
	format := body["output_config"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("format: %v", format)
	}
	schema := format["schema"].(map[string]any)
	if schema["additionalProperties"] != false || len(schema["required"].([]any)) != len(schema["properties"].(map[string]any)) {
		t.Fatal("схема: все поля обязательны, additionalProperties=false")
	}
	user := body["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(user, "2026-10-03 18:00, суббота") {
		t.Fatalf("в запросе нет текущего момента: %q", user)
	}

	ops, err := res.Ops("inbox-1")
	if err != nil || len(ops) != 2 || ops[0].Op != "create" || ops[1].To != "$t" {
		t.Fatalf("ops: %+v %v", ops, err)
	}
}
