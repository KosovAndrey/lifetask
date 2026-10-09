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
cd ~/projects/lifeplan && git pull && docker compose up -d --build app files-backup
```

При первом обновлении после добавления `files-backup` запусти этот сервис явно:
`docker compose up -d --build files-backup` (старый deploy.sh может обновлять только `app`).
Каждая сборка Docker выполняет бинарник в итоговом Alpine и проверяет обычную
ошибку без `DATABASE_URL`: это проверяет запуск и наличие `Europe/Moscow`.
Для проверки работающего приложения: `/healthz` — процесс жив, `/readyz` — база доступна.

Миграции 011/012 применяются при запуске автоматически. Старые предложенные планы,
которые меняют существующие задачи, могут получить 409: создай предложение заново,
чтобы оно учитывало текущую версию задачи. Старый ключ идемпотентности без сохранённого
ответа также возвращает 409; сначала проверь состояние, прежде чем повторять операцию.

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
- в Google Drive создаётся папка **LifeTask**: в `Вложения` — файлы задач (из веба и из бота), в `Бэкапы` — копии дампов базы и архивы локальных вложений.
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
`lp_files_backup` архивирует том `files` при запуске и ежедневно в 03:10 по Москве:
`./backups/files/files-<время UTC>.tar.gz`, последние 30 архивов. Архив сначала
пишется во временный файл, проверяется `gzip -t` и только затем публикуется
переименованием; незавершённые вложения `*.tmp` не попадают в него.
Ручной снимок: `docker compose exec files-backup files-backup.sh`.

С подключённым Google приложение при старте и раз в 6 часов копирует свежий
дамп и свежий архив в Drive (`LifeTask/Бэкапы`, по 30 копий каждого вида).
Без Google копии остаются на том же VPS; для защиты от потери сервера нужно
подключить Drive или копировать весь `./backups` на другой сервер.
В логах приложения: `бэкап в Drive file=…`; логи архивации:
`docker compose logs files-backup`. Загрузка в Drive использует существующий
API с чтением архива в память: для большого тома `files` учитывай размер архива
при выборе памяти контейнера.

## Восстановление базы и вложений

На новом VPS сохрани прежний `.env` отдельно от бэкапов: пароли и API-ключи
в дамп не входят. Скопируй репозиторий, восстанови `.env`, создай общую сеть
tryberry (или укажи её имя), собери образ: `docker compose build app`.
Скачай из Drive дамп и архив локальных вложений в `./backups`, сохранив имена.
Нужен архив `files`, сделанный не раньше выбранного дампа: в базе есть ссылки
на вложения. Это два независимых снимка, их время смотри по именам.

Следующие команды рассчитаны на **пустые** тома `pgdata` и `files` на новом
сервере. При восстановлении на прежнем сервере сначала отдельно сохрани его
текущие тома; автоматического удаления данных эти команды не делают.

```bash
DB_DUMP="backups/daily/lifeplan-<дата>.sql.gz"
FILES_ARCHIVE="files-<время UTC>.tar.gz"
gzip -t "$DB_DUMP" "backups/files/$FILES_ARCHIVE"
docker compose stop app backup files-backup
docker compose up -d postgres
# Дождись healthy: docker compose ps postgres
gunzip -c "$DB_DUMP" | docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U lifeplan lifeplan
docker compose run --rm --no-deps -T --user 0:0 --entrypoint sh \
  -e FILES_ARCHIVE="$FILES_ARCHIVE" app -ec \
  'tar -xzpf "/backups/files/$FILES_ARCHIVE" -C /data/files; chown -R 10001:10001 /data/files'
docker compose up -d app backup files-backup
curl --fail http://127.0.0.1:8090/readyz
```

Если архив скачан напрямую в `backups/`, переложи его в `backups/files/` перед
восстановлением. Проверь вход, несколько задач и старое вложение `l:…`.
Вложения `d:…` остаются в `LifeTask/Вложения` в прежнем Google-аккаунте и требуют
доступа к нему. Google-токены возвращаются с дампом; если доступ отозван,
повтори `docker compose exec -it app lifeplan google-auth` и перезапусти `app`.

## Garmin: сон, Body Battery, стресс, шаги

`deploy/garmin_sync.py` раз в час забирает из Garmin Connect сводку дня и сон
(библиотека `garminconnect`, неофициальный API) и отправляет в `PUT /api/health/{дата}`.
Данные видны в «Сегодня», дневнике, аналитике и утреннем брифе. Пустые поля
сервер не затирает, так что повторные запуски безопасны.

```bash
python3 -m venv ~/garmin && ~/garmin/bin/pip install garminconnect
# Первый вход: спросит код, если включена двухфакторная защита; токены лягут в ~/.garminconnect
GARMIN_EMAIL=… GARMIN_PASSWORD=… ~/garmin/bin/python deploy/garmin_sync.py --login --dry-run
```

Cron (пароль больше не нужен, хватает токенов; `TZ` — как на часах):

```
5 * * * * cd ~/lifeplan && TZ=Europe/Moscow LIFETASK_URL=https://lifetask.ru API_TOKEN=… ~/garmin/bin/python deploy/garmin_sync.py >> ~/garmin/sync.log 2>&1
```

Если Garmin недоступен с VPS напрямую — добавь `HTTPS_PROXY=…`. Токены Garmin
живут около года; когда протухнут, в логе будет ошибка входа — повтори `--login`.
Тесты без сети: `python3 deploy/garmin_sync_test.py`.
