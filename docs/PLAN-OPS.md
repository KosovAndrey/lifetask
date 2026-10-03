# Операции плана изменений

ИИ (Claude на разборе, бот) меняет данные только планом:

```
plan propose plan.json   → превью + id (прогон в транзакции с откатом, в БД ничего не пишется)
plan apply <id>          → то же самое атомарно, после «ок» пользователя
plan reject <id>
```

```json
{"summary": "Разбор 03.10", "ops": [ ... ]}
```

Ссылки на сущности, созданные выше в этом же плане: `"$<ref>"`.
Если хоть одна операция падает, не применяется ничего.

## Задачи

```json
{"op": "create", "ref": "call", "item": {
  "kind": "event",                       // task (по умолч.) | goal | event | note
  "title": "Созвон с Т-Банком",
  "sphere": "career",                    // slug: work career study product home leisure health finance
  "project": "lifeplan",                 // имя | $ref | id (необязательно)
  "parent": "$goal",                     // $ref | uuid — подзадача
  "status": "todo",                      // inbox todo doing waiting done cancelled someday
  "important": true, "urgent": false,    // матрица: Q1 оба, Q2 важно, Q3 срочно, Q4 ни то ни другое
  "planned_date": "2026-10-07",          // когда делаю (без времени)
  "start_at": "2026-10-07T16:00:00+03:00", "end_at": "2026-10-07T17:00:00+03:00",  // слот
  "deadline": "2026-10-10T23:59:00+03:00",
  "estimate_min": 60, "weight": 1,
  "tags": ["@звонок", "≤15мин"],         // @… и ≤… — контекстные теги
  "body": [ ... ], "props": { ... }
}}

{"op": "update", "id": "<uuid|$ref>", "set": {"planned_date": "2026-10-08", "estimate_min": 45}}
{"op": "done",   "id": "<uuid|$ref>"}
{"op": "delete", "id": "<uuid>"}          // только для мусора; отменённое — status=cancelled
```

`set` принимает те же поля, что и `item`; `null` очищает поле. Перенос `planned_date`
на более позднюю дату у разовой задачи увеличивает `postpone_count`; у повторяющихся — нет.

## Связи, время, инбокс, проекты

```json
{"op": "relate",   "from": "$prep", "to": "$call", "type": "prepares"}
// blocks (to нельзя начать до from) | related | follows | part_of (→ цель) | prepares (→ событие)
{"op": "unrelate", "from": "...", "to": "...", "type": "related"}

{"op": "log_time", "id": "<uuid|$ref>", "minutes": 40, "started_at": "2026-10-03T19:00:00+03:00", "note": "секундомер"}

{"op": "inbox", "id": "<inbox-id>", "status": "accepted", "to": "$task"}   // accepted | rejected | deferred

{"op": "project", "ref": "lp", "name": "lifeplan", "sphere": "product", "parent": "Продукты-группа"}
```

## Повторы

```json
{"op": "recur", "rule": "FREQ=WEEKLY;BYDAY=SA", "start": "2026-10-10",
 "item": {"title": "Уборка", "sphere": "home", "estimate_min": 60}}

{"op": "recur", "rule": "FREQ=WEEKLY;BYDAY=TU,TH", "start": "2026-10-06", "time": "19:00", "duration_min": 90,
 "until": "2026-12-31", "item": {"title": "Спорт", "sphere": "health"}}

{"op": "recur_stop", "id": "<recurrence-id>", "start": "2026-11-01"}   // с какой даты; по умолчанию сегодня
```

Правила — подмножество RRULE: `FREQ=DAILY|WEEKLY|MONTHLY`, `INTERVAL`, `BYDAY=MO..SU`,
`BYMONTHDAY` (`-1` = последний день). В `item` дат нет: день задаёт правило, время — `time` + `duration_min`.
Экземпляры создаются на 14 дней вперёд. Их перенос не увеличивает `postpone_count`.
Удалённый экземпляр не пересоздаётся. Список серий: `plan recur`.

## body — блоки оформления

Набор типов закрытый, состав и порядок блоков свободные: оформляй под задачу.

```json
{"type": "md",        "text": "markdown"}
{"type": "checklist", "items": [{"text": "...", "done": false}]}
{"type": "link",      "url": "https://...", "title": "..."}
{"type": "file",      "drive_id": "...", "name": "...", "mime": "..."}
{"type": "ref",       "item_id": "<uuid>"}
{"type": "callout",   "tone": "info|warn|ok", "text": "..."}
{"type": "code",      "lang": "go", "text": "..."}
{"type": "table",     "columns": ["..."], "rows": [["..."]]}
{"type": "comment",   "at": "2026-10-03T21:00:00+03:00", "author": "me|claude", "text": "..."}
```

Чек-лист — лёгкие пункты без учёта. Если пункту нужны дата, оценка или вес, это подзадача (`parent`).
