// LifeTask — веб-интерфейс. Без фреймворков: DOM строится функцией h(),
// данные пользователя вставляются только как текст (никакого innerHTML).
import { createOutbox, replayOutbox } from './queue.mjs';

// ── API ──────────────────────────────────────────────────────────────────────

class AuthError extends Error {}

// Очередь записей без сети: IndexedDB «lifetask/outbox». Каждая запись несёт
// Idempotency-Key — сервер выполнит её ровно один раз, даже если ответ потеряется.
const outbox = createOutbox(indexedDB);

let offlineSince = null;

function describeWrite(method, path, body) {
  if (path === 'items' && method === 'POST') return `новая задача «${body && body.title}»`;
  if (path === 'inbox' && method === 'POST') return `во входящие: «${body && body.text}»`;
  if (path.startsWith('journal/')) return `дневник ${path.slice(8)}`;
  if (path.startsWith('items/') && body && body.status === 'done') return 'отметка «готово»';
  return `${method} ${path}`;
}

// Ключ идемпотентности. randomUUID есть только в защищённом контексте (HTTPS),
// getRandomValues — везде.
function newKey() {
  if (crypto.randomUUID) return crypto.randomUUID();
  return [...crypto.getRandomValues(new Uint8Array(16))].map((b) => b.toString(16).padStart(2, '0')).join('');
}

