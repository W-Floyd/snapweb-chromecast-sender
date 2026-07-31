#!/usr/bin/env python3
"""Query a Chromecast device by IP and print JSON status to stdout."""
import json
import os
import socket
import sys
import threading
import traceback

BACKDROP_APP_ID = "E8C28D3C"
CHROMECAST_PORT = 8009

# Budget, in seconds. These must all stay under the caller's subprocess timeout
# so that we exit on our own terms with a usable message instead of being
# killed with nothing to show for it.
PROBE_TIMEOUT = 2       # plain TCP reachability check
CONNECT_TIMEOUT = 8     # waiting for the device's first status message
DISCONNECT_TIMEOUT = 2  # clean shutdown of the socket client
OVERALL_TIMEOUT = 12    # hard watchdog covering everything above

# Caps on the two free-form strings in the payload. The reader buffers at most
# 64KB of our stdout and drops the rest, and a *truncated* JSON object does not
# parse at all — so an unbounded traceback (a broken pychromecast install prints
# a long one, and chained exceptions make it longer) would not merely be clipped,
# it would take the "error" field down with it and leave the caller with no
# diagnosis whatsoever. The reader also shows only the first 400 characters of
# the message, so nothing useful is lost by bounding these here.
MAX_MESSAGE = 500
MAX_DETAIL = 4000

_emit_lock = threading.Lock()
_emitted = False


def clip(text, limit, keep_tail=False):
    """Bound a free-form string, marking it where it was cut."""
    text = (text or "").strip()
    if len(text) <= limit:
        return text
    # A traceback's last lines name the actual exception, so keep the tail of
    # that one; for a message the opening words are what identify it.
    return "…" + text[-limit:] if keep_tail else text[:limit] + "…"


def emit(payload):
    """Write one JSON object to stdout and flush it.

    Two details matter here. stdout is a pipe, so it is block-buffered: without
    the flush, a later hang gets the process killed with the JSON still in the
    buffer and the caller sees "signal: killed" instead of the real diagnostic.
    And only the first payload is ever written — the watchdog can fire while
    the main thread is emitting, and two concatenated objects are not valid
    JSON to the reader.
    """
    global _emitted
    with _emit_lock:
        if _emitted:
            return False
        _emitted = True
        sys.stdout.write(json.dumps(payload) + "\n")
        sys.stdout.flush()
        return True


def device_is_idle(app_id, is_stand_by):
    """Decide whether the device counts as having nothing of ours on screen.

    Treat an empty app_id like a missing one: some devices report "" rather than
    None when nothing is running, and `app_id is None` alone let that read as
    playing — the monitor then never re-cast the dashboard. Backdrop is the
    screensaver, so a device showing it is idle too; offering its id to the Go
    side's app-id learner would teach the screensaver as our dashboard.

    A separate function so the rule is testable without a device or pychromecast.
    """
    return bool(is_stand_by) or not app_id or app_id == BACKDROP_APP_ID


def reachable(host):
    """Cheap TCP check before handing off to pychromecast.

    pychromecast's get_chromecast_from_host makes a blocking HTTP request to
    determine the cast type, which takes ~30s to give up on a host that is
    simply offline — long past any sensible request timeout. An offline device
    is the common failure, so rule it out fast and with a clear message.
    """
    try:
        with socket.create_connection((host, CHROMECAST_PORT), PROBE_TIMEOUT):
            return True
    except OSError:
        return False


def main():
    # An *empty* host is rejected alongside a missing one. socket.create_connection
    # resolves "" to the loopback interface, so a blank argument would sail
    # through reachable() and report this container's own state as the device's.
    host = sys.argv[1].strip() if len(sys.argv) > 1 else ""
    if not host:
        emit({"error": "usage: cc_status.py <host>"})
        sys.exit(1)

    # Backstop for anything in pychromecast that blocks past its own timeouts.
    # os._exit skips the finally block below on purpose: a disconnect that is
    # already wedged is exactly what we are escaping.
    def on_timeout():
        emit({"error": "timed out after %ds querying %s" % (OVERALL_TIMEOUT, host)})
        os._exit(1)

    watchdog = threading.Timer(OVERALL_TIMEOUT, on_timeout)
    watchdog.daemon = True
    watchdog.start()

    cast = None
    try:
        if not reachable(host):
            emit({"error": "%s is not reachable on port %d" % (host, CHROMECAST_PORT)})
            sys.exit(1)

        import pychromecast

        cast = pychromecast.get_chromecast_from_host((host, CHROMECAST_PORT, None, None, None))

        cast.wait(timeout=CONNECT_TIMEOUT)
        s = cast.status
        if s is None:
            emit({"error": "no status received from %s" % host})
            sys.exit(1)

        app_id = s.app_id
        # Bounded like the error strings below: this is whatever the receiver app
        # calls itself, it is rendered straight onto the device card, and nothing
        # in the protocol says how long it may be.
        display_name = clip(s.display_name, MAX_MESSAGE)
        is_idle = device_is_idle(app_id, s.is_stand_by)

        # volume_level/volume_muted are None until the first status update lands.
        volume_level = round(s.volume_level, 2) if s.volume_level is not None else None

        emit({
            "app_id": app_id,
            "display_name": display_name,
            "is_idle": is_idle,
            "volume_level": volume_level,
            "volume_muted": s.volume_muted,
        })

    except SystemExit:
        raise
    except Exception as e:
        # Some exceptions stringify to "" (e.g. bare socket errors). An empty
        # "error" reads as success on the Go side, which then reports the
        # device as playing, so always fall back to the exception type name.
        message = clip(str(e), MAX_MESSAGE) or type(e).__name__
        emit({
            "error": message,
            "detail": clip(traceback.format_exc(), MAX_DETAIL, keep_tail=True),
        })
        sys.exit(1)
    finally:
        # disconnect() first, watchdog.cancel() after. Cancelling first left the
        # riskiest call in the script — the one the watchdog exists for — running
        # with no backstop: a disconnect that ignores its own timeout then hung
        # until the caller's 15s context killed us, holding a subprocess open for
        # every poll. The payload is already flushed by now, so the watchdog
        # firing here costs nothing; emit() drops its second object.
        if cast:
            try:
                # Bounded: an unreachable device leaves the socket client in a
                # connect-retry loop, and an unbounded disconnect() joins that
                # thread forever.
                cast.disconnect(timeout=DISCONNECT_TIMEOUT)
            except Exception:
                pass
        watchdog.cancel()


if __name__ == "__main__":
    main()
