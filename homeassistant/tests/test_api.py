"""Unit tests of the freedo API client with a fake HTTP session."""

import asyncio
import json

import aiohttp
import pytest

from api import (
    FreedoApiError,
    FreedoAuthError,
    FreedoClient,
    FreedoConnectionError,
)


class FakeResponse:
    def __init__(self, status, body):
        self.status = status
        self._body = body

    async def json(self, content_type=None):
        if isinstance(self._body, str):
            return json.loads(self._body)
        return self._body

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False


class FakeSession:
    """Records each request and answers from a route table."""

    def __init__(self, routes=None, error=None):
        self.routes = routes or {}
        self.error = error
        self.calls = []

    def request(self, method, url, json=None, headers=None, timeout=None):
        self.calls.append({"method": method, "url": url, "json": json, "headers": headers})
        if self.error is not None:
            raise self.error
        path = url.split(":8080", 1)[-1]
        status, body = self.routes.get((method, path), (404, {"error": "not found"}))
        return FakeResponse(status, body)


def run(coro):
    return asyncio.run(coro)


def test_bearer_header_and_url():
    session = FakeSession({("GET", "/api/home"): (200, {"away": False, "scheduled": {}})})
    client = FreedoClient(session, "10.0.0.2", 8080, "s3cret")
    assert run(client.home()) == {"away": False, "scheduled": {}}
    call = session.calls[0]
    assert call["url"] == "http://10.0.0.2:8080/api/home"
    assert call["headers"]["Authorization"] == "Bearer s3cret"
    assert call["json"] is None


def test_no_token_sends_no_authorization():
    session = FakeSession({("GET", "/api/home"): (200, {"away": False})})
    run(FreedoClient(session, "h", 8080, "").home())
    assert "Authorization" not in session.calls[0]["headers"]


def test_ipv6_host_is_bracketed():
    session = FakeSession()
    client = FreedoClient(session, "fd00::2", 8080)
    with pytest.raises(FreedoApiError):
        run(client.home())
    assert session.calls[0]["url"] == "http://[fd00::2]:8080/api/home"


def test_401_raises_auth_error():
    session = FakeSession({("GET", "/api/home"): (401, {"error": "unauthorized"})})
    with pytest.raises(FreedoAuthError):
        run(FreedoClient(session, "h", 8080, "wrong").home())


def test_error_status_carries_daemon_message():
    session = FakeSession(
        {("POST", "/api/outdoor"): (400, {"error": "tempC is required, within -60..60"})}
    )
    with pytest.raises(FreedoApiError) as err:
        run(FreedoClient(session, "h", 8080).push_outdoor(99))
    assert err.value.status == 400
    assert err.value.message == "tempC is required, within -60..60"


def test_non_json_error_body():
    session = FakeSession({("GET", "/api/health"): (503, "not json")})
    with pytest.raises(FreedoApiError) as err:
        run(FreedoClient(session, "h", 8080).health())
    assert err.value.status == 503


def test_connection_errors():
    for exc in (aiohttp.ClientConnectionError("refused"), asyncio.TimeoutError()):
        with pytest.raises(FreedoConnectionError):
            run(FreedoClient(FakeSession(error=exc), "h", 8080).home())


def test_write_payloads():
    session = FakeSession(
        {
            ("POST", "/api/outdoor"): (200, {"outdoor": {"tempC": 4.5}}),
            ("POST", "/api/zones/3/window"): (200, {"open": True}),
            ("PATCH", "/api/home"): (200, {"away": True}),
            ("PATCH", "/api/install"): (200, {"rev": 8, "offline_schedule": False}),
        }
    )
    client = FreedoClient(session, "h", 8080)
    run(client.push_outdoor(4.5))
    run(client.push_window(3, True))
    run(client.set_away(True))
    assert run(client.set_offline_schedule(False)) == {"rev": 8, "offline_schedule": False}
    assert [(c["method"], c["url"].split(":8080", 1)[-1], c["json"]) for c in session.calls] == [
        ("POST", "/api/outdoor", {"tempC": 4.5}),
        ("POST", "/api/zones/3/window", {"open": True}),
        ("PATCH", "/api/home", {"away": True}),
        ("PATCH", "/api/install", {"offline_schedule": False}),
    ]


def test_install_read():
    body = {"rev": 7, "offline_schedule": True}
    session = FakeSession({("GET", "/api/install"): (200, body)})
    assert run(FreedoClient(session, "h", 8080).install()) == body
    assert session.calls[0]["url"] == "http://h:8080/api/install"


def test_zone_command_payloads():
    ok = (200, {"queued": True, "pending": 1, "zone": 3, "target": "VA0000000001"})
    session = FakeSession(
        {
            ("POST", "/api/zones/3/setpoint"): ok,
            ("POST", "/api/zones/3/auto"): ok,
            ("POST", "/api/zones/3/boost"): ok,
            ("POST", "/api/zones/0/setpoint"): ok,
            ("POST", "/api/zones/0/off"): ok,
        }
    )
    client = FreedoClient(session, "h", 8080)
    assert run(client.zone_setpoint(3, 21.5)) == ok[1]
    run(client.zone_setpoint(3, 19, "timer", 1800))
    run(client.zone_auto(3))
    run(client.zone_boost(3, True))
    run(client.zone_setpoint(0, 50))
    run(client.zone_off(0))
    run(client.zone_off(0, "timer", 3600))
    assert [(c["url"].split(":8080", 1)[-1], c["json"]) for c in session.calls] == [
        ("/api/zones/3/setpoint", {"setpointC": 21.5, "mode": "manual"}),
        ("/api/zones/3/setpoint", {"setpointC": 19, "mode": "timer", "durationSec": 1800}),
        ("/api/zones/3/auto", None),
        ("/api/zones/3/boost", {"on": True}),
        ("/api/zones/0/setpoint", {"setpointC": 50, "mode": "manual"}),
        ("/api/zones/0/off", {"mode": "manual"}),
        ("/api/zones/0/off", {"mode": "timer", "durationSec": 3600}),
    ]
    assert all(c["method"] == "POST" for c in session.calls)