async function send(method, path, body, key) {
  const headers = { 'Content-Type': 'application/json', 'X-Requested-With': 'lifetask' };
  if (key) headers['Idempotency-Key'] = key;
  return fetch('/api/' + path, {
    method, credentials: 'same-origin', headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

async function api(method, path, body) {
  const write = method !== 'GET';
  const key = write ? newKey() : undefined;
  let res;
  try {
    res = await send(method, path, body, key);
  } catch (err) {
    if (!write) throw err;
    // Нет сети — кладём в очередь и считаем записанным.
    await outbox.add({ key, method, path, body, ts: Date.now(), label: describeWrite(method, path, body) });
    toast('Нет сети — сохранил, отправлю, когда появится');
    refreshQueueBadge();
    return { queued: true };
  }
  if (!write) setOffline(res.headers.get('X-LP-Offline') === '1');
  if (res.status === 401) throw new AuthError((await res.json().catch(() => ({}))).error || 'нужен вход');
  if (res.status === 204) return null;
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error((data && data.error) || res.statusText);
  return data;
}

let flushing = null;
let queueTimer;
function flushQueue(onlyKey) {
  if (flushing) return flushing;
  if (!navigator.onLine) return Promise.resolve();
  clearTimeout(queueTimer);
  flushing = (async () => {
    try {
      const r = await replayOutbox({ store: outbox, send, onlyKey });
      if (r.nextRetryAt !== null) {
        queueTimer = setTimeout(() => flushQueue(), Math.max(0, Math.min(2147483647, r.nextRetryAt - Date.now())));
      }
      if (r.sent) toast(`Отправлено из очереди: ${r.sent}`);
      else if (r.failed) toast('Некоторые изменения не отправились — проверь очередь во «Входящих»');
      if (r.changed) render();
    } catch (err) { handleError(err); }
    finally {
      flushing = null;
      refreshQueueBadge();
    }
  })();
  return flushing;
}

async function retryQueued(key) {
  if (flushing) await flushing;
  const entry = (await outbox.all()).find((e) => e.key === key);
  if (!entry) return;
  await outbox.add({ ...entry, state: 'pending', retryAt: null, attempts: 0, error: null });
  await flushQueue(key);
  render();
}

async function discardQueued(key) {
  if (flushing) await flushing;
  await outbox.remove(key);
  await refreshQueueBadge();
  render();
  flushQueue();
}

async function refreshQueueBadge() {
  const entries = await outbox.all().catch(() => []);
  const n = entries.length;
  const failed = entries.filter((e) => e.state === 'failed' || e.state === 'auth').length;
  const b = $('queue-count');
  b.textContent = `${failed ? '⚠️' : '⏳'} ${n}`;
  b.title = failed ? 'Есть неотправленные изменения — проверь очередь во «Входящих»' : 'Ждут отправки — список во «Входящих»';
  b.hidden = n === 0;
}

function setOffline(on) {
  if (on && !offlineSince) offlineSince = new Date();
  if (!on) offlineSince = null;
  const bar = $('offline');
  bar.hidden = !on;
  if (on) bar.textContent = 'Нет сети — показываю последние данные. Изменения сохранятся и уйдут позже.';
}

// ── DOM ──────────────────────────────────────────────────────────────────────

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else if (k === 'style') for (const [p, val] of Object.entries(v)) el.style.setProperty(p, val);
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid === null || kid === undefined || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

const $ = (id) => document.getElementById(id);

let toastTimer;
function toast(msg) {
  const t = $('toast');
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), 2600);
}

// ── Даты (всё по Москве) ─────────────────────────────────────────────────────

const TZ = 'Europe/Moscow';
const WD = ['вс', 'пн', 'вт', 'ср', 'чт', 'пт', 'сб'];
const MONTHS = ['января', 'февраля', 'марта', 'апреля', 'мая', 'июня', 'июля', 'августа', 'сентября', 'октября', 'ноября', 'декабря'];

function todayStr() {
  return new Intl.DateTimeFormat('en-CA', { timeZone: TZ }).format(new Date());
}
function addDays(ds, n) {
  const d = new Date(ds + 'T12:00:00Z');
  d.setUTCDate(d.getUTCDate() + n);
  return d.toISOString().slice(0, 10);
}
function weekday(ds) { return new Date(ds + 'T12:00:00Z').getUTCDay(); }
function mondayOf(ds) { return addDays(ds, -((weekday(ds) + 6) % 7)); }
function dayTitle(ds) {
  const d = new Date(ds + 'T12:00:00Z');
  return `${WD[d.getUTCDay()]}, ${d.getUTCDate()} ${MONTHS[d.getUTCMonth()]}`;
}
function shortDay(ds) {
  const d = new Date(ds + 'T12:00:00Z');
  return `${WD[d.getUTCDay()]} ${String(d.getUTCDate()).padStart(2, '0')}.${String(d.getUTCMonth() + 1).padStart(2, '0')}`;
}
function hm(iso) {
  return new Date(iso).toLocaleTimeString('ru-RU', { timeZone: TZ, hour: '2-digit', minute: '2-digit' });
}
function mskDate(iso) {
  return new Intl.DateTimeFormat('en-CA', { timeZone: TZ }).format(new Date(iso));
}
// Москва без перехода на летнее время — смещение всегда +03:00.
function toISO(date, time) { return `${date}T${time}:00+03:00`; }
function mins(m) {
  if (m >= 60) return m % 60 ? `${Math.floor(m / 60)}ч${String(m % 60).padStart(2, '0')}` : `${m / 60}ч`;
  return `${m}м`;
}

// ── Справочники ──────────────────────────────────────────────────────────────

const state = { spheres: [], sphereById: new Map() };

async function loadRefs() {
  const [spheres, projects] = await Promise.all([api('GET', 'spheres'), api('GET', 'projects')]);
  // В тёмной теме сфера берёт свой тёмный вариант цвета, если он задан.
  if (matchMedia('(prefers-color-scheme: dark)').matches) {
    for (const sp of spheres) if (sp.style && sp.style.color_dark) sp.color = sp.style.color_dark;
  }
  state.spheres = spheres;
  state.sphereById = new Map(spheres.map((s) => [s.id, s]));
  state.projects = projects || [];
}

// «Группа / Проект» — путь по дереву групп.
function projectPath(p) {
  const byId = new Map(state.projects.map((x) => [x.id, x]));
  const parts = [p.name];
  for (let cur = p, guard = 0; cur.parent_id && guard < 10; guard++) {
    cur = byId.get(cur.parent_id);
    if (!cur) break;
    parts.unshift(cur.name);
  }
  return parts.join(' / ');
}

function sphereOf(it) { return it.sphere_id ? state.sphereById.get(it.sphere_id) : null; }

const QUADRANTS = {
  1: 'Q1 · сделать сейчас',
  2: 'Q2 · запланировать',
  3: 'Q3 · быстро/делегировать',
  4: 'Q4 · можно отложить',
};
const STATUSES = {
  inbox: 'Входящее', todo: 'К выполнению', doing: 'В работе', waiting: 'Жду', done: 'Готово', cancelled: 'Отменено', someday: 'Когда-нибудь',
};
const KINDS = { task: 'Задача', event: 'Событие', goal: 'Цель', note: 'Заметка' };

// ── Задача в списке ──────────────────────────────────────────────────────────

function itemRow(it, { withDate = false, onChange } = {}) {
  const sp = sphereOf(it);
  const closed = it.status === 'done' || it.status === 'cancelled';
  const meta = [];
  if (it.status === 'doing' || it.status === 'waiting' || it.status === 'someday') {
    meta.push(h('span', { class: 'tag' }, { doing: '▶ в работе', waiting: '⏸ жду', someday: '… когда-нибудь' }[it.status]));
  }
  if (sp) meta.push(h('span', {}, `${sp.icon} ${sp.name}`));
  if (withDate && it.planned_date && !it.start_at) meta.push(h('span', {}, shortDay(it.planned_date)));
  if (it.estimate_min || it.spent_min) {
    meta.push(h('span', {}, `~${it.estimate_min ? mins(it.estimate_min) : '?'}` + (it.spent_min ? ` · факт ${mins(it.spent_min)}` : '')));
  }
  if (it.deadline) {
    const late = !closed && mskDate(it.deadline) < todayStr();
    meta.push(h('span', { class: late ? 'warn' : '' }, `⏰ ${shortDay(mskDate(it.deadline))}`));
  }
  if (it.postpone_count > 0) meta.push(h('span', { class: it.postpone_count >= 2 ? 'warn' : '' }, `↻ ${it.postpone_count}`));
  for (const t of it.tags || []) meta.push(h('span', { class: 'tag' }, t));

  const time = it.start_at
    ? h('span', { class: 'time' }, (withDate ? shortDay(mskDate(it.start_at)) + ' ' : '') + hm(it.start_at) + (it.end_at ? '–' + hm(it.end_at) : ''))
    : null;

  return h('div', {
    class: `item q${it.quadrant} ${it.status}`,
    style: sp ? { '--sphere': sp.color } : {},
    dataset: { id: it.id },
  },
    h('button', {
      class: 'check', 'aria-label': `${closed ? 'Вернуть в работу' : 'Завершить'}: ${it.title}`, 'aria-pressed': String(it.status === 'done'),
      onclick: async (e) => {
        e.stopPropagation();
        const button = e.currentTarget;
        try {
          const next = it.status === 'done' || it.status === 'cancelled' ? 'todo' : 'done';
          const r = await api('PATCH', `items/${it.id}`, { status: next });
          if (r && r.queued) {
            // Без сети — показываем результат сразу, отправится позже.
            it.status = next;
            button.closest('.item').classList.toggle('done', next === 'done');
            button.textContent = next === 'done' ? '✓' : '';
            button.setAttribute('aria-pressed', String(next === 'done'));
            button.setAttribute('aria-label', `${next === 'done' ? 'Вернуть в работу' : 'Завершить'}: ${it.title}`);
            return;
          }
          onChange && onChange();
        } catch (err) { handleError(err); }
      },
    }, it.status === 'done' ? '✓' : ''),
    h('div', { class: 'body' },
      h('button', { class: 'title task-title-button', type: 'button', onclick: () => openEditor(it.id, onChange) }, time, it.kind !== 'task' && it.kind !== 'event' ? `${KINDS[it.kind]} · ` : '', it.title, it.recurrence_id ? ' 🔁' : ''),
      meta.length ? h('div', { class: 'meta' }, meta) : null,
      it.progress != null ? h('div', { class: 'row mini-progress', title: 'Прогресс по весу частей' },
        h('div', { class: 'progress' }, h('i', { style: { width: `${Math.round(it.progress * 100)}%` } })),
        h('span', {}, `${Math.round(it.progress * 100)}%`)) : null,
    ),
  );
}

function list(items, opts) {
  return h('div', { class: 'items' }, items.map((it) => itemRow(it, opts)));
}

// ── Экран «День» ─────────────────────────────────────────────────────────────

async function viewDay(date) {
  const day = await api('GET', `day/${date}`);
  const today = todayStr();
  const reload = () => render();
  const unique = (items) => [...new Map(items.map((it) => [it.id, it])).values()];
  const scheduled = unique(day.scheduled || []);
  const planned = unique(day.planned || []);
  const primaryIDs = new Set([...scheduled, ...planned].map((it) => it.id));
  const deadlines = unique(day.deadlines || []).filter((it) => !primaryIDs.has(it.id));
  const visibleIDs = new Set([...primaryIDs, ...deadlines.map((it) => it.id)]);
  const overdue = unique(day.overdue || []).filter((it) => !visibleIDs.has(it.id));
  const closed = (it) => it.status === 'done' || it.status === 'cancelled';
  const activeScheduled = scheduled.filter((it) => !closed(it));
  const activePlanned = planned.filter((it) => !closed(it));
  const completed = unique([...scheduled, ...planned]).filter(closed);
  const tasks = unique([...activeScheduled, ...activePlanned, ...deadlines]).filter((it) => it.kind !== 'event');

  const input = h('input', { id: 'day-quick-add', placeholder: 'Например, «созвон в 16»', enterkeyhint: 'done', autocomplete: 'off' });
  const addButton = h('button', { class: 'btn primary', type: 'submit' }, 'Добавить');
  const add = async (e) => {
    e?.preventDefault();
    const text = input.value.trim();
    if (!text || addButton.disabled) return;
    addButton.disabled = true;
    try {
      const p = await api('POST', 'quick', { text, date });
      input.value = '';
      if (p && p.preview) toast(p.preview.join(' · '));
      if (!p || !p.queued) reload();
    } catch (err) { handleError(err); }
    finally { addButton.disabled = false; }
  };

  const panel = (title, items, empty, opts = {}, extra = null) => h('section', { class: 'panel card' },
    h('div', { class: 'panel-head' }, h('h2', {}, title), h('span', { class: 'count' }, items.length), extra),
    items.length ? list(items, { onChange: reload, ...opts }) : h('div', { class: 'empty' }, empty));
  const stat = (value, label) => h('div', { class: 'summary-stat' },
    h('span', { class: 'summary-value' }, value), h('span', { class: 'summary-label' }, label));
  const dateHeading = new Date(date + 'T12:00:00Z').toLocaleDateString('ru-RU', { timeZone: TZ, weekday: 'long', day: 'numeric', month: 'long' });

  return [
    h('div', { class: 'page-head' },
      h('div', {}, h('div', { class: 'eyebrow' }, 'План дня'),
        h('h1', { class: 'page-title' }, date === today ? 'Сегодня' : date === addDays(today, 1) ? 'Завтра' : dayTitle(date).split(', ')[1]),
        h('p', { class: 'page-description' }, dateHeading)),
      h('div', { class: 'page-head-actions date-nav' },
        h('button', { class: 'btn icon', 'aria-label': 'Предыдущий день', onclick: () => go(`#/day/${addDays(date, -1)}`) }, '‹'),
        h('button', { class: 'btn', onclick: () => go('#/day') }, 'Сегодня'),
        h('button', { class: 'btn icon', 'aria-label': 'Следующий день', onclick: () => go(`#/day/${addDays(date, 1)}`) }, '›'))),
    h('div', { class: 'day-summary', 'aria-label': 'Обзор дня' },
      stat(tasks.length, 'Дел на день'), stat(mins(day.plan_min || 0), 'Оценка задач'),
      stat(activeScheduled.length, 'В расписании'), stat(mins(day.busy_min || 0), 'Время в расписании')),
    h('form', { class: 'quick card', onsubmit: add },
      h('label', { class: 'quick-label', for: 'day-quick-add' }, 'Что нужно сделать?'),
      h('div', { class: 'quick-controls' }, input, addButton),
      h('div', { class: 'quick-hint' }, 'Дату, время и длительность можно написать словами.')),
    h('div', { class: 'day-layout' },
      h('div', { class: 'day-main' },
        panel('Задачи на день', activePlanned, 'План свободен. Добавьте первое дело выше.', {},
          h('button', { class: 'btn panel-add', 'aria-label': 'Новая задача на этот день', onclick: () => openEditor(null, reload, { planned_date: date }) }, '+ Задача')),
        deadlines.length ? panel('Дедлайны сегодня', deadlines, '', { withDate: true }) : null,
        overdue.length ? h('details', { class: 'day-fold card overdue-fold' },
          h('summary', {}, h('span', { class: 'fold-title' }, 'С прошлых дней'), h('span', { class: 'count' }, overdue.length)),
          h('p', { class: 'fold-hint' }, 'Выберите, что перенести в этот день.'),
          h('div', { class: 'items' }, overdue.map((it) => {
            const row = itemRow(it, { withDate: true, onChange: reload });
            row.append(h('button', { class: 'btn side', 'aria-label': `Перенести на ${shortDay(date)}: ${it.title}`, onclick: async () => {
              try { await api('PATCH', `items/${it.id}`, { planned_date: date }); reload(); } catch (err) { handleError(err); }
            } }, 'На этот день'));
            return row;
          }))) : null,
        completed.length ? h('details', { class: 'day-fold card completed-fold' },
          h('summary', {}, h('span', { class: 'fold-title' }, 'Завершено'), h('span', { class: 'count' }, completed.length)),
          list(completed, { onChange: reload })) : null),
      h('aside', { class: 'day-side', 'aria-label': 'Расписание дня' },
        panel('Расписание', activeScheduled, 'Встреч и дел с точным временем пока нет.'))),
  ];
}

// ── Экран «Неделя» ───────────────────────────────────────────────────────────

async function viewWeek(date) {
  const monday = mondayOf(date);
  const days = await api('GET', `week/${monday}`);
  const today = todayStr();
  const mini = (it) => h('button', {
    class: `mini ${it.status}`, style: sphereOf(it) ? { '--sphere': sphereOf(it).color } : {},
    onclick: () => openEditor(it.id, render),
  }, it.start_at ? `${hm(it.start_at)} ` : '', it.title, it.recurrence_id ? ' 🔁' : '');

  return [
    h('div', { class: 'bar' },
      h('button', { class: 'btn icon', 'aria-label': 'Предыдущая неделя', onclick: () => go(`#/week/${addDays(monday, -7)}`) }, '‹'),
      h('h1', {}, `${shortDay(monday)} — ${shortDay(addDays(monday, 6))}`),
      h('button', { class: 'btn', onclick: () => go('#/week') }, 'Эта неделя'),
      h('button', { class: 'btn icon', 'aria-label': 'Следующая неделя', onclick: () => go(`#/week/${addDays(monday, 7)}`) }, '›'),
    ),
    h('div', { class: 'week' }, days.map((d) =>
      h('div', { class: `wday card ${d.date === today ? 'today' : ''}` },
        h('h3', { onclick: () => go(`#/day/${d.date}`) }, shortDay(d.date),
          h('span', { class: 'load' }, d.busy_min + d.plan_min ? mins(d.busy_min + d.plan_min) : '')),
        [...d.scheduled, ...d.planned].map(mini),
        d.deadlines.map((it) => h('button', { class: 'mini', onclick: () => openEditor(it.id, render) }, '⏰ ', it.title)),
      ))),
  ];
}

// ── Экран «Доска» ────────────────────────────────────────────────────────────

const COLUMNS = [['todo', 'К выполнению'], ['doing', 'В работе'], ['waiting', 'Жду'], ['done', 'Готово · 7 дней']];
let boardSphere = null;
let boardStatus = null;

async function viewBoard() {
  const all = await api('GET', 'items?status=todo,doing,waiting&done_days=7&limit=500');
  const today = todayStr();
  // Доска — про дела: без событий и заметок; повторы — только на сегодня и раньше,
  // иначе две недели тренировок забьют колонку.
  const items = all.filter((it) => {
    if (it.kind === 'event' || it.kind === 'note') return false;
    if (boardSphere !== null && it.sphere_id !== boardSphere) return false;
    if (it.recurrence_id) {
      const d = it.planned_date || (it.start_at && mskDate(it.start_at));
      if (d && d > today) return false;
    }
    return true;
  });
  items.sort((a, b) => a.quadrant - b.quadrant || (a.planned_date || '9').localeCompare(b.planned_date || '9'));

  const chips = h('div', { class: 'chips' },
    h('button', { class: `chip ${boardSphere === null ? 'on' : ''}`, 'aria-pressed': String(boardSphere === null), onclick: () => { boardSphere = null; render(); } }, 'Все'),
    state.spheres.map((s) => h('button', {
      class: `chip ${boardSphere === s.id ? 'on' : ''}`, 'aria-pressed': String(boardSphere === s.id), onclick: () => { boardSphere = s.id; render(); },
    }, `${s.icon} ${s.name}`)));

  const move = async (id, status) => {
    try { await api('PATCH', `items/${id}`, { status }); render(); } catch (err) { handleError(err); }
  };

  const statusFilter = h('select', { class: 'board-status-filter', 'aria-label': 'Показать статус на доске', onchange: (e) => { boardStatus = e.target.value || null; render(); } },
    h('option', { value: '', selected: boardStatus === null }, 'Все статусы'),
    COLUMNS.map(([status, title]) => h('option', { value: status, selected: boardStatus === status }, title)));

  const cols = COLUMNS.filter(([status]) => boardStatus === null || boardStatus === status).map(([status, title]) => {
    const colItems = items.filter((it) => it.status === status);
    const col = h('div', { class: 'col', dataset: { status } },
      h('h2', {}, title, h('span', { class: 'load' }, String(colItems.length))),
      h('div', { class: 'items' }, colItems.map((it) => {
        const row = itemRow(it, { withDate: true, onChange: render });
        row.draggable = true;
        row.addEventListener('dragstart', (e) => e.dataTransfer.setData('text/plain', it.id));
        row.append(h('select', { class: 'board-move', 'aria-label': `Статус: ${it.title}`, onchange: (e) => move(it.id, e.target.value) },
          COLUMNS.map(([value, label]) => h('option', { value, selected: value === it.status }, label.replace(' · 7 дней', '')))));
        return row;
      })));
    col.addEventListener('dragover', (e) => { e.preventDefault(); col.classList.add('drop'); });
    col.addEventListener('dragleave', () => col.classList.remove('drop'));
    col.addEventListener('drop', (e) => {
      e.preventDefault();
      col.classList.remove('drop');
      const id = e.dataTransfer.getData('text/plain');
      if (id) move(id, status);
    });
    return col;
  });

  return [
    h('div', { class: 'page-head' }, h('div', {}, h('div', { class: 'eyebrow' }, 'Рабочий процесс'),
      h('h1', { class: 'page-title' }, 'Доска'), h('p', { class: 'page-description' }, 'Все дела по статусам. Меняйте статус в карточке или перетаскивайте её.'))),
    h('div', { class: 'board-toolbar' }, chips, statusFilter),
    h('div', { class: `board${boardStatus ? ' board-filtered' : ''}` }, cols),
  ];
}

// ── Экран «Входящие» ─────────────────────────────────────────────────────────

async function viewInbox() {
  const msgs = await api('GET', 'inbox');
  const input = h('textarea', { id: 'inbox-capture', rows: 3, placeholder: 'Идея, дело или мысль — запишите, пока помните.' });
  const addButton = h('button', { class: 'btn primary', type: 'submit' }, 'Сохранить во входящие');
  const add = async (e) => {
    e?.preventDefault();
    const text = input.value.trim();
    if (!text || addButton.disabled) return;
    addButton.disabled = true;
    try { await api('POST', 'inbox', { text }); input.value = ''; render(); } catch (err) { handleError(err); }
    finally { addButton.disabled = false; }
  };
  const resolve = async (id, status) => {
    try { await api('POST', `inbox/${id}/resolve`, { status }); render(); } catch (err) { handleError(err); }
  };
  const statusLabel = { new: 'Новая запись', proposed: 'Подтвердите в боте', deferred: 'На вечерний разбор' };
  const row = (m) => h('article', { class: 'inbox-row card' },
    h('div', { class: 'text' },
      h('div', { class: 'inbox-message' }, m.transcript || m.text),
      (m.files || []).map((b) => fileBlock(b)),
      h('div', { class: 'when' }, `${shortDay(mskDate(m.created_at))} · ${hm(m.created_at)}`, h('span', { class: 'tag' }, statusLabel[m.status] || m.status)),
      m.parse_error ? h('div', { class: 'err' }, 'Не удалось разобрать: ' + m.parse_error) : null),
    h('div', { class: 'acts' },
      h('button', { class: 'btn primary', onclick: async () => {
        try {
          const p = await api('POST', 'quick', { inbox_id: m.id });
          if (p && p.preview) toast(p.preview.filter((l) => !l.startsWith('📥')).join(' · '));
          render();
        } catch (err) { handleError(err); }
      } }, 'Создать задачу'),
      m.status !== 'deferred' ? h('button', { class: 'btn', onclick: () => resolve(m.id, 'deferred') }, 'На вечер') : null,
      h('button', { class: 'btn danger', 'aria-label': `Удалить запись: ${(m.transcript || m.text || '').slice(0, 60)}`, onclick: () => resolve(m.id, 'rejected') }, 'Удалить')));
  const pending = msgs.filter((m) => m.status !== 'deferred');
  const deferred = msgs.filter((m) => m.status === 'deferred');
  const group = (title, items) => items.length ? h('section', { class: 'inbox-group' },
    h('div', { class: 'panel-head' }, h('h2', {}, title), h('span', { class: 'count' }, items.length)),
    h('div', { class: 'inbox-list' }, items.map(row))) : null;

  return [
    h('div', { class: 'page-head' }, h('div', {}, h('div', { class: 'eyebrow' }, 'Освободить голову'),
      h('h1', { class: 'page-title' }, 'Входящие'), h('p', { class: 'page-description' }, 'Соберите мысли здесь. Превратите их в задачи или оставьте на вечер.'))),
    h('form', { class: 'inbox-composer card', onsubmit: add },
      h('label', { class: 'field', for: 'inbox-capture' }, 'Быстрая запись', input),
      h('div', { class: 'actions' }, h('span', { class: 'hint' }, 'При создании задачи дата, время и повторы распознаются автоматически.'), addButton)),
    msgs.length ? [group('Разобрать', pending), group('На вечер', deferred)] : h('div', { class: 'empty card' }, 'Всё разобрано. Здесь появятся новые записи.'),
    await queueSection(),
    h('div', { class: 'footer-links' }, h('button', { onclick: logout }, 'Выйти на этом устройстве')),
  ];
}

// ── Карточка задачи ──────────────────────────────────────────────────────────

let sheetReturnFocus = null;
let sheetInertElements = [];

function closeSheet() {
  const sheet = $('sheet');
  const wasOpen = !sheet.hidden;
  sheet.hidden = true;
  $('sheet-backdrop').hidden = true;
  sheet.replaceChildren();
  sheet.onkeydown = null;
  sheet.onpaste = null;
  document.body.classList.remove('sheet-open');
  for (const [el, previous] of sheetInertElements) el.inert = previous;
  sheetInertElements = [];
  if (wasOpen) {
    const target = sheetReturnFocus?.isConnected ? sheetReturnFocus : $('fab');
    if (target && !target.hidden) target.focus({ preventScroll: true });
  }
  sheetReturnFocus = null;
}

// Минимальный markdown → DOM: абзацы, списки «- », **жирный**, `код`, ссылки.
function inlineMd(text) {
  const out = [];
  const re = /(\*\*[^*]+\*\*|`[^`]+`|\[[^\]]+\]\(https?:\/\/[^)\s]+\)|https?:\/\/\S+)/g;
  let last = 0, m;
  while ((m = re.exec(text))) {
    if (m.index > last) out.push(text.slice(last, m.index));
    const t = m[0];
    if (t.startsWith('**')) out.push(h('b', {}, t.slice(2, -2)));
    else if (t.startsWith('`')) out.push(h('code', {}, t.slice(1, -1)));
    else if (t.startsWith('[')) {
      const [, label, url] = t.match(/^\[([^\]]+)\]\((.+)\)$/);
      out.push(h('a', { href: url, target: '_blank', rel: 'noopener' }, label));
    } else out.push(h('a', { href: t, target: '_blank', rel: 'noopener' }, t));
    last = m.index + t.length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}
