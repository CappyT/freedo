"""Behavior tests of the auxiliary heat blueprint, against the Home Assistant
automation engine.

They run only with pytest-homeassistant-custom-component installed. The tests
use a fake freedo room and a fake appliance whose climate services record the
calls and set its state, as an appliance that obeys does.
"""

import pathlib
import shutil
from datetime import timedelta

import pytest

pytest.importorskip("pytest_homeassistant_custom_component")

from homeassistant.const import EVENT_HOMEASSISTANT_STARTED  # noqa: E402
from homeassistant.loader import async_get_integration  # noqa: E402
from homeassistant.setup import async_setup_component  # noqa: E402
from homeassistant.util import dt as dt_util  # noqa: E402
from pytest_homeassistant_custom_component.common import async_fire_time_changed  # noqa: E402

from test_blueprint import flush, settle  # noqa: E402

BLUEPRINT = (
    pathlib.Path(__file__).resolve().parents[1]
    / "blueprints"
    / "automation"
    / "freedo"
    / "auxiliary_heat.yaml"
)
ROOM = "climate.living"
AC = "climate.living_ac"
START = [("mode", "heat"), ("temperature", 22)]


@pytest.fixture(autouse=True)
async def detach_triggers(hass):
    """Turn the automation off at the end, so its time trigger and a run that
    waits leave no timer behind."""
    yield
    if hass.services.has_service("automation", "turn_off"):
        await hass.services.async_call(
            "automation", "turn_off", {"entity_id": "all"}, blocking=True
        )


def room(hass, current, target=20.5, state="auto", **attrs):
    hass.states.async_set(
        ROOM,
        state,
        {
            "temperature": target,
            "current_temperature": current,
            "target_source": "program",
            "window_open": False,
            **attrs,
        },
    )


def appliance(hass, state, temperature=24):
    hass.states.async_set(AC, state, {"temperature": temperature})


async def start(hass, current=20.5, ac="off", obeys=True, **inputs):
    """Install the blueprint, create the automation and return the calls to the
    appliance as ("mode", m), ("temperature", t) or ("off",)."""
    dest = pathlib.Path(hass.config.path("blueprints/automation/freedo"))
    await hass.async_add_executor_job(
        lambda: (dest.mkdir(parents=True, exist_ok=True), shutil.copy(BLUEPRINT, dest))
    )
    room(hass, current)
    appliance(hass, ac)
    calls = []

    def service(service_name, record, apply):
        async def handle(call):
            # The scene that restores the appliance names the entity as a string.
            assert call.data["entity_id"] in (AC, [AC]), "a call to another entity"
            calls.append(record(call.data))
            if obeys:
                now = hass.states.get(AC)
                state, attrs = apply(call.data, now.state, dict(now.attributes))
                hass.states.async_set(AC, state, attrs)

        hass.services.async_register("climate", service_name, handle)

    service(
        "set_hvac_mode",
        lambda data: ("mode", data["hvac_mode"]),
        lambda data, state, attrs: (data["hvac_mode"], attrs),
    )
    service(
        "set_temperature",
        lambda data: ("temperature", data["temperature"]),
        lambda data, state, attrs: (state, {**attrs, "temperature": data["temperature"]}),
    )
    service("turn_off", lambda data: ("off",), lambda data, state, attrs: ("off", attrs))
    assert await async_setup_component(hass, "scene", {})
    # The scene that restores the appliance loads this platform. A first load
    # runs in the executor, and a test that moves the clock by hand does not
    # wait for it.
    await (await async_get_integration(hass, "climate")).async_get_platform("reproduce_state")
    assert await async_setup_component(
        hass,
        "automation",
        {
            "automation": {
                "use_blueprint": {
                    "path": "freedo/auxiliary_heat.yaml",
                    "input": {"room": ROOM, "appliance": AC, **inputs},
                }
            }
        },
    )
    await hass.async_block_till_done()
    return calls


async def cold(hass, current=18.0, **attrs):
    room(hass, current, **attrs)
    await flush(hass)


async def test_the_appliance_starts_at_the_start_gap(hass):
    calls = await start(hass)
    await cold(hass, 19.1)  # 1.4 below the target
    assert calls == []
    await cold(hass, 19.0)
    assert calls == START


async def test_the_appliance_returns_to_its_state_near_the_target(hass, freezer):
    calls = await start(hass)
    await cold(hass)
    await cold(hass, 20.1)
    assert calls == START
    await cold(hass, 20.2)  # 0.3 below the target
    assert calls[2:] == [("mode", "off"), ("temperature", 24)]
    assert hass.states.get(AC).state == "off"


async def test_the_state_before_the_run_can_be_a_running_one(hass):
    calls = await start(hass, ac="dry")
    await cold(hass)
    await cold(hass, 20.4)
    assert calls[2:] == [("mode", "dry"), ("temperature", 24)]


async def test_restore_off_turns_the_appliance_off(hass):
    calls = await start(hass, ac="dry", restore="off")
    await cold(hass)
    await cold(hass, 20.4)
    assert calls == [*START, ("off",)]