def test_room_off_payloads():
    ok = (200, {"queued": True, "pending": 1, "zone": 3, "target": "VA0000000001"})
    session = FakeSession({("POST", "/api/zones/3/off"): ok})
    client = FreedoClient(session, "h", 8080)
    assert run(client.zone_off(3)) == ok[1]
    run(client.zone_off(3, "until_next"))
    assert [(c["method"], c["url"].split(":8080", 1)[-1], c["json"]) for c in session.calls] == [
        ("POST", "/api/zones/3/off", {"mode": "manual"}),
        ("POST", "/api/zones/3/off", {"mode": "until_next"}),
    ]


def test_display_payloads():
    ok = (200, {"queued": True, "pending": 1,
                "display": {"orientation": "vertical", "notification": True}})
    session = FakeSession({("PUT", "/api/nodes/aaaa/display"): ok})
    client = FreedoClient(session, "h", 8080)
    assert run(client.set_display("aaaa", orientation="vertical")) == ok[1]
    run(client.set_display("aaaa", notification=False))
    assert [(c["method"], c["url"].split(":8080", 1)[-1], c["json"]) for c in session.calls] == [
        ("PUT", "/api/nodes/aaaa/display", {"orientation": "vertical"}),
        ("PUT", "/api/nodes/aaaa/display", {"notification": False}),
    ]


def test_valve_command_payloads():
    ok = (200, {"queued": True, "pending": 1})
    session = FakeSession(
        {
            ("POST", "/api/nodes/aaaa/childlock"): ok,
            ("POST", "/api/nodes/aaaa/identify"): ok,
            ("POST", "/api/nodes/aaaa/calibrate"): ok,
            ("POST", "/api/zones/3/offset"): (200, {"rev": 8}),
        }
    )
    client = FreedoClient(session, "h", 8080)
    run(client.set_child_lock("aaaa", True))
    run(client.identify("aaaa"))
    run(client.calibrate("aaaa"))
    assert run(client.set_zone_offset(3, -0.7)) == {"rev": 8}
    assert [(c["method"], c["url"].split(":8080", 1)[-1], c["json"]) for c in session.calls] == [
        ("POST", "/api/nodes/aaaa/childlock", {"on": True}),
        ("POST", "/api/nodes/aaaa/identify", None),
        ("POST", "/api/nodes/aaaa/calibrate", None),
        ("POST", "/api/zones/3/offset", {"offsetC": -0.7}),
    ]


def test_install_zone_patch():
    body = {"rev": 9, "warnings": [], "zone": {"number": 1, "early_start": True}}
    session = FakeSession({("PATCH", "/api/install/zones/1"): (200, body)})
    client = FreedoClient(session, "h", 8080)
    assert run(client.patch_install_zone(1, early_start=True)) == body
    run(client.patch_install_zone(1, device_overlay_end="timer", device_overlay_end_sec=3600))
    assert [(c["method"], c["url"].split(":8080", 1)[-1], c["json"]) for c in session.calls] == [
        ("PATCH", "/api/install/zones/1", {"early_start": True}),
        ("PATCH", "/api/install/zones/1",
         {"device_overlay_end": "timer", "device_overlay_end_sec": 3600}),
    ]


def test_zone_schedule():
    view = {"zone": 1, "members": ["aaaa"], "programs": []}
    session = FakeSession(
        {
            ("GET", "/api/zones/1/schedule"): (200, view),
            ("GET", "/api/zones/2/schedule"): (404, {"error": "zone 2 has no schedule"}),
            ("GET", "/api/zones/3/schedule"): (503, {"error": "no installation"}),
            ("PUT", "/api/zones/1/schedule"): (200, view),
        }
    )
    client = FreedoClient(session, "h", 8080)
    assert run(client.zone_schedule(1)) == view
    # A zone with no schedule reads as none, any other error stays one.
    assert run(client.zone_schedule(2)) is None
    with pytest.raises(FreedoApiError):
        run(client.zone_schedule(3))
    programs = [{"days": ["mon"], "entries": [{"minute": 0, "setpoint_c": 17, "enabled": True}]}]
    assert run(client.set_zone_schedule(1, programs)) == view
    assert session.calls[-1]["method"] == "PUT"
    assert session.calls[-1]["json"] == {"programs": programs}


def test_flow_limit_payload():
    circuits = {"rev": 7, "circuits": [{"id": 1, "name": "Heating", "heat_source": "0000000000000001"}]}
    session = FakeSession(
        {
            ("GET", "/api/install/circuits"): (200, circuits),
            ("POST", "/api/boiler/flow"): (200, {"queued": True}),
        }
    )
    client = FreedoClient(session, "h", 8080)
    assert run(client.install_circuits()) == circuits
    run(client.set_flow_limit(1, 52, "0000000000000001"))
    assert session.calls[-1]["json"] == {"tempC": 52, "id": 1, "target": "0000000000000001"}