function md(text) {
  return h('div', { class: 'blk-md' }, text.split(/\n{2,}/).map((para) => {
    const lines = para.split('\n');
    if (lines.every((l) => /^\s*[-*] /.test(l))) {
      return h('ul', {}, lines.map((l) => h('li', {}, inlineMd(l.replace(/^\s*[-*] /, '')))));
    }
    return h('p', {}, lines.flatMap((l, i) => (i ? [h('br'), ...inlineMd(l)] : inlineMd(l))));
  }));
}

// Блоки body: md-блоки и чек-листы редактируются, остальные — только показываются.
function renderBlocks(body, onChange) {
  return h('div', { class: 'blocks' }, body.map((b, idx) => {
    switch (b.type) {
      case 'md': {
        const ta = h('textarea', { rows: Math.min(10, Math.max(3, (b.text || '').split('\n').length + 1)) });
        ta.value = b.text || '';
        ta.addEventListener('input', () => { body[idx] = { ...b, text: ta.value }; onChange(); });
        return h('label', { class: 'field' }, 'Заметка', ta);
      }
      case 'checklist':
        return h('div', { class: 'blk-check' }, (b.items || []).map((ci, j) => {
          const cb = h('input', { type: 'checkbox', checked: !!ci.done });
          const label = h('label', { class: ci.done ? 'done' : '' }, cb, h('span', {}, ci.text));
          cb.addEventListener('change', () => {
            b.items[j] = { ...ci, done: cb.checked };
            label.classList.toggle('done', cb.checked);
            onChange();
          });
          return label;
        }));
      case 'link':
        return h('a', { href: b.url, target: '_blank', rel: 'noopener' }, '🔗 ', b.title || b.url);
      case 'callout':
        return h('div', { class: `blk-callout ${b.tone || ''}` }, inlineMd(b.text || ''));
      case 'code':
        return h('pre', { class: 'blk-code' }, b.text || '');
      case 'comment':
        return h('div', { class: 'blk-comment' }, `${b.author === 'claude' ? '🤖' : '💬'} ${b.text || ''}`);
      case 'table':
        return h('table', { class: 'blk-table' },
          h('tr', {}, (b.columns || []).map((c) => h('th', {}, c))),
          (b.rows || []).map((r) => h('tr', {}, r.map((c) => h('td', {}, c)))));
      case 'file':
        return fileBlock(b, () => { body[idx] = { type: '_removed' }; onChange(); });
      case 'ref':
        return h('div', { class: 'blk-comment' }, '↗ связанная задача');
      default:
        return b.text ? md(b.text) : null;
    }
  }));
}

// ── Файлы ────────────────────────────────────────────────────────────────────

function fileUrl(b) { return '/api/files/' + encodeURIComponent(b.file_id); }
function fmtSize(n) {
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1).replace('.0', '') + ' МБ';
  if (n >= 1024) return Math.round(n / 1024) + ' КБ';
  return n + ' Б';
}
const IMAGE = /^image\/(png|jpeg|gif|webp)$/;

// Вложение: картинка — превью, остальное — ссылка. onRemove — убрать из задачи
// (сам файл остаётся в Drive: случайно убранное можно найти там).
function fileBlock(b, onRemove) {
  if (!b.file_id) return h('div', {}, '📎 ', b.name || 'файл');
  const box = h('div', { class: 'blk-file' },
    h('a', { href: fileUrl(b), target: '_blank', rel: 'noopener' },
      IMAGE.test(b.mime || '') ? h('img', { src: fileUrl(b), alt: b.name || '', loading: 'lazy' }) : h('span', { class: 'ico' }, '📎'),
      h('span', { class: 'name' }, b.name || 'файл'),
      b.size ? h('span', { class: 'size' }, fmtSize(b.size)) : null),
    onRemove ? h('button', { class: 'btn icon', type: 'button', title: 'Убрать из задачи', 'aria-label': `Убрать «${b.name}»`, onclick: () => {
      if (!confirm(`Убрать «${b.name}» из задачи? Файл останется в Google Drive.`)) return;
      box.remove();
      onRemove();
    } }, '×') : null);
  return box;
}

// Загрузка — только при сети: файлы в офлайн-очередь не кладём (тяжёлые).
async function uploadFiles(id, list) {
  const fd = new FormData();
  for (const f of list) fd.append('file', f, f.name || 'file');
  let res;
  try {
    res = await fetch(`/api/items/${id}/files`, { method: 'POST', credentials: 'same-origin', headers: { 'X-Requested-With': 'lifetask' }, body: fd });
  } catch {
    throw new Error('файлы загружаются только при сети');
  }
  if (res.status === 401) throw new AuthError('нужен вход');
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error((data && data.error) || res.statusText);
  return data;
}

function seg(options, value, onPick) {
  const wrap = h('div', { class: 'seg', role: 'group' });
  const draw = (v) => wrap.replaceChildren(...Object.entries(options).map(([k, label]) =>
    h('button', { type: 'button', class: k === v ? 'on' : '', 'aria-pressed': String(k === v), onclick: () => { draw(k); onPick(k); } }, label)));
  draw(value);
  return wrap;
}

