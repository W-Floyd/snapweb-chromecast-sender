#!/usr/bin/env python3
"""Tests for cc_status.py.

Needs neither pychromecast nor a device: everything here is the part of the
helper that decides what the Go side gets to see. The import is safe because the
script does its work under a __main__ guard.

    python3 -m unittest discover -s scripts

The Go tests cover the other half of the contract — the timeout budget is read
straight out of this script by TestStatusQueryTimeoutOutlastsTheScriptWatchdog,
so the two languages cannot drift apart on it.
"""
import io
import json
import socket
import sys
import unittest
from unittest import mock

import cc_status


class ClipTest(unittest.TestCase):
    def test_short_text_is_returned_stripped_and_whole(self):
        self.assertEqual(cc_status.clip("  hello  ", 100), "hello")
        self.assertEqual(cc_status.clip("hello", 5), "hello")

    def test_none_becomes_empty(self):
        # display_name is None until the first status update lands, and an
        # unguarded len() would raise inside the success path.
        self.assertEqual(cc_status.clip(None, 10), "")

    def test_a_message_keeps_its_opening_words(self):
        got = cc_status.clip("abcdefghij", 4)
        self.assertEqual(got, "abcd…")

    def test_a_traceback_keeps_its_tail(self):
        # The last lines name the actual exception; the frames above it do not.
        got = cc_status.clip("abcdefghij", 4, keep_tail=True)
        self.assertEqual(got, "…ghij")

    def test_the_caps_leave_the_payload_parseable(self):
        # The reader keeps only the first 64KB of stdout, and a *truncated* JSON
        # object does not parse at all — so an unbounded traceback in "detail"
        # would take the "error" field down with it and leave the caller with no
        # diagnosis whatsoever.
        payload = json.dumps({
            "error": "e" * cc_status.MAX_MESSAGE,
            "detail": "d" * cc_status.MAX_DETAIL,
        })
        self.assertLess(len(payload), 64 << 10)


class DeviceIsIdleTest(unittest.TestCase):
    def test_nothing_running_is_idle(self):
        # "" rather than None is what some devices report, and reading that as
        # playing meant the monitor never re-cast the dashboard.
        for app_id in (None, ""):
            self.assertTrue(cc_status.device_is_idle(app_id, False))

    def test_the_screensaver_is_idle(self):
        self.assertTrue(cc_status.device_is_idle(cc_status.BACKDROP_APP_ID, False))

    def test_standby_is_idle_whatever_is_loaded(self):
        self.assertTrue(cc_status.device_is_idle("84912283", True))

    def test_a_running_app_is_not_idle(self):
        self.assertFalse(cc_status.device_is_idle("84912283", False))
        # is_stand_by is None until the first status update lands, and bool(None)
        # must not be allowed to read as standby.
        self.assertFalse(cc_status.device_is_idle("84912283", None))


class EmitTest(unittest.TestCase):
    def setUp(self):
        self.stdout = io.StringIO()
        patcher = mock.patch.object(sys, "stdout", self.stdout)
        patcher.start()
        self.addCleanup(patcher.stop)
        # Module-level latch, so it has to be put back for the next test.
        cc_status._emitted = False
        self.addCleanup(setattr, cc_status, "_emitted", False)

    def test_one_json_object_on_its_own_line(self):
        self.assertTrue(cc_status.emit({"error": "boom"}))
        self.assertEqual(self.stdout.getvalue(), '{"error": "boom"}\n')
        self.assertEqual(json.loads(self.stdout.getvalue()), {"error": "boom"})

    def test_only_the_first_payload_is_written(self):
        # The watchdog thread can fire while the main thread is emitting, and two
        # concatenated objects are not valid JSON to the reader.
        self.assertTrue(cc_status.emit({"app_id": "84912283"}))
        self.assertFalse(cc_status.emit({"error": "timed out"}))
        self.assertEqual(json.loads(self.stdout.getvalue()), {"app_id": "84912283"})


class ReachableTest(unittest.TestCase):
    """The plain-TCP pre-check that keeps an offline device from costing ~30s."""

    def test_a_listening_host_is_reachable(self):
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        self.addCleanup(listener.close)
        port = listener.getsockname()[1]
        with mock.patch.object(cc_status, "CHROMECAST_PORT", port):
            self.assertTrue(cc_status.reachable("127.0.0.1"))

    def test_a_closed_port_is_not_reachable(self):
        # Bind and close, so nothing is listening on a port nothing else claimed.
        probe = socket.socket()
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
        probe.close()
        with mock.patch.object(cc_status, "CHROMECAST_PORT", port):
            self.assertFalse(cc_status.reachable("127.0.0.1"))


class MainArgumentTest(unittest.TestCase):
    def setUp(self):
        self.stdout = io.StringIO()
        patcher = mock.patch.object(sys, "stdout", self.stdout)
        patcher.start()
        self.addCleanup(patcher.stop)
        cc_status._emitted = False
        self.addCleanup(setattr, cc_status, "_emitted", False)

    def run_main(self, argv):
        with mock.patch.object(sys, "argv", argv):
            with self.assertRaises(SystemExit) as exit_ctx:
                cc_status.main()
        return exit_ctx.exception.code, json.loads(self.stdout.getvalue())

    def test_a_missing_host_is_reported_not_guessed(self):
        code, payload = self.run_main(["cc_status.py"])
        self.assertEqual(code, 1)
        self.assertIn("usage", payload["error"])

    def test_a_blank_host_is_rejected_before_any_connection(self):
        # socket.create_connection resolves "" to the loopback interface, so a
        # blank argument would sail through reachable() and report this
        # container's own state as the device's.
        with mock.patch.object(cc_status, "reachable") as never:
            code, payload = self.run_main(["cc_status.py", "   "])
        never.assert_not_called()
        self.assertEqual(code, 1)
        self.assertIn("usage", payload["error"])

    def test_an_unreachable_host_says_so_and_never_imports_pychromecast(self):
        # The pre-check is not redundant: get_chromecast_from_host makes a
        # blocking HTTP call that takes ~30s to give up on an offline host, which
        # blew past every timeout in the budget.
        with mock.patch.object(cc_status, "reachable", return_value=False):
            code, payload = self.run_main(["cc_status.py", "192.0.2.1"])
        self.assertEqual(code, 1)
        self.assertIn("not reachable", payload["error"])
        self.assertIn("192.0.2.1", payload["error"])

    def test_a_failure_always_carries_a_message(self):
        # Some exceptions stringify to "" (bare socket errors do). An empty
        # "error" reads as success on the Go side, which then reports the device
        # as playing.
        with mock.patch.object(cc_status, "reachable", side_effect=OSError()):
            code, payload = self.run_main(["cc_status.py", "192.0.2.1"])
        self.assertEqual(code, 1)
        self.assertEqual(payload["error"], "OSError")
        self.assertIn("detail", payload)


if __name__ == "__main__":
    unittest.main()
