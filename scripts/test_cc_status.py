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
import threading
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


_UNSET = object()


class FakeStatus:
    """The subset of pychromecast's CastStatus that the helper reads."""

    def __init__(self, app_id=None, display_name=None, is_stand_by=None,
                 volume_level=None, volume_muted=None):
        self.app_id = app_id
        self.display_name = display_name
        self.is_stand_by = is_stand_by
        self.volume_level = volume_level
        self.volume_muted = volume_muted


class FakeCast:
    def __init__(self, status):
        self.status = status
        self.wait_timeouts = []
        self.disconnect_timeouts = []

    def wait(self, timeout=None):
        self.wait_timeouts.append(timeout)

    def disconnect(self, timeout=None):
        self.disconnect_timeouts.append(timeout)


class MainTest(unittest.TestCase):
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

    def run_with_status(self, status=_UNSET, expect_exit=None, **status_fields):
        """Drive main() against a stand-in pychromecast.

        The real one needs a device on the network; the module is imported inside
        main() precisely so an offline host never pays for it, which also means it
        can be substituted here. Returns the fake cast, so the caller can check
        what main() did with it.
        """
        if status is _UNSET:
            status = FakeStatus(**status_fields)
        cast = FakeCast(status)
        self.module = module = mock.Mock()
        module.get_chromecast_from_host.return_value = cast
        timers = []
        real_timer = threading.Timer

        def record_timer(interval, fn):
            timer = real_timer(interval, fn)
            timers.append(timer)
            return timer

        with mock.patch.dict(sys.modules, {"pychromecast": module}):
            with mock.patch.object(threading, "Timer", record_timer):
                with mock.patch.object(cc_status, "reachable", return_value=True):
                    with mock.patch.object(sys, "argv", ["cc_status.py", "192.0.2.1"]):
                        if expect_exit is None:
                            cc_status.main()
                        else:
                            with self.assertRaises(SystemExit) as exit_ctx:
                                cc_status.main()
                            self.assertEqual(exit_ctx.exception.code, expect_exit)
        # The watchdog has to be cancelled however main() left, on the success path
        # as much as the failure ones: its callback ends in os._exit, so one left
        # armed takes the whole process down — here the test runner — twelve
        # seconds later, with no traceback to say why.
        self.assertEqual([t.interval for t in timers], [cc_status.OVERALL_TIMEOUT])
        timers[0].join(5)
        self.assertFalse(timers[0].is_alive(), "the watchdog was left running")
        return cast

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

    def test_the_status_payload_carries_what_the_go_side_reads(self):
        # The success path, with pychromecast stood in for: it is the only path
        # that produces a status at all, and none of it — the idle rule, the
        # rounding, the caps — was reachable without a device.
        cast = self.run_with_status(app_id="84912283", display_name="DashCast",
                                    is_stand_by=False, volume_level=0.4567,
                                    volume_muted=False)
        payload = json.loads(self.stdout.getvalue())
        self.assertEqual(payload, {
            "app_id": "84912283",
            "display_name": "DashCast",
            "is_idle": False,
            "volume_level": 0.46,
            "volume_muted": False,
        })
        # No "error" key at all: an empty one would read as success on the Go
        # side, and a present-but-empty one is the same thing.
        self.assertNotIn("error", payload)
        # The socket client is left in a connect-retry loop by an unreachable
        # device, and an unbounded disconnect() joins that thread forever.
        self.assertEqual(cast.disconnect_timeouts, [cc_status.DISCONNECT_TIMEOUT])
        # The device is addressed by IP and port, with no discovery: mDNS does not
        # work under Docker bridge networking, which is what this helper exists for.
        self.module.get_chromecast_from_host.assert_called_once_with(
            ("192.0.2.1", cc_status.CHROMECAST_PORT, None, None, None))
        # Every wait in the budget is bounded, or the watchdog is the only thing
        # left to end the process and it does so with no payload on the pipe.
        self.assertEqual(cast.wait_timeouts, [cc_status.CONNECT_TIMEOUT])

    def test_an_unset_volume_is_reported_as_null_not_rounded(self):
        # volume_level is None until the first status update lands, and round()
        # would raise inside the success path.
        self.run_with_status(app_id="84912283", display_name="DashCast",
                             is_stand_by=False, volume_level=None,
                             volume_muted=None)
        payload = json.loads(self.stdout.getvalue())
        self.assertIsNone(payload["volume_level"])
        self.assertFalse(payload["is_idle"])

    def test_both_device_supplied_strings_are_bounded(self):
        # The reader keeps only the first 64KB of stdout and a *truncated* JSON
        # object does not parse at all, so one unbounded field takes the whole
        # payload down — status, error and all. display_name is rendered onto the
        # device card; app_id is compared against, and stored as, the id our own
        # casts run under. Nothing in the cast protocol bounds either.
        long_id = "A" * (cc_status.MAX_MESSAGE * 40)
        self.run_with_status(app_id=long_id, display_name="B" * (cc_status.MAX_MESSAGE * 40),
                             is_stand_by=False, volume_level=1.0, volume_muted=False)
        raw = self.stdout.getvalue()
        payload = json.loads(raw)  # must still parse, which is the whole point
        self.assertLess(len(raw), 64 << 10)
        for field in ("app_id", "display_name"):
            self.assertEqual(len(payload[field]), cc_status.MAX_MESSAGE + 1,
                             "%s was not clipped" % field)
            self.assertTrue(payload[field].endswith("…"))
        # And a running app stays a running app: the idle rule is applied to the
        # raw id, so clipping cannot turn one value into another.
        self.assertFalse(payload["is_idle"])

    def test_the_screensaver_is_reported_idle_through_the_whole_path(self):
        # Backdrop names an app while the device has nothing of ours on screen.
        # The Go side only offers a *playing* device's app id to its learner, so
        # this flag is what keeps the screensaver from becoming our dashboard.
        self.run_with_status(app_id=cc_status.BACKDROP_APP_ID, display_name="Backdrop",
                             is_stand_by=False, volume_level=0.5, volume_muted=False)
        self.assertTrue(json.loads(self.stdout.getvalue())["is_idle"])

    def test_a_device_that_never_reports_a_status_says_so(self):
        self.run_with_status(status=None, expect_exit=1)
        payload = json.loads(self.stdout.getvalue())
        self.assertIn("no status received", payload["error"])
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