async function openEditor(id, onDone, preset = {}) {
  if ($('sheet').hidden) sheetReturnFocus = document.activeElement;
  let detail;
  try {
    detail = id ? await api('GET', `items/${id}`) : {
      kind: 'task', title: '', status: 'todo', important: false, urgent: false, body: [], tags: [],
      children: [], relations: [], time: [], weight: 1, ...preset,
    };
  } catch (err) { return handleError(err); }

  const it = structuredClone(detail);
  const body = structuredClone(detail.body || []);
  let bodyChanged = false;

  const title = h('input', { class: 'title-input', placeholder: 'Название', value: it.title, 'aria-label': 'Название' });

  const sphere = h('select', {}, h('option', { value: '' }, '— без сферы —'),
    state.spheres.map((s) => h('option', { value: s.id, selected: s.id === it.sphere_id }, `${s.icon} ${s.name}`)));
  const status = h('select', {}, Object.entries(STATUSES).map(([k, v]) => h('option', { value: k, selected: k === it.status }, v)));

  const project = h('select', {}, h('option', { value: '' }, '— без проекта —'),
    [...(state.projects || [])].sort((a, b) => projectPath(a).localeCompare(projectPath(b)))
      .map((p) => h('option', { value: p.id, selected: p.id === it.project_id }, projectPath(p))));
  const newProject = h('button', { class: 'btn icon', type: 'button', title: 'Новый проект', 'aria-label': 'Создать проект', onclick: async () => {
    const name = prompt('Название проекта (группу можно задать позже):');
    if (!name || !name.trim()) return;
    try {
      const p = await api('POST', 'projects', { name: name.trim(), sphere_id: sphere.value ? Number(sphere.value) : null });
      state.projects.push(p);
      project.append(h('option', { value: p.id, selected: true }, p.name));
      project.value = String(p.id);
    } catch (err) { handleError(err); }
  } }, '+');

  // Повтор — только при создании: серия создаётся вместо одной задачи.
  const repeat = h('select', {},
    h('option', { value: '' }, 'не повторяется'),
    h('option', { value: 'FREQ=DAILY' }, 'каждый день'),
    h('option', { value: 'FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR' }, 'по будням'),
    h('option', { value: 'weekly' }, 'каждую неделю (в день даты)'),
    h('option', { value: 'FREQ=MONTHLY' }, 'каждый месяц (в число даты)'),
    h('option', { value: 'custom' }, 'своё правило (RRULE)…'));
  const stopRepeat = it.recurrence_id ? h('button', { class: 'btn', type: 'button', onclick: async () => {
    const from = it.occurrence_date || todayStr();
    if (!confirm(`Остановить повтор с ${shortDay(from)}? Будущие несделанные экземпляры удалятся.`)) return;
    try {
      const r = await api('POST', `recurrences/${it.recurrence_id}/stop`, { from });
      closeSheet();
      toast(`Повтор остановлен, убрано: ${(r && r.removed) || 0}`);
      onDone && onDone();
    } catch (err) { handleError(err); }
  } }, '⏹ Остановить повтор') : null;

  const qlabel = h('span', { class: `qlabel q${detail.quadrant || 4}` });
  const important = h('input', { type: 'checkbox', checked: it.important });
  const urgent = h('input', { type: 'checkbox', checked: it.urgent });
  const drawQ = () => {
    const q = important.checked && urgent.checked ? 1 : important.checked ? 2 : urgent.checked ? 3 : 4;
    qlabel.className = `qlabel q${q}`;
    qlabel.textContent = QUADRANTS[q];
  };
  important.addEventListener('change', drawQ);
  urgent.addEventListener('change', drawQ);
  drawQ();

  const baseDate = it.start_at ? mskDate(it.start_at) : it.planned_date || '';
  const date = h('input', { type: 'date', value: baseDate });
  const from = h('input', { type: 'time', value: it.start_at ? hm(it.start_at) : '' });
  const to = h('input', { type: 'time', value: it.end_at ? hm(it.end_at) : '' });
  const deadline = h('input', { type: 'date', value: it.deadline ? mskDate(it.deadline) : '' });
  const estimate = h('input', { type: 'number', min: 0, step: 5, inputmode: 'numeric', value: it.estimate_min ?? '' });
  const tags = h('input', { value: (it.tags || []).join(', '), placeholder: '@комп, @звонок' });
  const weight = h('input', { type: 'number', min: 0, step: 1, inputmode: 'numeric', value: it.weight ?? 1 });

  let kind = it.kind;
  const kindSeg = seg(KINDS, kind, (k) => (kind = k));
  kindSeg.setAttribute('aria-label', 'Тип записи');

  // Учёт времени: добавить минуты (секундомер на часах → сюда).
  const spent = h('span', {}, it.spent_min ? `факт ${mins(it.spent_min)}` : 'факт —');
  const addMin = h('input', { type: 'number', min: 1, inputmode: 'numeric', 'aria-label': 'Добавить потраченные минуты', placeholder: 'мин', style: { width: '80px' } });
  const timeRow = id ? h('div', { class: 'row' }, spent, addMin, h('button', {
    class: 'btn', type: 'button', onclick: async () => {
      const m = parseInt(addMin.value, 10);
      if (!m) return;
      try {
        await api('POST', `items/${id}/time`, { minutes: m, source: 'web' });
        it.spent_min = (it.spent_min || 0) + m;
        spent.textContent = `факт ${mins(it.spent_min)}`;
        addMin.value = '';
        toast(`+${mins(m)}`);
      } catch (err) { handleError(err); }
    },
  }, '+ время')) : null;

  // Подзадачи с прогрессом по весам.
  const subs = id ? (() => {
    const box = h('div', { class: 'subtasks' });
    const draw = () => {
      const kids = detail.children || [];
      box.replaceChildren(...[
        h('div', { class: 'section' }, `Подзадачи${detail.progress != null ? ` · ${Math.round(detail.progress * 100)}%` : ''}`),
        detail.progress != null ? h('div', { class: 'progress' }, h('i', { style: { width: `${detail.progress * 100}%` } })) : null,
        h('div', { class: 'items' }, kids.map((kid) => {
          const row = itemRow(kid, { onChange: async () => { detail = await api('GET', `items/${id}`); draw(); } });
          const w = h('input', { type: 'number', min: 0, step: 1, value: kid.weight, class: 'weight', title: 'Вес', 'aria-label': `Вес «${kid.title}»` });
          w.addEventListener('change', async () => {
            try { await api('PATCH', `items/${kid.id}`, { weight: Number(w.value) || 0 }); detail = await api('GET', `items/${id}`); draw(); } catch (err) { handleError(err); }
          });
          row.append(h('label', { class: 'side weight-box' }, 'вес', w));
          return row;
        })),
      ].filter(Boolean));
      const subInput = h('input', { placeholder: 'Новая подзадача', 'aria-label': 'Новая подзадача' });
      subInput.addEventListener('keydown', async (e) => {
        if (e.key !== 'Enter' || !subInput.value.trim()) return;
        try {
          await api('POST', 'items', { title: subInput.value.trim(), parent_id: id, sphere_id: it.sphere_id, planned_date: it.planned_date });
          detail = await api('GET', `items/${id}`);
          draw();
        } catch (err) { handleError(err); }
      });
      box.append(h('div', { class: 'quick' }, subInput));
    };
    draw();
    return box;
  })() : null;

  // Без md-блока — даём поле для заметки (добавится при сохранении, если не пустое).
  if (!body.some((b) => b.type === 'md')) body.unshift({ type: 'md', text: '' });
  const blocksEl = renderBlocks(body, () => (bodyChanged = true));

  // Вложения: кнопка, перетаскивание, вставка из буфера. У новой задачи файлы
  // ждут сохранения — загрузить их можно только к уже созданной.
  const pending = [];
  const pendingList = h('div', { class: 'pending-files' });
  const attach = async (list) => {
    list = [...list];
    if (!list.length) return;
    if (!id) {
      pending.push(...list);
      pendingList.replaceChildren(...pending.map((f) => h('div', {}, '📎 ', f.name, h('span', { class: 'size' }, ' — загрузится при сохранении'))));
      return;
    }
    toast(`Загружаю: ${list.length}…`);
    try {
      const res = await uploadFiles(id, list);
      const have = new Set(body.filter((b) => b.type === 'file').map((b) => b.file_id));
      for (const b of (res && res.body) || []) {
        if (b.type !== 'file' || have.has(b.file_id)) continue;
        body.push(b);
        blocksEl.append(fileBlock(b, () => { body[body.indexOf(b)] = { type: '_removed' }; bodyChanged = true; }));
      }
      toast(`📎 Прикреплено: ${list.length}`);
      onDone && onDone();
    } catch (err) { handleError(err); }
  };
  const fileInput = h('input', { type: 'file', multiple: true, hidden: true });
  fileInput.addEventListener('change', () => { attach(fileInput.files); fileInput.value = ''; });
  const dropZone = h('div', { class: 'dropzone' },
    h('button', { class: 'btn', type: 'button', onclick: () => fileInput.click() }, '📎 Прикрепить файл'),
    h('span', { class: 'hint' }, 'или перетащи сюда / вставь из буфера'), fileInput, pendingList);
  dropZone.addEventListener('dragover', (e) => { e.preventDefault(); dropZone.classList.add('over'); });
  dropZone.addEventListener('dragleave', () => dropZone.classList.remove('over'));
  dropZone.addEventListener('drop', (e) => { e.preventDefault(); dropZone.classList.remove('over'); attach(e.dataTransfer.files); });

  const save = async () => {
    const patch = {};
    // Для новой задачи сравниваем с пустой — иначе предзаполненная дата не уйдёт на сервер.
    const base = id ? detail : {};
    const set = (k, v) => { if (JSON.stringify(v) !== JSON.stringify(base[k] ?? null)) patch[k] = v; };
    const t = title.value.trim();
    if (!t) { title.setAttribute('aria-invalid', 'true'); title.focus(); return toast('Нужно название'); }
    title.removeAttribute('aria-invalid');
    set('title', t);
    set('kind', kind);
    set('status', status.value);
    set('sphere_id', sphere.value ? Number(sphere.value) : null);
    set('project_id', project.value ? Number(project.value) : null);
    set('important', important.checked);
    set('urgent', urgent.checked);
    if (date.value && from.value) {
      set('start_at', new Date(toISO(date.value, from.value)).toISOString());
      set('end_at', to.value ? new Date(toISO(date.value, to.value)).toISOString() : null);
      set('planned_date', date.value);
    } else {
      set('start_at', null);
      set('end_at', null);
      set('planned_date', date.value || null);
    }
    set('deadline', deadline.value ? new Date(toISO(deadline.value, '23:59')).toISOString() : null);
    set('estimate_min', estimate.value === '' ? null : Number(estimate.value));
    if (weight.value !== '') set('weight', Number(weight.value));
    const tagList = tags.value.split(',').map((s) => s.trim()).filter(Boolean);
    if (JSON.stringify(tagList) !== JSON.stringify(base.tags || [])) patch.tags = tagList;
    if (bodyChanged) patch.body = body.filter((b) => b.type !== '_removed' && !(b.type === 'md' && !b.text.trim()));
    // Время сравниваем как моменты, а не строки (сервер отдаёт другое представление).
    for (const k of ['start_at', 'end_at', 'deadline']) {
      if (k in patch && patch[k] && base[k] && new Date(patch[k]).getTime() === new Date(base[k]).getTime()) delete patch[k];
    }
    if (!id && repeat.value) {
      let rule = repeat.value;
      const start = date.value || todayStr();
      if (rule === 'weekly') rule = 'FREQ=WEEKLY;BYDAY=' + ['SU', 'MO', 'TU', 'WE', 'TH', 'FR', 'SA'][weekday(start)];
      if (rule === 'FREQ=MONTHLY') rule += ';BYMONTHDAY=' + Number(start.slice(8));
      if (rule === 'custom') {
        rule = prompt('Правило: например FREQ=WEEKLY;BYDAY=TU,TH или FREQ=WEEKLY;INTERVAL=2;BYDAY=MO');
        if (!rule) return;
      }
      const tpl = { ...patch };
      for (const k of ['planned_date', 'start_at', 'end_at', 'deadline', 'status']) delete tpl[k];
      const req = { rule, start, item: { ...tpl, title: t, kind } };
      if (from.value) {
        req.time = from.value;
        if (to.value && to.value > from.value) {
          const [h1, m1] = from.value.split(':').map(Number), [h2, m2] = to.value.split(':').map(Number);
          req.duration_min = h2 * 60 + m2 - (h1 * 60 + m1);
        }
      }
      try {
        await api('POST', 'recurrences', req);
        closeSheet();
        toast('🔁 Повтор создан на две недели вперёд');
        onDone && onDone();
      } catch (err) { handleError(err); }
      return;
    }
    try {
      if (id) {
        if (Object.keys(patch).length) await api('PATCH', `items/${id}`, patch);
      } else {
        const created = await api('POST', 'items', { ...patch, title: t, kind });
        if (pending.length) {
          if (created && created.id) {
            await uploadFiles(created.id, pending).catch((err) => toast('⚠️ Задача создана, файлы не загрузились: ' + err.message));
          } else toast('Задача в очереди, а файлы без сети не загрузить — прикрепи позже');
        }
      }
      closeSheet();
      onDone && onDone();
    } catch (err) { handleError(err); }
  };

  const remove = async () => {
    if (!confirm('Удалить задачу совсем? Если она просто не нужна — лучше статус «Отменено».')) return;
    try { await api('DELETE', `items/${id}`); closeSheet(); onDone && onDone(); } catch (err) { handleError(err); }
  };

  const f = (label, el, cls) => h('label', { class: `field ${cls || ''}` }, label, el);

  const saveButton = h('button', { class: 'btn primary', type: 'button', onclick: async () => {
    if (saveButton.disabled) return;
    saveButton.disabled = true;
    try { await save(); } finally { saveButton.disabled = false; }
  } }, 'Сохранить');
  const editorSection = (heading, ...nodes) => h('section', { class: 'editor-section' },
    h('h3', { class: 'section' }, heading), ...nodes);
  const sheet = $('sheet');
  sheet.replaceChildren(...[
    h('div', { class: 'sheet-head' },
      h('div', {}, h('div', { class: 'eyebrow' }, id ? 'Редактирование' : 'Новое дело'),
        h('h2', { id: 'editor-heading' }, id ? 'Карточка задачи' : 'Новая задача')),
      h('button', { class: 'btn icon', type: 'button', 'aria-label': 'Закрыть карточку', onclick: closeSheet }, '×')),
    f('Название', title, 'editor-title'),
    editorSection('Основное', h('div', { class: 'grid' },
      f('Статус', status), f('Сфера', sphere), f('Дата', date), f('Оценка, мин', estimate))),
    h('details', { class: 'editor-details', open: !!it.start_at || it.kind === 'event' },
      h('summary', {}, 'Дополнительные параметры'),
      h('div', { class: 'grid' },
        h('div', { class: 'field wide' }, h('span', {}, 'Тип записи'), kindSeg),
        h('label', { class: 'field wide' }, 'Проект', h('div', { class: 'row' }, project, newProject)),
        h('div', { class: 'wide row' },
          h('label', { class: 'toggle' }, important, 'Важно'),
          h('label', { class: 'toggle' }, urgent, 'Срочно'), qlabel),
        f('Начало, МСК', from), f('Окончание, МСК', to),
        f('Дедлайн', deadline), f('Теги', tags),
        it.parent_id || (detail.relations || []).some((r) => r.type === 'part_of' && r.from_id === it.id)
          ? f('Вес в прогрессе родителя', weight) : null,
        timeRow ? h('div', { class: 'wide' }, timeRow) : null,
        it.postpone_count ? h('div', { class: 'wide field' }, `Переносов: ${it.postpone_count}`) : null,
        it.recurrence_id ? h('div', { class: 'wide row field' }, 'Экземпляр повтора', stopRepeat) : null,
        !id ? f('Повтор', repeat, 'wide') : null)),
    editorSection('Заметки и файлы', blocksEl, dropZone),
    subs,
    id ? h('details', { class: 'editor-details' }, h('summary', {}, 'Связи с другими задачами'),
      relationsBlock(detail, () => openEditor(id, onDone))) : null,
    h('div', { class: 'actions sheet-actions' },
      id ? h('button', { class: 'btn danger', type: 'button', onclick: remove }, 'Удалить') : null,
      h('span', { class: 'spacer' }),
      h('button', { class: 'btn', type: 'button', onclick: closeSheet }, 'Отмена'), saveButton),
  ].filter(Boolean));
  sheet.hidden = false;
  sheet.setAttribute('aria-labelledby', 'editor-heading');
  sheet.setAttribute('tabindex', '-1');
  $('sheet-backdrop').hidden = false;
  document.body.classList.add('sheet-open');
  if (!sheetInertElements.length) {
    sheetInertElements = [...document.body.children]
      .filter((el) => !el.contains(sheet) && !el.contains($('sheet-backdrop')) && !el.contains($('toast')))
      .map((el) => [el, el.inert]);
    for (const [el] of sheetInertElements) el.inert = true;
  }
  sheet.onkeydown = (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') { e.preventDefault(); saveButton.click(); return; }
    if (e.key !== 'Tab') return;
    const focusable = [...sheet.querySelectorAll('button, input, select, textarea, a[href], summary, [tabindex]')]
      .filter((el) => !el.disabled && !el.hidden && el.tabIndex >= 0 && el.getClientRects().length);
    if (!focusable.length) { e.preventDefault(); sheet.focus(); return; }
    const first = focusable[0], last = focusable[focusable.length - 1];
    if (e.shiftKey && (document.activeElement === first || document.activeElement === sheet)) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  };
  sheet.onpaste = (e) => {
    const list = [...(e.clipboardData && e.clipboardData.files) || []];
    if (list.length) { e.preventDefault(); attach(list); }
  };
  title.focus({ preventScroll: true });
}

