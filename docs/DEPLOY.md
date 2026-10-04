# Деплой на VPS

Тот же сервер, что и tryberry, но отдельный compose-проект `lifeplan`.

## Первый запуск

```bash
git clone git@gitlab.com:KosovAndrey/lifeplan.git ~/projects/lifeplan && cd ~/projects/lifeplan
cp .env.example .env && nano .env        # пароль БД, API_TOKEN, ключи
docker network ls | grep default         # имя сети tryberry → TRYBERRY_NETWORK
docker compose up -d --build
docker compose logs -f app               # «migration applied», «tg: бот запущен»
```

1. Напиши боту что угодно: он ответит твоим chat id.
2. Впиши его в `TG_OWNER_ID` и выполни `docker compose up -d app`.
3. Проверь `/today` и голосовое.

## Домен lifetask.ru и веб

Nginx общий с tryberry (контейнер `tryberry_nginx`) и ходит в `lp_app:8090` по общей docker-сети.

1. В reg.ru (или где куплен домен): A-запись `lifetask.ru` и `www` → IP сервера. AAAA не нужна; если хостер добавил её сам, удали.
   Проверка: `dig +short lifetask.ru` выдаёт IP сервера.
2. HTTP-часть и сертификат:
   ```bash
   cp ~/projects/lifeplan/deploy/nginx/lifetask.conf ~/projects/tryberrybot/nginx/conf.d/
   docker exec tryberry_nginx nginx -t && docker exec tryberry_nginx nginx -s reload
   cd ~/projects/tryberrybot && docker compose -f docker-compose.yml -f docker-compose.prod.yml run --rm --entrypoint certbot certbot \
     certonly --webroot -w /var/www/certbot -d lifetask.ru -d www.lifetask.ru --email <почта> --agree-tos --no-eff-email
   ```
3. HTTPS-часть:
   ```bash
   cp ~/projects/lifeplan/deploy/nginx/lifetask-ssl.conf.tpl ~/projects/tryberrybot/nginx/conf.d/lifetask-ssl.conf
   docker exec tryberry_nginx nginx -t && docker exec tryberry_nginx nginx -s reload
   ```
4. В `.env` lifeplan: `PUBLIC_URL=https://lifetask.ru`, затем `docker compose up -d app`.
5. В боте `/login` → ссылка → вход на 90 дней. На iPhone: Safari → «Поделиться» → «На экран „Домой“».

Продлевает сертификат существующий контейнер `tryberry_certbot`, nginx перечитывает его раз в 6 часов.

Офлайн-режим (сервис-воркер) работает только по HTTPS: по голому IP сайт откроется, но без офлайна.

## Обновление

После настройки своего git-сервера (репозиторий `infra`, git.lifetask.ru) — само: пуш в main → тесты → деплой
с healthcheck и откатом (`.forgejo/workflows/ci.yml`).

Вручную (до CI или если CI лежит):

```bash
cd ~/projects/lifeplan && git pull && docker compose up -d --build app
```

## Прокси

Anthropic и Groq с российских IP не отвечают, поэтому `AI_PROXY_URL` указывает на
`pt_xray:8888` из tryberry: контейнер `app` подключён к сети tryberry только ради этого.
Проверка: отправь боту заметку. Если карточка пришла, прокси работает. Если пришло «Записал, но разобрать не вышло», смотри `docker compose logs app | grep parse`.

## Google Calendar, Tasks и Drive

1. В `.env` впиши `GOOGLE_CLIENT_ID` и `GOOGLE_CLIENT_SECRET` (те же, что у tryberry) и выполни `docker compose up -d app`.
2. Проверь в Google Cloud Console → *OAuth consent screen*: статус должен быть **In production**. В статусе *Testing* refresh-токен живёт 7 дней.
   Там же в *APIs & Services → Library* включи **Google Calendar API**, **Google Tasks API** и **Google Drive API** (Drive у tryberry, скорее всего, выключен).
3. Выдай доступ:
   ```bash
   docker compose exec -it app lifeplan google-auth
   ```
   Открой ссылку, разреши доступ. Браузер перекинет на `http://localhost:8765/?code=…`: страница не откроется, так и задумано. Скопируй адрес из строки браузера и вставь в терминал.
4. `docker compose restart app`. В логах должно появиться `Google: синк каждые 5 минут`.

Что синхронизируется:
- в Google Calendar создаётся календарь **LifeTask**. В него попадают задачи и события со временем, цвет зависит от сферы. Перенос, переименование и удаление в Google подтягиваются обратно (удаление = отмена задачи);
- **основной календарь** зеркалится к нам на 2 недели вперёд (только чтение): встречи, добавленные руками, видны в дне и брифах;
- в Google Tasks создаётся список **«Срочно»**. Что туда добавишь, уходит во входящие, а в Google отмечается выполненным;
- в Google Drive создаётся папка **LifeTask**: в `Вложения` — файлы задач (из веба и из бота), в `Бэкапы` — копии дампов базы.
  Доступ `drive.file`: приложение видит только свои файлы, остальной Drive ему недоступен.

Без Google (или если Drive не ответил) файлы ложатся в том `files` на VPS и открываются так же.

## CLI для разборов с ноутбука

API слушает `127.0.0.1:8090` на VPS. С ноутбука ходим через SSH-туннель:

```bash
ssh -N -L 8090:127.0.0.1:8090 <vps> &
# ~/.config/lifeplan/env:
LIFEPLAN_URL=http://127.0.0.1:8090
LIFEPLAN_TOKEN=<API_TOKEN с VPS>
```

(В WSL TCP на localhost виснет. Тогда туннель на сокет: `ssh -N -L $HOME/.lifeplan.sock:127.0.0.1:8090 <vps>`
и `LIFEPLAN_URL=unix:$HOME/.lifeplan.sock`.)

Либо прямо на VPS: `docker compose exec app plan day today` (переменные LIFEPLAN_URL/TOKEN передать через `-e`).

## Бэкапы

`lp_backup` каждый день кладёт дамп в `./backups` (14 дней, 8 недель, 12 месяцев).
С подключённым Google приложение раз в 6 часов копирует свежий дамп в Drive (`LifeTask/Бэкапы`, последние 30).
В логах: `бэкап в Drive file=…`.

Восстановление — в пустую базу (свежий том `pgdata`): `gunzip -c <дамп>.sql.gz | docker compose exec -T postgres psql -U lifeplan lifeplan`.
