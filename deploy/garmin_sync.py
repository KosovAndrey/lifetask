#!/usr/bin/env python3
"""Garmin Connect → LifeTask: сон, Body Battery, стресс, шаги и пульс покоя по дням.

Берёт сводку дня и сон из Garmin Connect (библиотека garminconnect, неофициальный
API) и отправляет в LifeTask: PUT /api/health/{дата}. Пустые поля сервер не затирает,
поэтому запускать можно сколько угодно раз — по cron раз в час.

Окружение:
  LIFETASK_URL      адрес сервера, например https://lifetask.ru
  API_TOKEN         тот же токен, что у CLI plan (Bearer)
  GARMIN_EMAIL, GARMIN_PASSWORD — нужны только для первого входа (--login)
  GARMINTOKENS      где хранить токены Garmin (по умолчанию ~/.garminconnect)
  HTTPS_PROXY       прокси, если Garmin недоступен напрямую (requests берёт его сам)

Первый вход (спросит код, если включена двухфакторная защита):
  python3 garmin_sync.py --login
Дальше — без пароля, на сохранённых токенах:
  python3 garmin_sync.py            # сегодня и вчера
  python3 garmin_sync.py --days 7   # неделя
  python3 garmin_sync.py --dry-run  # только показать, что отправится
"""

import argparse
import datetime as dt
import json
import os
import sys
import urllib.error
import urllib.request

# Допустимые значения — как CHECK в таблице health_days: вне диапазона сервер ответит 400.
LIMITS = {
    "sleep_min": (0, 1440),
    "sleep_score": (0, 100),
    "stress_avg": (0, 100),
    "body_battery": (0, 100),
    "steps": (0, 10**7),
    "resting_hr": (20, 250),
}


def _int(v):
    if isinstance(v, bool) or v is None:
        return None
    if isinstance(v, (int, float)):
        return int(round(v))
    return None


def build_payload(summary, sleep):
    """Показатели дня из ответов Garmin (сводка дня и сон) в формате LifeTask.

    Garmin отдаёт отрицательные значения и null, когда данных нет (часы не носили,
    стресс не измерен) — такие поля пропускаем, а не шлём нулями.
    """
    summary = summary or {}
    dto = (sleep or {}).get("dailySleepDTO") or {}
    p = {}
    secs = _int(dto.get("sleepTimeSeconds"))
    if secs:
        p["sleep_min"] = secs // 60
    score = ((dto.get("sleepScores") or {}).get("overall") or {}).get("value")
    p["sleep_score"] = _int(score)
    p["stress_avg"] = _int(summary.get("averageStressLevel"))
    # «Утренний заряд»: максимум за день почти всегда приходится на пробуждение.
    p["body_battery"] = _int(summary.get("bodyBatteryHighestValue"))
    p["steps"] = _int(summary.get("totalSteps"))
    p["resting_hr"] = _int(summary.get("restingHeartRate"))
    out = {}
    for k, v in p.items():
        lo, hi = LIMITS[k]
        if v is not None and lo <= v <= hi and not (k == "steps" and v == 0):
            out[k] = v
    return out


def put_health(base_url, token, date, payload, timeout=30):
    body = json.dumps({**payload, "source": "garmin"}).encode()
    req = urllib.request.Request(
        f"{base_url.rstrip('/')}/api/health/{date}",
        data=body,
        method="PUT",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read() or b"null")


def garmin_client(login):
    # Импорт здесь: тесты и --help работают без установленной библиотеки.
    from garminconnect import Garmin

    tokens = os.path.expanduser(os.getenv("GARMINTOKENS", "~/.garminconnect"))
    if login:
        email, password = os.getenv("GARMIN_EMAIL"), os.getenv("GARMIN_PASSWORD")
        if not email or not password:
            sys.exit("Для --login нужны GARMIN_EMAIL и GARMIN_PASSWORD")
        os.makedirs(tokens, mode=0o700, exist_ok=True)
        g = Garmin(email, password, prompt_mfa=lambda: input("Код из письма/приложения Garmin: ").strip())
    else:
        if not os.path.exists(tokens):
            sys.exit(f"Нет токенов Garmin в {tokens} — сначала запусти с --login")
        g = Garmin()
    g.login(tokens)
    return g


def main(argv=None):
    ap = argparse.ArgumentParser(description="Garmin Connect → LifeTask (здоровье по дням)")
    ap.add_argument("--days", type=int, default=2, help="сколько последних дней, включая сегодня (по умолчанию 2)")
    ap.add_argument("--login", action="store_true", help="войти по GARMIN_EMAIL/GARMIN_PASSWORD и сохранить токены")
    ap.add_argument("--dry-run", action="store_true", help="не отправлять, только показать")
    args = ap.parse_args(argv)

    base, token = os.getenv("LIFETASK_URL"), os.getenv("API_TOKEN")
    if not args.dry_run and (not base or not token):
        sys.exit("Нужны LIFETASK_URL и API_TOKEN")

    g = garmin_client(args.login)
    # «Сегодня» — по часовому поясу машины; если сервер в UTC, запускай с TZ как на часах
    # (TZ=Europe/Moscow), иначе после полуночи по часам скрипт будет спрашивать вчерашний день.
    today = dt.date.today()
    failed = 0
    for i in range(max(1, args.days)):
        d = (today - dt.timedelta(days=i)).isoformat()
        try:
            payload = build_payload(g.get_user_summary(d), g.get_sleep_data(d))
        except Exception as e:  # один плохой день не мешает остальным
            print(f"{d}: Garmin не ответил: {e}", file=sys.stderr)
            failed += 1
            continue
        if not payload:
            print(f"{d}: данных нет")
            continue
        if args.dry_run:
            print(f"{d}: {json.dumps(payload, ensure_ascii=False)}")
            continue
        try:
            put_health(base, token, d, payload)
            print(f"{d}: отправлено {json.dumps(payload, ensure_ascii=False)}")
        except urllib.error.HTTPError as e:
            print(f"{d}: LifeTask ответил {e.code}: {e.read().decode(errors='replace')}", file=sys.stderr)
            failed += 1
        except OSError as e:
            print(f"{d}: LifeTask недоступен: {e}", file=sys.stderr)
            failed += 1
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