@pytest.mark.parametrize(
    "blocked",
    [
        {"state": "off"},
        {"state": "unavailable"},
        {"target_source": "away"},
        {"target_source": "window"},
        {"window_open": True},
        {"target": None},
        {"current": None},
    ],
)
async def test_no_start_and_a_stop_for_a_room_that_does_not_heat(hass, blocked):
    calls = await start(hass)
    current = blocked.pop("current", 18.0)
    await cold(hass, current, **blocked)
    assert calls == []
    await cold(hass)
    assert calls == START
    await cold(hass, current, **blocked)
    assert calls[2:] == [("mode", "off"), ("temperature", 24)]


async def test_no_start_for_an_appliance_that_is_unavailable(hass):
    calls = await start(hass, ac="unavailable")
    await cold(hass)
    assert calls == []


async def test_a_change_by_hand_ends_the_run_until_the_room_is_near(hass, freezer):
    calls = await start(hass)
    await cold(hass)
    appliance(hass, "off", 22)  # a person turns the appliance off
    await flush(hass)
    assert calls == START
    await cold(hass, 18.5)
    await settle(hass, freezer, hours=1)
    await cold(hass, 18.6)
    assert calls == START
    # The room comes near, the appliance rests, and a new gap starts a run.
    await cold(hass, 20.3)
    await settle(hass, freezer, minutes=11)
    await cold(hass, 18.0)
    assert calls[2:] == START


async def test_a_temperature_change_by_hand_ends_the_run(hass, freezer):
    calls = await start(hass)
    await cold(hass)
    appliance(hass, "heat", 25)
    await flush(hass)
    await cold(hass, 20.4)
    await settle(hass, freezer, hours=1)
    assert calls == START
    assert hass.states.get(AC).state == "heat"


async def test_the_maximum_run_time(hass, freezer):
    calls = await start(hass, max_run={"minutes": 30})
    await cold(hass)
    await settle(hass, freezer, minutes=29)
    assert calls == START
    await settle(hass, freezer, minutes=2)
    assert calls[2:] == [("mode", "off"), ("temperature", 24)]
    # No new run while the room is still far from the target.
    await settle(hass, freezer, hours=1)
    await cold(hass, 18.2)
    assert len(calls) == 4
    await cold(hass, 20.4)
    await settle(hass, freezer, minutes=11)
    await cold(hass, 18.0)
    assert calls[4:] == START


async def test_the_minimum_off_time(hass, freezer):
    calls = await start(hass)
    await cold(hass)
    await cold(hass, 20.4)
    assert len(calls) == 4
    await cold(hass, 18.0)
    await settle(hass, freezer, minutes=9)
    await cold(hass, 17.9)
    assert len(calls) == 4
    await settle(hass, freezer, minutes=2)
    await cold(hass, 17.8)
    assert calls[4:] == START


async def test_the_time_window(hass, freezer):
    freezer.move_to(dt_util.now().replace(hour=12, minute=0, second=0))
    calls = await start(hass, window_start="06:00:00", window_end="07:00:00")
    await cold(hass)
    assert calls == []
    # The gap is already there when the window opens.
    freezer.move_to((dt_util.now() + timedelta(days=1)).replace(hour=6, minute=0, second=1))
    async_fire_time_changed(hass)
    await flush(hass)
    assert calls == START
    # A run that started goes on after the window.
    freezer.move_to(dt_util.now().replace(hour=7, minute=30))
    async_fire_time_changed(hass)
    await flush(hass)
    assert calls == START
    await cold(hass, 20.4)
    assert len(calls) == 4


async def test_an_appliance_that_drops_out_keeps_the_run(hass, freezer):
    calls = await start(hass)
    await cold(hass)
    appliance(hass, "unavailable")
    await settle(hass, freezer, minutes=5)
    appliance(hass, "heat", 22)
    await settle(hass, freezer, minutes=5)
    assert calls == START
    await cold(hass, 20.4)
    assert calls[2:] == [("mode", "off"), ("temperature", 24)]


async def test_an_appliance_that_does_not_take_the_mode(hass, freezer):
    calls = await start(hass, obeys=False)
    await cold(hass)
    await settle(hass, freezer, minutes=1)
    assert calls == START
    await settle(hass, freezer, minutes=2)
    assert calls[2:] == [("mode", "off"), ("temperature", 24)]
    await settle(hass, freezer, minutes=9)
    await cold(hass, 17.9)
    assert len(calls) == 4
    await settle(hass, freezer, minutes=2)
    await cold(hass, 17.8)
    assert calls[4:] == START


async def test_start_takes_back_an_appliance_that_runs(hass):
    calls = await start(hass, current=18.0)
    appliance(hass, "heat", 22)
    hass.bus.async_fire(EVENT_HOMEASSISTANT_STARTED)
    await flush(hass)
    assert calls == []
    await cold(hass, 20.4)
    assert calls == [("off",)]


async def test_start_leaves_an_appliance_with_other_settings(hass):
    calls = await start(hass, current=20.4)
    appliance(hass, "heat", 25)
    hass.bus.async_fire(EVENT_HOMEASSISTANT_STARTED)
    await flush(hass)
    await cold(hass, 20.5)
    assert calls == []


async def test_inputs_set_the_gaps_and_the_appliance(hass):
    calls = await start(
        hass, start_gap=3, stop_gap=1, appliance_mode="heat_cool", appliance_temperature=20
    )
    await cold(hass, 17.6)
    assert calls == []
    await cold(hass, 17.5)
    assert calls == [("mode", "heat_cool"), ("temperature", 20)]
    await cold(hass, 19.4)
    assert len(calls) == 2
    await cold(hass, 19.5)
    assert len(calls) == 4