// ── Подсказка (общая для всех графиков) ─────────────────────────────────────

function showTip(e, nodes) {
  const tip = $('tip');
  tip.replaceChildren(...nodes);
  tip.hidden = false;
  const r = (e.target.getBoundingClientRect && e.clientX === undefined) ? e.target.getBoundingClientRect() : null;
  const x = r ? r.left + r.width / 2 : e.clientX;
  const y = r ? r.top : e.clientY;
  const w = tip.offsetWidth, hgt = tip.offsetHeight;
  tip.style.left = Math.min(window.innerWidth - w - 8, Math.max(8, x - w / 2)) + 'px';
  tip.style.top = Math.max(8, y - hgt - 12) + 'px';
}
function hideTip() { $('tip').hidden = true; }

// Подсказка на элементе: и мышь, и фокус с клавиатуры.
function tipOn(el, build) {
  el.setAttribute('tabindex', '0');
  el.addEventListener('pointermove', (e) => showTip(e, build()));
  el.addEventListener('focus', (e) => showTip(e, build()));
  el.addEventListener('pointerleave', hideTip);
  el.addEventListener('blur', hideTip);
  return el;
}

const NS = 'http://www.w3.org/2000/svg';
function s(tag, attrs, ...kids) {
  const el = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs || {})) if (v !== undefined && v !== null) el.setAttribute(k, v);
  for (const kid of kids.flat(Infinity)) if (kid !== null && kid !== undefined) el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  return el;
}

// «Красивый» верх оси: 1, 2, 5 × 10^n.
function niceMax(v) {
  if (v <= 0) return 1;
  const p = 10 ** Math.floor(Math.log10(v));
  return [1, 2, 5, 10].map((m) => m * p).find((m) => m >= v);
}

// Графики рисуются после вставки в DOM — нужна реальная ширина.
const charts = [];
function chartBox(draw) {
  const box = h('div');
  charts.push(() => {
    const w = Math.max(260, box.clientWidth);
    box.replaceChildren(draw(w));
  });
  return box;
}
let resizeTimer;
window.addEventListener('resize', () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => charts.forEach((d) => d()), 150);
});

function table(head, rows) {
  return h('details', {}, h('summary', {}, 'Таблица'),
    h('table', {}, h('tr', {}, head.map((c) => h('th', {}, c))), rows.map((r) => h('tr', {}, r.map((c) => h('td', {}, c))))));
}

function chartCard(title, hint, body, cls = '') {
  return h('section', { class: `chart card ${cls}` }, h('h2', {}, title), hint ? h('div', { class: 'hint' }, hint) : null, body);
}

// ── Аналитика ────────────────────────────────────────────────────────────────

let statsDays = 7;

// 90 дней по дням нечитаемы на телефоне — сворачиваем в недели.
function bucket(days) {
  if (statsDays < 90) return days.map((d) => ({ ...d, label: shortDay(d.date), key: d.date }));
  const weeks = new Map();
  for (const d of days) {
    const k = mondayOf(d.date);
    const w = weeks.get(k) || { key: k, label: 'нед. ' + shortDay(k).slice(3), done: 0, created: 0, minutes: 0, plan_done: 0, plan_missed: 0, plan_moved: 0 };
    for (const f of ['done', 'created', 'minutes', 'plan_done', 'plan_missed', 'plan_moved']) w[f] += d[f];
    weeks.set(k, w);
  }
  return [...weeks.values()];
}

