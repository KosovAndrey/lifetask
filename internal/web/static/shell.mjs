const $ = (id) => document.getElementById(id);
const sections = {
  day: ['Планирование', 'Мой день'], week: ['Планирование', 'Неделя'],
  board: ['Планирование', 'Все задачи'], inbox: ['Планирование', 'Входящие'],
  notes: ['Личное', 'Заметки'], journal: ['Личное', 'Дневник'],
  stats: ['Обзор', 'Аналитика'], graph: ['Обзор', 'Связи'],
  spheres: ['Настройки', 'Сферы жизни'], search: ['Поиск', 'Результаты'],
};
let returnFocus;
let navigationBackground = [];

function closeNavigation() {
  if (!$('sidebar').classList.contains('is-open')) return;
  $('sidebar').classList.remove('is-open');
  $('nav-backdrop').hidden = true;
  $('nav-more').setAttribute('aria-expanded', 'false');
  for (const [element, previous] of navigationBackground) element.inert = previous;
  navigationBackground = [];
  $('sidebar').removeAttribute('role');
  $('sidebar').removeAttribute('aria-modal');
  document.body.classList.remove('nav-open');
  returnFocus?.focus();
}

function updateNavigation() {
  const tab = location.hash.split('/')[1] || 'day';
  const [group, title] = sections[tab] || sections.day;
  $('section-group').textContent = group;
  $('section-name').textContent = title;
  document.title = `${title} · LifeTask`;
  document.querySelectorAll('.nav-link[data-tab]').forEach((link) => {
    const active = link.dataset.tab === tab;
    link.classList.toggle('active', active);
    if (active) link.setAttribute('aria-current', 'page');
    else link.removeAttribute('aria-current');
  });
  $('nav-more').classList.toggle('active', !['day', 'week', 'board', 'inbox'].includes(tab));
  closeNavigation();
}

$('nav-more').addEventListener('click', () => {
  returnFocus = document.activeElement;
  $('sidebar').classList.add('is-open');
  $('nav-backdrop').hidden = false;
  $('nav-more').setAttribute('aria-expanded', 'true');
  navigationBackground = [...document.body.children]
    .filter((el) => el !== $('sidebar') && el !== $('nav-backdrop') && el !== $('toast'))
    .map((el) => [el, el.inert]);
  for (const [element] of navigationBackground) element.inert = true;
  $('sidebar').setAttribute('role', 'dialog');
  $('sidebar').setAttribute('aria-modal', 'true');
  document.body.classList.add('nav-open');
  $('nav-close').focus();
});
$('nav-close').addEventListener('click', closeNavigation);
$('nav-backdrop').addEventListener('click', closeNavigation);
$('sidebar').addEventListener('click', (e) => { if (e.target.closest('a')) closeNavigation(); });
$('nav-create').addEventListener('click', () => { closeNavigation(); $('fab').click(); });
$('global-search').addEventListener('submit', (e) => {
  e.preventDefault();
  const query = $('search-input').value.trim();
  if (query) { location.hash = `#/search/${encodeURIComponent(query)}`; $('search-input').blur(); }
});
document.querySelector('.skip-link').addEventListener('click', (e) => {
  e.preventDefault();
  $('view').focus();
});
document.addEventListener('keydown', (e) => {
  if ($('sidebar').classList.contains('is-open')) {
    if (e.key === 'Escape') { e.preventDefault(); closeNavigation(); }
    if (e.key === 'Tab') {
      const targets = [...$('sidebar').querySelectorAll('a,button')].filter((el) => el.getClientRects().length);
      const first = targets[0], last = targets.at(-1);
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }
    return;
  }
  if (e.ctrlKey || e.metaKey || e.altKey || e.target.closest('input,textarea,select,[contenteditable="true"]') || !$('sheet').hidden) return;
  if (e.key === '/') { e.preventDefault(); $('search-input').focus(); }
  if (e.key.toLowerCase() === 'n' && !$('fab').hidden) { e.preventDefault(); $('fab').click(); }
});
matchMedia('(min-width: 900px)').addEventListener('change', closeNavigation);
window.addEventListener('hashchange', closeNavigation);
window.addEventListener('lifetask:rendered', updateNavigation);
updateNavigation();

const compactSearch = matchMedia('(max-width: 599px)');
const updateSearchHint = () => { $('search-input').placeholder = compactSearch.matches ? 'Поиск' : 'Найти задачу или заметку'; };
compactSearch.addEventListener('change', updateSearchHint);
updateSearchHint();
