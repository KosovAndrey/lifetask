"""Тесты garmin_sync без сети: разбор ответов Garmin и отправка в LifeTask.

python3 deploy/garmin_sync_test.py
"""

import http.server
import json
import os
import sys
import threading
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import garmin_sync  # noqa: E402

# Сокращённые ответы Garmin Connect (поля — как в garminconnect/typed.py).
SUMMARY = {"totalSteps": 8432, "restingHeartRate": 54, "averageStressLevel": 31,
           "bodyBatteryHighestValue": 78, "bodyBatteryLowestValue": 12, "privacyProtected": False}
SLEEP = {"dailySleepDTO": {"calendarDate": "2026-10-09", "sleepTimeSeconds": 26100,
                           "sleepScores": {"overall": {"value": 81, "qualifierKey": "GOOD"}}}}


class BuildPayload(unittest.TestCase):
    def test_full_day(self):
        self.assertEqual(garmin_sync.build_payload(SUMMARY, SLEEP), {
            "sleep_min": 435, "sleep_score": 81, "stress_avg": 31, "body_battery": 78, "steps": 8432, "resting_hr": 54})

    def test_missing_data_is_skipped(self):
        # Часы не носили: Garmin отдаёт -1/-2 для стресса, null и нули.
        summary = {"totalSteps": 0, "restingHeartRate": None, "averageStressLevel": -2, "bodyBatteryHighestValue": None}
        self.assertEqual(garmin_sync.build_payload(summary, {"dailySleepDTO": {"sleepTimeSeconds": None}}), {})
        self.assertEqual(garmin_sync.build_payload(None, None), {})
        self.assertEqual(garmin_sync.build_payload({}, {"dailySleepDTO": None}), {})

    def test_out_of_range_dropped(self):
        p = garmin_sync.build_payload({"restingHeartRate": 5, "averageStressLevel": 120, "totalSteps": 100.4}, {})
        self.assertEqual(p, {"steps": 100})


class Server(http.server.BaseHTTPRequestHandler):
    seen = []

    def do_PUT(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        Server.seen.append((self.path, self.headers["Authorization"], body))
        code = 400 if body.get("sleep_min", 0) > 1440 else 200
        self.send_response(code)
        self.end_headers()
        self.wfile.write(b'{"error":"bad"}' if code == 400 else json.dumps(body).encode())

    def log_message(self, *a):
        pass


class Send(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = http.server.HTTPServer(("127.0.0.1", 0), Server)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.url = f"http://127.0.0.1:{cls.srv.server_port}"

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def setUp(self):
        Server.seen.clear()

    def test_put_health(self):
        garmin_sync.put_health(self.url, "tok", "2026-10-09", {"steps": 100})
        self.assertEqual(Server.seen, [("/api/health/2026-10-09", "Bearer tok", {"steps": 100, "source": "garmin"})])

    def test_main_two_days_one_empty(self):
        g = mock.Mock()
        g.get_user_summary.side_effect = lambda d: SUMMARY if d == garmin_sync.dt.date.today().isoformat() else {}
        g.get_sleep_data.return_value = {}
        env = {"LIFETASK_URL": self.url, "API_TOKEN": "tok"}
        with mock.patch.dict(os.environ, env), mock.patch.object(garmin_sync, "garmin_client", return_value=g):
            self.assertEqual(garmin_sync.main(["--days", "2"]), 0)
        self.assertEqual(len(Server.seen), 1)  # вчера данных нет — ничего не шлём
        self.assertEqual(Server.seen[0][2]["steps"], 8432)

    def test_main_reports_failures(self):
        g = mock.Mock()
        g.get_user_summary.side_effect = [RuntimeError("429"), {"totalSteps": 10}]
        g.get_sleep_data.return_value = {}
        with mock.patch.dict(os.environ, {"LIFETASK_URL": self.url, "API_TOKEN": "tok"}), \
                mock.patch.object(garmin_sync, "garmin_client", return_value=g):
            self.assertEqual(garmin_sync.main(["--days", "2"]), 1)  # один день упал — код 1 для cron
        self.assertEqual(len(Server.seen), 1)

    def test_dry_run_sends_nothing(self):
        g = mock.Mock()
        g.get_user_summary.return_value = SUMMARY
        g.get_sleep_data.return_value = SLEEP
        with mock.patch.dict(os.environ, {}, clear=False), mock.patch.object(garmin_sync, "garmin_client", return_value=g):
            self.assertEqual(garmin_sync.main(["--dry-run", "--days", "1"]), 0)
        self.assertEqual(Server.seen, [])


if __name__ == "__main__":
    unittest.main()