function lineChart(points, series) {
  // series: [{key, name, color}] — одна ось: оба ряда в штуках.
  return (W) => {
    const H = 180, L = 30, R = 8, T = 10, B = 24;
    const max = niceMax(Math.max(1, ...points.flatMap((p) => series.map((sr) => p[sr.key]))));
    const x = (i) => L + (points.length === 1 ? (W - L - R) / 2 : (i * (W - L - R)) / (points.length - 1));
    const y = (v) => T + (H - T - B) * (1 - v / max);
    const svg = s('svg', { viewBox: `0 0 ${W} ${H}`, height: H, role: 'img', 'aria-label': series.map((sr) => sr.name).join(' и ') });
    for (const t of [0, max / 2, max]) {
      svg.append(s('line', { x1: L, x2: W - R, y1: y(t), y2: y(t), stroke: 'var(--viz-grid)', 'stroke-width': 1 }));
      svg.append(s('text', { x: L - 6, y: y(t) + 4, 'text-anchor': 'end' }, Number.isInteger(t) ? t : t.toFixed(1)));
    }
    const step = Math.ceil(points.length / Math.max(2, Math.floor((W - L) / 70)));
    points.forEach((p, i) => {
      if (i % step === 0 || i === points.length - 1) svg.append(s('text', { x: x(i), y: H - 6, 'text-anchor': 'middle' }, p.label));
    });
    for (const sr of series) {
      const d = points.map((p, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)},${y(p[sr.key]).toFixed(1)}`).join('');
      svg.append(s('path', { d, fill: 'none', stroke: sr.color, 'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-linecap': 'round' }));
      const last = points.length - 1;
      svg.append(s('circle', { cx: x(last), cy: y(points[last][sr.key]), r: 4, fill: sr.color, stroke: 'var(--surface)', 'stroke-width': 2 }));
    }
    // Перекрестие: ближайшая точка по X, в подсказке — оба ряда.
    const cross = s('line', { y1: T, y2: H - B, stroke: 'var(--muted)', 'stroke-width': 1, visibility: 'hidden' });
    svg.append(cross);
    const hit = s('rect', { x: L, y: T, width: W - L - R, height: H - T - B, fill: 'transparent' });
    const pick = (e) => {
      const rect = svg.getBoundingClientRect();
      const px = ((e.clientX - rect.left) / rect.width) * W;
      const i = Math.max(0, Math.min(points.length - 1, Math.round(((px - L) / (W - L - R)) * (points.length - 1))));
      cross.setAttribute('x1', x(i)); cross.setAttribute('x2', x(i)); cross.setAttribute('visibility', 'visible');
      showTip(e, [h('div', {}, points[i].label), ...series.map((sr) =>
        h('div', {}, h('span', { class: 'k', style: { '--k': sr.color } }), h('b', {}, points[i][sr.key]), ' ', sr.name))]);
    };
    hit.addEventListener('pointermove', pick);
    hit.addEventListener('pointerleave', () => { cross.setAttribute('visibility', 'hidden'); hideTip(); });
    svg.append(hit);
    return svg;
  };
}

function planColumns(points) {
  return (W) => {
    const H = 170, L = 34, R = 8, T = 10, B = 24;
    const band = (W - L - R) / points.length;
    const bw = Math.min(24, Math.max(3, band - 2));
    const y = (v) => T + (H - T - B) * (1 - v);
    const svg = s('svg', { viewBox: `0 0 ${W} ${H}`, height: H, role: 'img', 'aria-label': 'Выполнение плана по дням' });
    for (const t of [0, 0.5, 1]) {
      svg.append(s('line', { x1: L, x2: W - R, y1: y(t), y2: y(t), stroke: 'var(--viz-grid)', 'stroke-width': 1 }));
      svg.append(s('text', { x: L - 6, y: y(t) + 4, 'text-anchor': 'end' }, `${t * 100}%`));
    }
    const step = Math.ceil(points.length / Math.max(2, Math.floor((W - L) / 70)));
    points.forEach((p, i) => {
      const cx = L + band * i + band / 2;
      if (i % step === 0 || i === points.length - 1) svg.append(s('text', { x: cx, y: H - 6, 'text-anchor': 'middle' }, p.label));
      const all = p.plan_done + p.plan_missed + p.plan_moved;
      if (!all) return;
      const rate = p.plan_done / all;
      const top = y(rate), base = y(0), hgt = Math.max(1, base - top), r = Math.min(4, bw / 2, hgt);
      // Скругление только у верхнего края, основание прямое.
      const x0 = cx - bw / 2, x1 = cx + bw / 2;
      svg.append(s('path', { d: `M${x0},${base}V${top + r}Q${x0},${top} ${x0 + r},${top}H${x1 - r}Q${x1},${top} ${x1},${top + r}V${base}Z`, fill: 'var(--viz-1)' }));
      // Зона наведения — вся высота полосы.
      const hit = s('rect', { x: L + band * i, y: T, width: band, height: H - T - B, fill: 'transparent', class: 'mark' });
      tipOn(hit, () => [h('div', {}, p.label), h('div', {}, h('b', {}, `${Math.round(rate * 100)}%`), ` · сделано ${p.plan_done} из ${all}`),
        p.plan_moved ? h('div', {}, `перенесено ${p.plan_moved}`) : null, p.plan_missed ? h('div', {}, `не сделано ${p.plan_missed}`) : null].filter(Boolean));
      svg.append(hit);
    });
    return svg;
  };
}

function dumbbell(rows) {
  // Оценка (серая точка) → факт (основной цвет), общая шкала минут.
  const max = niceMax(Math.max(...rows.flatMap((r) => [r.estimate_min, r.spent_min])));
  return (W) => {
    const wrap = h('div', { class: 'dumb' });
    const tw = Math.max(120, W * 0.6 - 10);
    for (const r of rows) {
      const x = (v) => 6 + (v / max) * (tw - 12);
      const svg = s('svg', { viewBox: `0 0 ${tw} 18`, height: 18 });
      svg.append(s('line', { x1: x(Math.min(r.estimate_min, r.spent_min)), x2: x(Math.max(r.estimate_min, r.spent_min)), y1: 9, y2: 9, stroke: 'var(--viz-ctx)', 'stroke-width': 2 }));
      svg.append(s('circle', { cx: x(r.estimate_min), cy: 9, r: 4.5, fill: 'var(--viz-ctx)', stroke: 'var(--surface)', 'stroke-width': 2 }));
      svg.append(s('circle', { cx: x(r.spent_min), cy: 9, r: 4.5, fill: 'var(--viz-1)', stroke: 'var(--surface)', 'stroke-width': 2 }));
      tipOn(svg, () => [h('div', {}, r.title), h('div', {}, 'оценка ', h('b', {}, mins(r.estimate_min)), ' · факт ', h('b', {}, mins(r.spent_min)))]);
      wrap.append(h('div', { class: 'name', title: r.title }, r.title), svg);
    }
    return wrap;
  };
}

function median(xs) {
  if (!xs.length) return null;
  const a = [...xs].sort((p, q) => p - q);
  return a.length % 2 ? a[(a.length - 1) / 2] : (a[a.length / 2 - 1] + a[a.length / 2]) / 2;
}

function deltaText(cur, prev, fmt = (v) => v) {
  if (!prev && !cur) return '';
  if (!prev) return 'в прошлом периоде — 0';
  const d = cur - prev;
  if (!d) return 'как в прошлом периоде';
  return `${d > 0 ? '↑' : '↓'} ${fmt(Math.abs(d))} к прошлому периоду`;
}

async function viewStats() {
  const to = addDays(todayStr(), 1);
  const from = addDays(to, -statsDays);
  const st = await api('GET', `stats?from=${from}&to=${to}`);

  const days = st.days || [];
  const planDone = days.reduce((a, d) => a + d.plan_done, 0);
  const planAll = days.reduce((a, d) => a + d.plan_done + d.plan_missed + d.plan_moved, 0);
  const ratios = (st.estimates || []).filter((e) => e.estimate_min > 0).map((e) => e.spent_min / e.estimate_min);
  const acc = median(ratios);

  const kpi = (label, value, delta) => h('div', { class: 'kpi card' },
    h('div', { class: 'label' }, label), h('div', { class: 'value' }, value), delta ? h('div', { class: 'delta' }, delta) : null);

  const periods = h('div', { class: 'chips' }, [[7, '7 дней'], [30, '30 дней'], [90, '90 дней']].map(([n, l]) =>
    h('button', { class: `chip ${statsDays === n ? 'on' : ''}`, onclick: () => { statsDays = n; render(); } }, l)));

  const pts = bucket(days);
  const bySphere = st.time_by_sphere || [];
  const maxMin = Math.max(1, ...bySphere.map((r) => r.minutes));
  const sphereIcon = new Map(state.spheres.map((sp) => [sp.name, sp.icon]));

  const est = (st.estimates || []).filter((e) => e.estimate_min > 0)
    .sort((a, b) => Math.abs(Math.log(b.spent_min / b.estimate_min)) - Math.abs(Math.log(a.spent_min / a.estimate_min)))
    .slice(0, 10);

  const itemsCard = (title, hint, items, empty) => h('section', { class: 'chart card' }, h('h2', {}, title), h('div', { class: 'hint' }, hint),
    items.length ? list(items, { withDate: true, onChange: render }) : h('div', { class: 'empty' }, empty));

  const nodes = [
    h('div', { class: 'bar' }, h('h1', {}, 'Аналитика')),
    periods,
    h('div', { class: 'kpis' },
      kpi('Сделано', String(st.done), deltaText(st.done, st.prev.done)),
      kpi('Создано', String(st.created), deltaText(st.created, st.prev.created)),
      kpi('Учтено времени', mins(st.total_minutes), deltaText(st.total_minutes, st.prev.minutes, mins)),
      kpi('План выполнен', planAll ? `${Math.round((planDone / planAll) * 100)}%` : '—', planAll ? `${planDone} из ${planAll}` : 'нет задач с датой'),
      kpi('Факт / оценка', acc ? `${acc.toFixed(1)}×` : '—', acc ? `медиана по ${ratios.length} задачам` : 'нужны оценка и учёт времени'),
    ),
    h('div', { class: 'charts' },
      chartCard('Сделано и создано', statsDays < 90 ? 'по дням' : 'по неделям',
        [h('div', { class: 'legend' },
          h('span', {}, h('i', { style: { '--k': 'var(--viz-1)' } }), 'сделано'),
          h('span', {}, h('i', { style: { '--k': 'var(--viz-ctx)' } }), 'создано')),
        chartBox(lineChart(pts, [{ key: 'created', name: 'создано', color: 'var(--viz-ctx)' }, { key: 'done', name: 'сделано', color: 'var(--viz-1)' }])),
        table(['Период', 'Сделано', 'Создано'], pts.map((p) => [p.label, p.done, p.created]))]),
      chartCard('Выполнение плана', 'доля сделанного из запланированного; перенос = не выполнено',
        [chartBox(planColumns(pts)),
          table(['Период', 'Сделано', 'Не сделано', 'Перенесено'], pts.map((p) => [p.label, p.plan_done, p.plan_missed, p.plan_moved]))]),
      chartCard('Время по сферам', 'по записям времени за период',
        bySphere.length ? [h('div', { class: 'hbars' }, bySphere.flatMap((r) => [
          h('div', {}, `${sphereIcon.get(r.sphere) || '·'} ${r.sphere}`),
          h('div', { class: 'track' }, h('div', { class: 'bar', style: { width: `${(r.minutes / maxMin) * 85}%` } }), h('span', { class: 'val' }, mins(r.minutes)))]))]
          : h('div', { class: 'empty' }, 'Время ещё не записывалось: «+ время» в карточке или «уборка 40м» боту.')),
      chartCard('Оценка и факт', 'самые большие промахи; серая — оценка, синяя — факт',
        est.length ? [h('div', { class: 'legend' },
          h('span', {}, h('i', { class: 'dot', style: { '--k': 'var(--viz-ctx)' } }), 'оценка'),
          h('span', {}, h('i', { class: 'dot', style: { '--k': 'var(--viz-1)' } }), 'факт')),
        chartBox(dumbbell(est)),
        table(['Задача', 'Оценка', 'Факт'], est.map((e) => [e.title, mins(e.estimate_min), mins(e.spent_min)]))]
          : h('div', { class: 'empty' }, 'Появится, когда у закрытых задач будут и оценка, и записанное время.')),
    ),
    h('div', { class: 'lists' },
      itemsCard('Переносятся снова и снова', 'разовые, перенесённые 2+ раза: разбить, делегировать или выкинуть?', st.postponed || [], 'Таких нет 👍'),
      itemsCard('Долго висят', 'открыты больше двух недель', st.stale || [], 'Таких нет 👍')),
  ];
  return nodes;
}

// ── Граф связей ──────────────────────────────────────────────────────────────

const REL_NAMES = { parent: 'подзадача', blocks: 'блокирует', follows: 'затем', related: 'связано', part_of: 'часть цели', prepares: 'готовит к' };
let graphSphere = null;

// Силовая раскладка одного связного кусочка в квадрате size×size.
function forceLayout(nodes, links, size) {
  const n = nodes.length;
  if (n === 1) { nodes[0].x = size / 2; nodes[0].y = size / 2; return; }
  const k = size / Math.sqrt(n) * 0.75;
  nodes.forEach((p, i) => {
    const a = (i / n) * Math.PI * 2;
    p.x = size / 2 + Math.cos(a) * size * 0.3; p.y = size / 2 + Math.sin(a) * size * 0.3;
  });
  let temp = size / 6;
  for (let it = 0; it < 300; it++) {
    const dx = new Float64Array(n), dy = new Float64Array(n);
    for (let i = 0; i < n; i++) for (let j = i + 1; j < n; j++) {
      const x = nodes[i].x - nodes[j].x, y = nodes[i].y - nodes[j].y;
      const d2 = Math.max(0.01, x * x + y * y), f = (k * k) / d2;
      dx[i] += x * f; dy[i] += y * f; dx[j] -= x * f; dy[j] -= y * f;
    }
    for (const [a, b] of links) {
      const x = nodes[a].x - nodes[b].x, y = nodes[a].y - nodes[b].y;
      const d = Math.max(0.01, Math.hypot(x, y)), f = d / k;
      dx[a] -= x * f; dy[a] -= y * f; dx[b] += x * f; dy[b] += y * f;
    }
    for (let i = 0; i < n; i++) {
      dx[i] += (size / 2 - nodes[i].x) * 0.05; dy[i] += (size / 2 - nodes[i].y) * 0.05;
      const d = Math.max(0.01, Math.hypot(dx[i], dy[i])), m = Math.min(d, temp);
      nodes[i].x += (dx[i] / d) * m; nodes[i].y += (dy[i] / d) * m;
    }
    temp *= 0.985;
  }
}

// Раскладка графа: связные кусочки раскладываются по отдельности (иначе они
// отталкиваются в углы) и ставятся строками, крупные — первыми; затем всё
// вписывается в окно с запасом справа под подписи.
function layout(nodes, edges, W, H) {
  const idx = new Map(nodes.map((p, i) => [p.id, i]));
  const parent = nodes.map((_, i) => i);
  const find = (i) => (parent[i] === i ? i : (parent[i] = find(parent[i])));
  const links = edges.map((e) => [idx.get(e.from), idx.get(e.to)]).filter(([a, b]) => a !== undefined && b !== undefined);
  for (const [a, b] of links) parent[find(a)] = find(b);
  const groups = new Map();
  nodes.forEach((_, i) => { const r = find(i); if (!groups.has(r)) groups.set(r, []); groups.get(r).push(i); });
  const comps = [...groups.values()].sort((a, b) => b.length - a.length).map((members) => {
    const local = new Map(members.map((gi, li) => [gi, li]));
    const sub = members.map((gi) => nodes[gi]);
    const size = Math.max(140, 120 * Math.sqrt(members.length));
    forceLayout(sub, links.filter(([a, b]) => local.has(a) && local.has(b)).map(([a, b]) => [local.get(a), local.get(b)]), size);
    // Подписи справа от узлов: вытянутый по горизонтали кусочек разворачиваем
    // вертикально, иначе подпись наезжает на соседний узел.
    let xs = sub.map((p) => p.x), ys = sub.map((p) => p.y);
    if (Math.max(...xs) - Math.min(...xs) > Math.max(...ys) - Math.min(...ys)) {
      sub.forEach((p) => { [p.x, p.y] = [p.y, p.x]; });
      xs = sub.map((p) => p.x); ys = sub.map((p) => p.y);
    }
    const minX = Math.min(...xs), minY = Math.min(...ys);
    sub.forEach((p) => { p.x -= minX; p.y -= minY; });
    return { sub, w: Math.max(...xs) - minX + 190, h: Math.max(...ys) - minY + 40 }; // +ширина подписи
  });
  // Строки слева направо в пределах ширины окна.
  let x = 0, y = 0, rowH = 0, totalW = 0;
  const maxRow = Math.max(W, 400);
  for (const c of comps) {
    if (x > 0 && x + c.w > maxRow) { x = 0; y += rowH; rowH = 0; }
    c.sub.forEach((p) => { p.x += x; p.y += y; });
    x += c.w; rowH = Math.max(rowH, c.h); totalW = Math.max(totalW, x);
  }
  const totalH = y + rowH;
  const pad = 24;
  const sc = Math.min((W - 2 * pad) / Math.max(1, totalW), (H - 2 * pad) / Math.max(1, totalH), 1.6);
  const offX = pad + (W - 2 * pad - totalW * sc) / 2, offY = pad + (H - 2 * pad - totalH * sc) / 2;
  for (const p of nodes) { p.x = offX + (p.x + 10) * sc; p.y = offY + (p.y + 20) * sc; }
}

async function viewGraph() {
  const sp = graphSphere !== null ? state.sphereById.get(graphSphere) : null;
  const g = await api('GET', 'graph' + (sp ? `?sphere=${sp.slug}` : ''));
  const chips = h('div', { class: 'chips' },
    h('button', { class: `chip ${graphSphere === null ? 'on' : ''}`, onclick: () => { graphSphere = null; render(); } }, 'Все'),
    state.spheres.map((x) => h('button', { class: `chip ${graphSphere === x.id ? 'on' : ''}`, onclick: () => { graphSphere = x.id; render(); } }, `${x.icon} ${x.name}`)));
  const head = [h('div', { class: 'bar' }, h('h1', {}, 'Граф связей'), h('div', { class: 'sub' }, 'Цели, подзадачи и связи. Нажми на узел — откроется задача.')), chips];
  if (!g.nodes.length) {
    return [...head, h('div', { class: 'empty' }, 'Связей пока нет. Добавь подзадачу или связь в карточке задачи.')];
  }
  const box = h('div', { class: 'graph card' });
  charts.push(() => {
    const W = Math.max(320, box.clientWidth), H = Math.round(window.innerHeight * 0.7);
    const nodes = g.nodes.map((it) => ({ ...it }));
    layout(nodes, g.edges, W, H);
    const byId = new Map(nodes.map((p) => [p.id, p]));
    const svg = s('svg', { viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': 'Граф связей задач' });
    svg.append(s('defs', {}, s('marker', { id: 'arr', viewBox: '0 0 10 10', refX: 16, refY: 5, markerWidth: 7, markerHeight: 7, orient: 'auto-start-reverse' },
      s('path', { d: 'M0,0L10,5L0,10z', fill: 'var(--muted)' }))));
    const world = s('g');
    svg.append(world);
    const nbr = new Map(nodes.map((p) => [p.id, new Set([p.id])]));
    const edgeEls = g.edges.map((e) => {
      const a = byId.get(e.from), b = byId.get(e.to);
      nbr.get(e.from).add(e.to); nbr.get(e.to).add(e.from);
      const directed = e.type !== 'related';
      const el = s('line', { x1: a.x, y1: a.y, x2: b.x, y2: b.y, class: `edge ${e.type}`, 'marker-end': directed ? 'url(#arr)' : null });
      el.dataset.a = e.from; el.dataset.b = e.to;
      world.append(el);
      return el;
    });
    const showLabels = nodes.length <= 60;
    const nodeEls = nodes.map((p) => {
      const sph = sphereOf(p);
      const r = p.kind === 'goal' ? 10 : 6.5;
      const label = p.title.length > 26 ? p.title.slice(0, 25) + '…' : p.title;
      const gEl = s('g', { class: `node ${p.status}`, transform: `translate(${p.x},${p.y})` },
        s('circle', { r, fill: sph ? sph.color : 'var(--muted)' }),
        s('text', { x: r + 4, y: 4, visibility: showLabels ? 'visible' : 'hidden' }, label));
      gEl.dataset.id = p.id;
      gEl.addEventListener('click', () => openEditor(p.id, render));
      tipOn(gEl, () => [h('div', {}, h('b', {}, p.title)), h('div', {}, `${KINDS[p.kind]} · ${STATUSES[p.status]}`), sph ? h('div', {}, `${sph.icon} ${sph.name}`) : null].filter(Boolean));
      gEl.addEventListener('pointerenter', () => {
        const near = nbr.get(p.id);
        box.classList.add('focus');
        nodeEls.forEach((n) => { n.classList.toggle('near', near.has(n.dataset.id)); n.querySelector('text').setAttribute('visibility', near.has(n.dataset.id) || showLabels ? 'visible' : 'hidden'); });
        edgeEls.forEach((el) => el.classList.toggle('near', el.dataset.a === p.id || el.dataset.b === p.id));
      });
      gEl.addEventListener('pointerleave', () => {
        box.classList.remove('focus');
        nodeEls.forEach((n) => n.querySelector('text').setAttribute('visibility', showLabels ? 'visible' : 'hidden'));
      });
      world.append(gEl);
      return gEl;
    });
    // Перетаскивание и колесо — сдвиг и масштаб всей сцены.
    let view = { x: 0, y: 0, k: 1 }, drag = null;
    const apply = () => world.setAttribute('transform', `translate(${view.x},${view.y}) scale(${view.k})`);
    svg.addEventListener('pointerdown', (e) => { if (e.target === svg) { drag = { x: e.clientX, y: e.clientY, vx: view.x, vy: view.y }; svg.setPointerCapture(e.pointerId); } });
    svg.addEventListener('pointermove', (e) => { if (drag) { const r = W / svg.getBoundingClientRect().width; view.x = drag.vx + (e.clientX - drag.x) * r; view.y = drag.vy + (e.clientY - drag.y) * r; apply(); } });
    svg.addEventListener('pointerup', () => (drag = null));
    svg.addEventListener('wheel', (e) => {
      e.preventDefault();
      const f = e.deltaY < 0 ? 1.15 : 1 / 1.15, rect = svg.getBoundingClientRect();
      const px = ((e.clientX - rect.left) / rect.width) * W, py = ((e.clientY - rect.top) / rect.height) * H;
      view.x = px - (px - view.x) * f; view.y = py - (py - view.y) * f; view.k *= f; apply();
    }, { passive: false });
    return svg;
  });
  // chartBox-подобный монтаж: рисуем после вставки, по реальной ширине.
  const mount = charts.pop();
  charts.push(() => box.replaceChildren(mount()));
  const legend = h('div', { class: 'graph-legend' },
    h('span', {}, '● цель — крупный узел'), h('span', {}, '→ стрелка: от причины к следствию'),
    h('span', {}, h('span', { style: { color: 'var(--viz-1)' } }, '━'), ' блокирует'), h('span', {}, 'цвет узла — сфера'));
  return [...head, legend, box];
}

// ── Связи в карточке задачи ──────────────────────────────────────────────────

const REL_OUT = { blocks: 'блокирует', follows: 'затем', related: 'связано с', part_of: 'часть цели', prepares: 'готовит к' };
const REL_IN = { blocks: 'ждёт', follows: 'после', related: 'связано с', part_of: 'включает', prepares: 'подготовка' };

function relationsBlock(detail, reload) {
  const box = h('div', { class: 'rels' });
  const byId = new Map((detail.related || []).map((it) => [it.id, it]));
  const rows = (detail.relations || []).map((r) => {
    const out = r.from_id === detail.id;
    const other = byId.get(out ? r.to_id : r.from_id);
    if (!other) return null;
    return h('div', { class: 'rel' },
      h('span', { class: 't' }, (out ? REL_OUT : REL_IN)[r.type] || r.type),
      h('span', { class: 'name', onclick: () => openEditor(other.id, reload) }, other.title),
      h('button', { class: 'btn icon', type: 'button', 'aria-label': 'Убрать связь', onclick: async () => {
        try { await api('DELETE', 'relations', { from_id: r.from_id, to_id: r.to_id, type: r.type }); reload(); } catch (err) { handleError(err); }
      } }, '×'));
  }).filter(Boolean);

  const type = h('select', { 'aria-label': 'Тип связи' }, Object.entries(REL_OUT).map(([k, v]) => h('option', { value: k }, v)));
  const listId = 'rel-options-' + detail.id;
  const datalist = h('datalist', { id: listId });
  const q = h('input', { placeholder: 'Найти задачу…', list: listId, 'aria-label': 'Связать с задачей' });
  let found = [];
  let qTimer;
  q.addEventListener('input', () => {
    clearTimeout(qTimer);
    qTimer = setTimeout(async () => {
      if (q.value.trim().length < 2) return;
      try {
        found = (await api('GET', `items?q=${encodeURIComponent(q.value.trim())}&limit=15`)).filter((it) => it.id !== detail.id);
        datalist.replaceChildren(...found.map((it) => h('option', { value: it.title })));
      } catch (err) { handleError(err); }
    }, 250);
  });
  const add = async () => {
    const target = found.find((it) => it.title === q.value) || found[0];
    if (!target) return toast('Не нашёл такую задачу');
    try {
      await api('POST', 'relations', { from_id: detail.id, to_id: target.id, type: type.value });
      reload();
    } catch (err) { handleError(err); }
  };
  box.append(h('div', { class: 'section' }, 'Связи'), ...rows,
    h('div', { class: 'add' }, type, q, datalist, h('button', { class: 'btn', type: 'button', onclick: add }, 'Связать')));
  return box;
}

// ── Дневник ──────────────────────────────────────────────────────────────────

const MOODS = { 1: '😞', 2: '😕', 3: '😐', 4: '🙂', 5: '😄' };

async function viewJournal(date) {
  const [j, entries] = await Promise.all([api('GET', `journal/${date}`), api('GET', 'journal')]);
  const today = todayStr();
  let mood = j.mood || null;

  const moodBtns = h('div', { class: 'moods', role: 'group', 'aria-label': 'Настроение' });
  const drawMoods = () => moodBtns.replaceChildren(...Object.entries(MOODS).map(([k, e]) =>
    h('button', { type: 'button', class: Number(k) === mood ? 'on' : '', 'aria-pressed': String(Number(k) === mood), 'aria-label': `Настроение ${k}`,
      onclick: async () => { mood = Number(k); drawMoods(); try { await api('PATCH', `journal/${date}`, { mood }); } catch (err) { handleError(err); } } }, e)));
  drawMoods();

  const text = h('textarea', { placeholder: 'Как прошёл день: что получилось, что нет, что понял…', 'aria-label': 'Запись в дневник' });
  text.value = j.text || '';
  const status = h('span', { class: 'hint' });
  const save = async () => {
    try {
      const r = await api('PATCH', `journal/${date}`, { text: text.value });
      status.textContent = r && r.queued ? 'сохранится, когда появится сеть' : 'сохранено';
    } catch (err) { handleError(err); }
  };
  // Автосохранение: пишешь — через пару секунд тишины сохраняется само.
  let t;
  text.addEventListener('input', () => { status.textContent = '…'; clearTimeout(t); t = setTimeout(save, 1500); });

  return [
    h('div', { class: 'bar' },
      h('button', { class: 'btn icon', 'aria-label': 'Предыдущий день', onclick: () => go(`#/journal/${addDays(date, -1)}`) }, '‹'),
      h('h1', {}, date === today ? 'Сегодня' : dayTitle(date)),
      date !== today ? h('button', { class: 'btn', onclick: () => go('#/journal') }, 'Сегодня') : null,
      h('button', { class: 'btn icon', 'aria-label': 'Следующий день', onclick: () => go(`#/journal/${addDays(date, 1)}`) }, '›'),
      h('div', { class: 'sub' }, `учтено ${mins(j.minutes)} · сделано ${j.done.length}`)),
    moodBtns,
    h('div', { class: 'journal' }, text),
    h('div', { class: 'actions' }, status, h('span', { class: 'spacer' }), h('button', { class: 'btn primary', onclick: save }, 'Сохранить')),
    j.summary ? h('div', { class: 'summary card' }, h('div', { class: 'hint' }, 'Итог дня (с вечернего разбора)'), md(j.summary)) : null,
    j.done.length ? [h('div', { class: 'section' }, 'Сделано за день'), list(j.done, { onChange: render })] : null,
    entries.length ? [h('div', { class: 'section' }, 'Последние записи'),
      h('div', { class: 'items entries' }, entries.map((e) => h('div', { class: 'entry card', onclick: () => go(`#/journal/${e.date}`) },
        h('span', { class: 'd' }, shortDay(e.date)), h('span', {}, e.mood ? MOODS[e.mood] : '·'),
        h('span', { class: 't' }, (e.summary || e.text || '').split('\n')[0]))))] : null,
  ];
}

