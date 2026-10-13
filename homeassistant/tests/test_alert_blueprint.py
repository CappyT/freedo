"""Behavior tests of the alert notification blueprint, against the Home
Assistant automation engine.

They run only with pytest-homeassistant-custom-component installed. The tests
use a fake Alert event entity, a fake Window sensor and a fake notify entity.
"""

import pathlib
import shutil
from datetime import timedelta

import pytest

pytest.importorskip("pytest_homeassistant_custom_component")

from homeassistant.setup import async_setup_component  # noqa: E402
from homeassistant.util import dt as dt_util  # noqa: E402
from pytest_homeassistant_custom_component.common import (  # noqa: E402
    async_fire_time_changed,
    async_mock_service,
)

from test_blueprint import flush  # noqa: E402

BLUEPRINT = (
    pathlib.Path(__file__).resolve().parents[1]
    / "blueprints"
    / "automation"
    / "freedo"
    / "alert_notification.yaml"
)
ALERT = "event.freedo_alert"
PHONE = "notify.phone"
WINDOW = "binary_sensor.living_window"


@pytest.fixture(autouse=True)
async def detach_triggers(hass):
    """Turn the automation off at the end, so a waiting run leaves no timer
    behind."""
    yield
    if hass.services.has_service("automation", "turn_off"):
        await hass.services.async_call(
            "automation", "turn_off", {"entity_id": "all"}, blocking=True
        )


async def start(hass, freezer, time="12:00:00", **inputs):
    """Install the blueprint at time of 2026-12-01, create the automation and
    return the notify calls."""
    await hass.config.async_set_time_zone("UTC")
    freezer.move_to(f"2026-12-01 {time}")
    dest = pathlib.Path(hass.config.path("blueprints/automation/freedo"))
    await hass.async_add_executor_job(
        lambda: (dest.mkdir(parents=True, exist_ok=True), shutil.copy(BLUEPRINT, dest))
    )
    hass.states.async_set(ALERT, "unknown", {"event_type": None})
    hass.states.async_set(WINDOW, "off", {"friendly_name": "Living Window"})
    calls = async_mock_service(hass, "notify", "send_message")
    assert await async_setup_component(
        hass,
        "automation",
        {
            "automation": {
                "use_blueprint": {
                    "path": "freedo/alert_notification.yaml",
                    "input": {"alert_event": ALERT, "notify_targets": [PHONE], **inputs},
                }
            }
        },
    )
    await hass.async_block_till_done()
    assert [s.state for s in hass.states.async_all("automation")] == ["on"]
    return calls


async def alert(hass, event_type, room=None, device=None, details=()):
    """The Alert entity gets an event: its state is the time of the event."""
    hass.states.async_set(
        ALERT,
        dt_util.utcnow().isoformat(timespec="milliseconds"),
        {"event_type": event_type, "room": room, "device": device, "details": list(details)},
    )
    await flush(hass)


def sent(calls):
    return [(c.data["entity_id"], c.data["title"], c.data["message"]) for c in calls]


def messages(calls):
    return [c.data["message"] for c in calls]


async def settle(hass, freezer, **elapsed):
    freezer.tick(timedelta(**elapsed))
    async_fire_time_changed(hass)
    await flush(hass)


@pytest.mark.parametrize(
    ("event", "message"),
    [
        (
            ("low_battery", "Living", "Sofa (VA0000000001)", []),
            "Low battery: Sofa (VA0000000001) in Living.",
        ),
        (
            ("device_fault", "Living", "Sofa (VA0000000001)", ["stall", "stackLow"]),
            "Device fault: Sofa (VA0000000001) in Living (motor blocked, low internal memory).",
        ),
        (
            ("valve_warning", "Living", "Sofa (VA0000000001)", ["notMounted"]),
            "Valve warning: Sofa (VA0000000001) in Living (not mounted on a radiator).",
        ),
        # A name the blueprint does not know goes out as freedo gives it.
        (
            ("valve_warning", "Living", "VA0000000002", ["newState"]),
            "Valve warning: VA0000000002 in Living (newState).",
        ),
        # A device in no room.
        (("device_offline", None, "RU0000000001", []), "Device offline: RU0000000001."),
        (("boiler_fault", None, None, ["291", "1110"]), "Boiler fault: codes 291, 1110."),
    ],
)
async def test_each_alert_sends_one_notification(hass, freezer, event, message):
    calls = await start(hass, freezer)
    await alert(hass, *event)
    assert sent(calls) == [([PHONE], "freedo", message)]


async def test_two_alerts_of_one_poll_send_two_notifications(hass, freezer):
    # The entity raises the alerts of one poll one second apart.
    calls = await start(hass, freezer)
    await alert(hass, "low_battery", "Living", "VA0000000001")
    freezer.tick(timedelta(seconds=1))
    await alert(hass, "device_offline", "Bedroom", "VA0000000004")
    assert messages(calls) == [
        "Low battery: VA0000000001 in Living.",
        "Device offline: VA0000000004 in Bedroom.",
    ]


async def test_only_the_chosen_alerts_are_sent(hass, freezer):
    calls = await start(hass, freezer, alerts=["boiler_fault"])
    await alert(hass, "low_battery", "Living", "VA0000000001")
    assert calls == []
    freezer.tick(timedelta(seconds=1))
    await alert(hass, "boiler_fault", details=["291"])
    assert messages(calls) == ["Boiler fault: codes 291."]


