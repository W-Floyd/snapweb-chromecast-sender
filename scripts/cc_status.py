#!/usr/bin/env python3
"""Query a Chromecast device by IP and print JSON status to stdout."""
import sys
import json
import traceback

BACKDROP_APP_ID = "E8C28D3C"

def main():
    if len(sys.argv) < 2:
        print(json.dumps({"error": "usage: cc_status.py <host>"}))
        sys.exit(1)

    host = sys.argv[1]
    cast = None
    try:
        import pychromecast

        cast = pychromecast.get_chromecast_from_host((host, 8009, None, None, None))

        cast.wait(timeout=10)
        s = cast.status

        app_id = s.app_id
        display_name = (s.display_name or "").strip()
        is_idle = s.is_stand_by or app_id is None or app_id == BACKDROP_APP_ID

        print(json.dumps({
            "app_id": app_id,
            "display_name": display_name,
            "is_idle": is_idle,
            "volume_level": round(s.volume_level, 2),
            "volume_muted": s.volume_muted,
        }))

    except Exception as e:
        print(json.dumps({"error": str(e), "detail": traceback.format_exc()}))
        sys.exit(1)
    finally:
        if cast:
            try:
                cast.disconnect()
            except Exception:
                pass

if __name__ == "__main__":
    main()