// Очередь неотправленного — видна во «Входящих».
async function queueSection() {
  const q = await outbox.all().catch(() => []);
  if (!q.length) return null;
  return [h('div', { class: 'section' }, `Ждут отправки · ${q.length}`),
    h('div', { class: 'items queue-list' }, q.map((e) => h('div', { class: 'q card' },
      h('div', { class: 'queue-detail' }, h('div', {}, e.label),
        e.error ? h('div', { class: 'queue-error' }, e.error) : null,
        h('div', { class: 'hint' }, e.state === 'failed' ? 'Не отправлено — повтори или удали из очереди'
          : e.state === 'auth' ? 'Нужен вход — после входа можно повторить'
          : e.retryAt ? `Повторю после ${new Date(e.retryAt).toLocaleTimeString('ru-RU')}` : 'Сохранено на этом устройстве')),
      h('div', { class: 'queue-actions' },
        h('button', { class: 'btn', onclick: () => retryQueued(e.key).catch(handleError) }, 'Повторить'),
        h('button', { class: 'btn danger', title: 'Не отправлять', onclick: () => discardQueued(e.key).catch(handleError) }, 'Удалить')))))];
}

async function logout() {
  if (!confirm('Выйти на этом устройстве? Неотправленные изменения пропадут.')) return;
  try { await api('POST', 'logout'); } catch { /* выходим всё равно */ }
  for (const k of await caches.keys()) await caches.delete(k);
  for (const e of await outbox.all().catch(() => [])) await outbox.remove(e.key);
  location.reload();
}

// ── Заметки ──────────────────────────────────────────────────────────────────

let notesQuery = '';