async def test_an_entity_that_comes_back_sends_nothing(hass, freezer):
    # A reload of the integration: the entity is unavailable, then it shows
    # its last event again.
    calls = await start(hass, freezer)
    await alert(hass, "low_battery", "Living", "VA0000000001")
    last = hass.states.get(ALERT)
    hass.states.async_set(ALERT, "unavailable")
    await flush(hass)
    hass.states.async_set(ALERT, last.state, last.attributes)
    await flush(hass)
    assert len(calls) == 1


async def test_quiet_time_holds_the_notification_until_its_end(hass, freezer):
    quiet = {"quiet_start": "22:00:00", "quiet_end": "07:00:00"}
    calls = await start(hass, freezer, time="23:30:00", **quiet)
    await alert(hass, "low_battery", "Living", "VA0000000001")
    freezer.tick(timedelta(seconds=1))
    await alert(hass, "boiler_fault", details=["291"])
    await settle(hass, freezer, hours=7)
    assert calls == []
    # 07:00 passes: both go out, in their order.
    await settle(hass, freezer, minutes=31)
    assert messages(calls) == ["Low battery: VA0000000001 in Living.", "Boiler fault: codes 291."]
    # Outside the quiet time a notification goes out at once.
    freezer.tick(timedelta(seconds=1))
    await alert(hass, "device_offline", "Living", "VA0000000001")
    assert len(calls) == 3


async def test_window_open_longer_than_the_time(hass, freezer):
    calls = await start(
        hass, freezer, window_sensors=[WINDOW], window_delay={"minutes": 20}
    )
    hass.states.async_set(WINDOW, "on", {"friendly_name": "Living Window"})
    await settle(hass, freezer, minutes=19)
    assert calls == []
    await settle(hass, freezer, minutes=2)
    assert messages(calls) == ["Window open for more than 20 minutes: Living Window."]
    # It stays open: no second notification.
    await settle(hass, freezer, hours=2)
    assert len(calls) == 1


async def test_a_window_closed_in_time_sends_nothing(hass, freezer):
    calls = await start(hass, freezer, window_sensors=[WINDOW])
    hass.states.async_set(WINDOW, "on", {"friendly_name": "Living Window"})
    await settle(hass, freezer, minutes=10)
    hass.states.async_set(WINDOW, "off", {"friendly_name": "Living Window"})
    await settle(hass, freezer, hours=1)
    assert calls == []


async def test_more_actions_get_the_title_and_the_message(hass, freezer):
    logged = async_mock_service(hass, "logbook", "log")
    extra = [{"action": "logbook.log", "data": {"name": "{{ title }}", "message": "{{ message }}"}}]
    calls = await start(hass, freezer, notify_targets=[], extra_actions=extra)
    await alert(hass, "boiler_fault", details=["291"])
    assert calls == []
    assert [(c.data["name"], c.data["message"]) for c in logged] == [
        ("freedo", "Boiler fault: codes 291.")
    ]


LIVING = "climate.living"
BEDROOM = "climate.bedroom"
OUTDOOR = "sensor.outdoor"


def room(hass, entity, name, inside, action="heating"):
    hass.states.async_set(
        entity, "heat", {"friendly_name": name, "current_temperature": inside, "hvac_action": action}
    )


def outdoor(hass, value):
    hass.states.async_set(OUTDOOR, value, {"unit_of_measurement": "°C"})


WARM = {"warm_rooms": [LIVING, BEDROOM], "warm_outdoor_sensor": OUTDOOR}


async def test_a_room_that_heats_while_it_is_warmer_outside(hass, freezer):
    room(hass, LIVING, "Living", 19.0)
    room(hass, BEDROOM, "Bedroom", 21.5)
    outdoor(hass, "19.5")
    calls = await start(hass, freezer, **WARM)
    # 19.5 outside is not 1 degree above any room.
    await settle(hass, freezer, hours=1)
    assert calls == []
    outdoor(hass, "20.5")
    await settle(hass, freezer, minutes=29)
    assert calls == []
    await settle(hass, freezer, minutes=2)
    assert messages(calls) == [
        "It is warmer outside (20.5°C) than in Living (19.0). The heating is on there"
        " and is not needed: switch it off or open a window."
    ]
    # It holds, and a second room joins: no other reminder.
    outdoor(hass, "23.0")
    await settle(hass, freezer, hours=2)
    assert len(calls) == 1
    # It ends, then starts again: a new reminder, for the rooms of that time.
    outdoor(hass, "10.0")
    await settle(hass, freezer, minutes=5)
    outdoor(hass, "23.0")
    await settle(hass, freezer, minutes=31)
    assert messages(calls)[1:] == [
        "It is warmer outside (23.0°C) than in Living (19.0), Bedroom (21.5). The heating is on"
        " there and is not needed: switch it off or open a window."
    ]


async def test_no_warm_reminder_for_a_room_that_does_not_heat(hass, freezer):
    room(hass, LIVING, "Living", 19.0, action="idle")
    room(hass, BEDROOM, "Bedroom", None)
    outdoor(hass, "25.0")
    calls = await start(hass, freezer, **WARM)
    await settle(hass, freezer, hours=2)
    # An outdoor sensor with no reading sends nothing either.
    room(hass, LIVING, "Living", 19.0)
    outdoor(hass, "unavailable")
    await settle(hass, freezer, hours=2)
    assert calls == []


async def test_no_warm_reminder_without_its_inputs(hass, freezer):
    # The default: no room, and an outdoor sensor that does not exist.
    room(hass, LIVING, "Living", 19.0)
    outdoor(hass, "25.0")
    calls = await start(hass, freezer)
    await settle(hass, freezer, hours=2)
    assert calls == []