function noteSnippet(it) {
  const md = (it.body || []).find((b) => b.type === 'md' && b.text);
  if (md) return md.text.replace(/[*`#>\[\]]/g, '').slice(0, 220);
  const ch = (it.body || []).find((b) => b.type === 'checklist');
  return ch ? ch.items.map((x) => (x.done ? '☑ ' : '☐ ') + x.text).join(' · ').slice(0, 220) : '';
}

async function viewNotes() {
  const q = notesQuery.trim();
  const notes = await api('GET', `items?kind=note&limit=200${q ? '&q=' + encodeURIComponent(q) : ''}`);
  const search = h('input', { type: 'search', placeholder: 'Поиск по заметкам', value: notesQuery, 'aria-label': 'Поиск по заметкам' });
  let t;
  search.addEventListener('input', () => {
    clearTimeout(t);
    t = setTimeout(() => { notesQuery = search.value; render().then(() => {
      const el = document.querySelector('.quick input[type=search]');
      if (el) { el.focus(); el.setSelectionRange(el.value.length, el.value.length); }
    }); }, 300);
  });
  return [
    h('div', { class: 'bar' }, h('h1', {}, 'Заметки'),
      h('button', { class: 'btn primary', onclick: () => openEditor(null, render, { kind: 'note' }) }, 'Новая заметка'),
      h('div', { class: 'sub' }, 'Мысли, конспекты, ссылки. Боту можно написать «идея: …» или «заметка: …».')),
    h('div', { class: 'quick' }, search),
    notes.length ? h('div', { class: 'items notes', style: { 'margin-top': '12px' } }, notes.map((n) => {
      const sp = sphereOf(n);
      return h('div', { class: 'note card', style: sp ? { '--sphere': sp.color } : {}, onclick: () => openEditor(n.id, render) },
        h('h3', {}, n.title),
        noteSnippet(n) ? h('p', {}, noteSnippet(n)) : null,
        h('div', { class: 'meta' }, sp ? h('span', {}, `${sp.icon} ${sp.name}`) : null,
          h('span', {}, shortDay(mskDate(n.updated_at))), (n.tags || []).map((tg) => h('span', { class: 'tag' }, tg))));
    })) : h('div', { class: 'empty' }, q ? 'Ничего не нашлось.' : 'Заметок пока нет.'),
  ];
}

// ── Сферы (настройки) ────────────────────────────────────────────────────────

// Цвета событий Google Календаря: номер → название и как они выглядят.
const GCAL_COLORS = {
  1: ['Лаванда', '#7986cb'], 2: ['Шалфей', '#33b679'], 3: ['Виноград', '#8e24aa'], 4: ['Фламинго', '#e67c73'],
  5: ['Банан', '#f6bf26'], 6: ['Мандарин', '#f4511e'], 7: ['Павлин', '#039be5'], 8: ['Графит', '#616161'],
  9: ['Черника', '#3f51b5'], 10: ['Базилик', '#0b8043'], 11: ['Томат', '#d50000'],
};

async function viewSpheres() {
  const list = await api('GET', 'spheres?all=1');
  const active = list.filter((s) => !s.archived);
  const archived = list.filter((s) => s.archived);

  // Каждое поле сохраняется сразу; справочник перечитается при следующем экране.
  const save = async (sp, patch, rerender) => {
    try {
      Object.assign(sp, await api('PATCH', `spheres/${sp.id}`, patch));
      state.spheres = [];
      toast('Сохранено');
      if (rerender) render();
    } catch (err) { handleError(err); }
  };
  const move = async (i, dir) => {
    const a = active[i], b = active[i + dir];
    if (!b) return;
    // Если порядок совпадал — разводим по индексам, иначе меняем местами.
    const sa = a.sort === b.sort ? (i + dir) * 10 : b.sort, sb = a.sort === b.sort ? i * 10 : a.sort;
    try {
      await api('PATCH', `spheres/${a.id}`, { sort: sa });
      await api('PATCH', `spheres/${b.id}`, { sort: sb });
      state.spheres = [];
      render();
    } catch (err) { handleError(err); }
  };

  const card = (sp, i) => {
    const st = sp.style || {};
    const swatch = h('span', { class: 'swatch' });
    const paint = () => {
      swatch.style.setProperty('--light', sp.color);
      swatch.style.setProperty('--dark', (sp.style && sp.style.color_dark) || sp.color);
    };
    paint();
    const icon = h('input', { class: 'icon-input', value: sp.icon || '', maxlength: 8, 'aria-label': 'Иконка' });
    icon.addEventListener('change', () => save(sp, { icon: icon.value.trim() }));
    const name = h('input', { value: sp.name, maxlength: 40, 'aria-label': 'Название' });
    name.addEventListener('change', () => name.value.trim() && save(sp, { name: name.value.trim() }));
    const light = h('input', { type: 'color', value: sp.color.toLowerCase(), 'aria-label': 'Цвет в светлой теме' });
    light.addEventListener('change', async () => { await save(sp, { color: light.value }); paint(); });
    const dark = h('input', { type: 'color', value: (st.color_dark || sp.color).toLowerCase(), 'aria-label': 'Цвет в тёмной теме' });
    dark.addEventListener('change', async () => { await save(sp, { color_dark: dark.value }); paint(); });
    const gcal = h('select', { 'aria-label': 'Цвет в Google Календаре' },
      h('option', { value: '' }, 'подобрать автоматически'),
      Object.entries(GCAL_COLORS).map(([k, [n]]) => h('option', { value: k, selected: st.gcal_color === k }, n)));
    const gdot = h('span', { class: 'gdot' });
    const drawG = () => gdot.style.setProperty('background', gcal.value ? GCAL_COLORS[gcal.value][1] : 'transparent');
    drawG();
    gcal.addEventListener('change', () => { drawG(); save(sp, { gcal_color: gcal.value }); });
    const hint = h('textarea', { rows: 2, maxlength: 300, placeholder: 'Что сюда относится — подсказка для разбора заметок ИИ' });
    hint.value = st.hint || '';
    hint.addEventListener('change', () => save(sp, { hint: hint.value }));

    return h('div', { class: `sphere-card card${sp.archived ? ' archived' : ''}` },
      h('div', { class: 'head' }, swatch, icon, name,
        !sp.archived ? h('button', { class: 'btn icon', type: 'button', title: 'Выше', 'aria-label': 'Выше', disabled: i === 0, onclick: () => move(i, -1) }, '↑') : null,
        !sp.archived ? h('button', { class: 'btn icon', type: 'button', title: 'Ниже', 'aria-label': 'Ниже', disabled: i === active.length - 1, onclick: () => move(i, 1) }, '↓') : null),
      h('div', { class: 'grid' },
        h('label', { class: 'field' }, 'Светлая тема', light),
        h('label', { class: 'field' }, 'Тёмная тема', dark),
        h('label', { class: 'field wide' }, 'Google Календарь', h('div', { class: 'row' }, gdot, gcal)),
        h('label', { class: 'field wide' }, 'Подсказка для ИИ', hint)),
      h('div', { class: 'actions' },
        h('span', { class: 'slug' }, sp.slug),
        h('span', { class: 'spacer' }),
        h('button', { class: 'btn', type: 'button', onclick: () => {
          if (!sp.archived && !confirm(`Убрать «${sp.name}» в архив? Задачи останутся, но без цвета сферы.`)) return;
          save(sp, { archived: !sp.archived }, true);
        } }, sp.archived ? 'Вернуть из архива' : 'В архив')));
  };

  const newName = h('input', { placeholder: 'Название', maxlength: 40, 'aria-label': 'Название новой сферы' });
  const newIcon = h('input', { class: 'icon-input', placeholder: '✨', maxlength: 8, 'aria-label': 'Иконка новой сферы' });
  const newColor = h('input', { type: 'color', value: '#6b7280', 'aria-label': 'Цвет новой сферы' });
  const create = async () => {
    if (!newName.value.trim()) return toast('Нужно название');
    try {
      await api('POST', 'spheres', { name: newName.value.trim(), icon: newIcon.value.trim(), color: newColor.value });
      state.spheres = [];
      render();
    } catch (err) { handleError(err); }
  };

  return [
    h('div', { class: 'bar' }, h('h1', {}, 'Сферы'),
      h('div', { class: 'sub' }, 'Цвета, порядок и подсказки для разбора. Сохраняется сразу.')),
    h('div', { class: 'spheres-list' }, active.map(card)),
    h('div', { class: 'section' }, 'Новая сфера'),
    h('div', { class: 'card new-sphere' }, h('div', { class: 'row' }, newIcon, newName, newColor,
      h('button', { class: 'btn primary', type: 'button', onclick: create }, 'Добавить'))),
    archived.length ? [h('div', { class: 'section' }, 'Архив'), h('div', { class: 'spheres-list' }, archived.map((sp) => card(sp, -1)))] : null,
  ];
}

// Поиск объединяет задачи, события, цели и заметки в одном месте.
async function viewSearch(encoded) {
  let query;
  try { query = decodeURIComponent(encoded || ''); } catch { query = encoded || ''; }
  $('search-input').value = query;
  const items = query ? await api('GET', `items?q=${encodeURIComponent(query)}&limit=100`) : [];
  const tasks = items.filter((it) => it.kind !== 'note');
  const notes = items.filter((it) => it.kind === 'note');
  const section = (title, values) => values.length ? h('section', { class: 'panel card' },
    h('div', { class: 'panel-head' }, h('h2', {}, title), h('span', { class: 'count' }, values.length)),
    list(values, { withDate: true, onChange: render })) : null;
  return h('div', { class: 'search-results' },
    h('div', { class: 'page-head' }, h('div', {}, h('div', { class: 'eyebrow' }, 'Поиск по пространству'),
      h('h1', { class: 'page-title' }, query ? `«${query}»` : 'Что найти?'),
      h('p', { class: 'page-description' }, items.length === 100 ? 'Первые 100 результатов. Уточните запрос, чтобы найти нужное.' : `Найдено: ${items.length}`))),
    section('Задачи, события и цели', tasks), section('Заметки', notes),
    !items.length ? h('div', { class: 'panel card empty' }, 'Ничего не найдено. Попробуйте другое слово из названия или заметки.') : null);
}

// ── Роутинг ──────────────────────────────────────────────────────────────────

function go(hash) { location.hash = hash; }

function route() {
  const [, tab = 'day', arg] = location.hash.split('/');
  return { tab, arg };
}

function handleError(err) {
  if (err instanceof AuthError) return showLogin(err.message);
  // Чтение без сети (и без кеша сервис-воркера) — плашка, а не ошибка: экран остаётся как был.
  if (err instanceof TypeError && !navigator.onLine) return setOffline(true);
  if (err instanceof TypeError && /fetch|network|load failed/i.test(err.message)) return setOffline(true);
  console.error(err);
  toast('⚠️ ' + err.message);
}

function showLogin(msg) {
  closeSheet();
  $('fab').hidden = true;
  $('nav-create').hidden = true;
  $('view').replaceChildren(h('div', { class: 'login card' },
    h('h1', {}, 'LifeTask'),
    h('p', {}, 'Напиши боту ', h('b', {}, '/login'), ' — он пришлёт ссылку для входа.'),
    msg ? h('p', { class: 'empty' }, msg) : null));
}

let renderSeq = 0;
async function render() {
  const seq = ++renderSeq;
  const { tab, arg } = route();
  try {
    if (!state.spheres.length) await loadRefs();
    charts.length = 0;
    let nodes;
    switch (tab) {
      case 'week': nodes = await viewWeek(arg || todayStr()); break;
      case 'board': nodes = await viewBoard(); break;
      case 'inbox': nodes = await viewInbox(); break;
      case 'stats': nodes = await viewStats(); break;
      case 'graph': nodes = await viewGraph(); break;
      case 'journal': nodes = await viewJournal(arg || todayStr()); break;
      case 'notes': nodes = await viewNotes(); break;
      case 'spheres': nodes = await viewSpheres(); break;
      case 'search': nodes = await viewSearch(arg); break;
      default: nodes = await viewDay(arg || todayStr());
    }
    if (seq !== renderSeq) return; // пока грузили, пользователь ушёл на другой экран
    $('fab').hidden = tab === 'spheres';
    $('nav-create').hidden = false;
    // Вкладку подсвечиваем только после загрузки: без сети экран остаётся прежним.
    document.querySelectorAll('.nav-link[data-tab]').forEach((a) => a.classList.toggle('active', a.dataset.tab === tab));
    $('view').replaceChildren(...[nodes].flat(Infinity).filter(Boolean));
    window.dispatchEvent(new Event('lifetask:rendered'));
    if (tab === 'stats' || tab === 'graph') charts.forEach((draw) => draw());
    refreshInboxCount();
  } catch (err) {
    handleError(err);
    // The local outbox must remain accessible even if the inbox API is down.
    if (tab === 'inbox' && !(err instanceof AuthError)) {
      const queue = await queueSection();
      if (queue && seq === renderSeq) $('view').replaceChildren(
        h('h1', {}, 'Входящие'), h('p', { class: 'hint' }, 'Не удалось загрузить входящие. Неотправленные изменения сохранены на этом устройстве.'),
        ...queue);
    }
  }
}

async function refreshInboxCount() {
  try {
    const msgs = await api('GET', 'inbox');
    const b = $('inbox-count');
    b.textContent = String(msgs.length);
    b.hidden = msgs.length === 0;
  } catch { /* счётчик не критичен */ }
}

window.addEventListener('hashchange', () => { closeSheet(); render(); });
window.addEventListener('online', () => { setOffline(false); flushQueue(); });
window.addEventListener('offline', () => setOffline(true));
if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch((err) => console.warn('SW:', err));
refreshQueueBadge();
flushQueue();
document.addEventListener('keydown', (e) => e.key === 'Escape' && closeSheet());
document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible') { flushQueue(); render(); } });
$('sheet-backdrop').addEventListener('click', closeSheet);
$('fab').addEventListener('click', () => {
  const { tab, arg } = route();
  if (tab === 'notes') return openEditor(null, render, { kind: 'note' });
  openEditor(null, render, { planned_date: tab === 'day' ? arg || todayStr() : todayStr() });
});
render();
