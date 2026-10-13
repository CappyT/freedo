"""Behavior tests of the valve exercise blueprint, against the Home Assistant
automation engine and the freedo integration on its fake daemon: two valves,
their Calibrate motor buttons and their alert sensors.

They run only with pytest-homeassistant-custom-component installed.
"""

import pathlib
import shutil
from datetime import timedelta

import pytest

pytest.importorskip("pytest_homeassistant_custom_component")

from homeassistant.setup import async_setup_component  # noqa: E402
from pytest_homeassistant_custom_component.common import async_fire_time_changed  # noqa: E402

from test_blueprint import flush  # noqa: E402
from test_ha import (  # noqa: E402,F401
    auto_enable_custom_integrations,
    client,
    entity_id,
    health_device,
    pair_valve,
    refresh,
    setup_entry,
)

BLUEPRINT = (
    pathlib.Path(__file__).resolve().parents[1]
    / "blueprints"
    / "automation"
    / "freedo"
    / "valve_exercise.yaml"
)
# The valves by short address, in the order of their button entity ids: ffff
# is the valve Bed that joins the fixture, aaaa its valve Living.
FIRST = "ffff"
SECOND = "aaaa"


@pytest.fixture(autouse=True)
async def detach_triggers(hass):
    """Turn the automation off at the end, so its daily time trigger and a
    pause leave no timer behind."""
    yield
    if hass.services.has_service("automation", "turn_off"):
        await hass.services.async_call(
            "automation", "turn_off", {"entity_id": "all"}, blocking=True
        )


async def start(hass, freezer, client, moment, **inputs):
    """Set up freedo with two healthy valves and no room that heats, install
    the blueprint at moment (UTC) and return the config entry."""
    await hass.config.async_set_time_zone("UTC")
    freezer.move_to(moment)
    health_device(client, SECOND).update(lowBattery=False, commsWarnings=[], deviceWarnings=[])
    pair_valve(client, FIRST, "VA0000000009", "Bed", 2)
    health_device(client, FIRST).update(lowBattery=False, commsWarnings=[])
    client.nodes_json.append(dict(client.nodes_json[0], Short=FIRST))
    for view in client.zones_json["demand"]:
        view["DemandPct"] = 0
    entry = await setup_entry(hass, client)
    client.calls.clear()
    dest = pathlib.Path(hass.config.path("blueprints/automation/freedo"))
    await hass.async_add_executor_job(
        lambda: (dest.mkdir(parents=True, exist_ok=True), shutil.copy(BLUEPRINT, dest))
    )
    assert await async_setup_component(
        hass,
        "automation",
        {"automation": {"use_blueprint": {"path": "freedo/valve_exercise.yaml", "input": inputs}}},
    )
    await hass.async_block_till_done()
    assert [s.state for s in hass.states.async_all("automation")] == ["on"]
    return entry


def strokes(client):
    """The valves the daemon was asked to calibrate, in order."""
    return [call[1] for call in client.calls if call[0] == "calibrate"]


async def go(hass, freezer, moment=None, **elapsed):
    if moment is not None:
        freezer.move_to(moment)
    else:
        freezer.tick(timedelta(**elapsed))
    async_fire_time_changed(hass)
    # Not async_block_till_done: it would wait for the pause of a run.
    for _ in range(5):
        await flush(hass)


async def test_each_valve_moves_once_with_a_pause(hass, freezer, client):
    await start(hass, freezer, client, "2026-07-01 10:00:00")
    await go(hass, freezer, "2026-07-01 11:00:01")
    # Every Calibrate motor button of freedo, never an Identify button, one
    # valve at a time.
    assert strokes(client) == [FIRST]
    await go(hass, freezer, minutes=4)
    assert strokes(client) == [FIRST]
    await go(hass, freezer, minutes=2)
    assert strokes(client) == [FIRST, SECOND]
    assert not any(call[0] == "identify" for call in client.calls)
    # The other days of the month move nothing.
    for day in (2, 3, 15, 28):
        await go(hass, freezer, f"2026-07-{day:02d} 11:00:01")
        await go(hass, freezer, minutes=10)
    assert strokes(client) == [FIRST, SECOND]
    # The next month moves them again.
    await go(hass, freezer, "2026-08-01 11:00:01")
    assert strokes(client) == [FIRST, SECOND, FIRST]


async def test_not_before_the_day_of_the_month(hass, freezer, client):
    entry = await start(hass, freezer, client, "2026-07-01 10:00:00", day=10)
    button = entity_id(hass, entry, "button", f"{FIRST}_calibrate")
    await go(hass, freezer, "2026-07-09 11:00:01")
    assert strokes(client) == []
    await go(hass, freezer, "2026-07-10 11:00:01")
    await go(hass, freezer, minutes=6)
    assert strokes(client) == [FIRST, SECOND]
    # A press by hand before the day does not count for the month.
    await go(hass, freezer, "2026-08-05 12:00:00")
    await hass.services.async_call("button", "press", {"entity_id": button}, blocking=True)
    await go(hass, freezer, "2026-08-10 11:00:01")
    await go(hass, freezer, minutes=6)
    assert strokes(client)[2:] == [FIRST, FIRST, SECOND]


async def test_only_the_chosen_valves(hass, freezer, client):
    # The automation is created before the entities exist here, so the input
    # names the button by the id a new install gives it.
    await start(
        hass, freezer, client, "2026-07-01 10:00:00",
        buttons=["button.living_va0000000001_calibrate_motor"],
    )
    await go(hass, freezer, "2026-07-01 11:00:01")
    await go(hass, freezer, minutes=10)
    assert strokes(client) == [SECOND]


@pytest.mark.parametrize(
    "problem",
    [
        {"lowBattery": True},
        {"fault": True},
        {"deviceWarnings": ["notMounted"]},
        {"commsWarnings": ["noConfigAnswer"]},
        {"offline": True},
    ],
)
async def test_a_valve_with_a_problem_waits_for_another_day(hass, freezer, client, problem):
    entry = await start(hass, freezer, client, "2026-07-01 10:00:00")
    healthy = {key: health_device(client, FIRST).get(key) for key in problem}
    health_device(client, FIRST).update(problem)
    await refresh(hass, entry)
    await go(hass, freezer, "2026-07-01 11:00:01")
    await go(hass, freezer, minutes=6)
    assert strokes(client) == [SECOND]
    # The problem ends: the valve moves at the next run, and the other one
    # does not move again.
    health_device(client, FIRST).update(healthy)
    await refresh(hass, entry)
    await go(hass, freezer, "2026-07-02 11:00:01")
    await go(hass, freezer, minutes=6)
    assert strokes(client) == [SECOND, FIRST]


async def test_nothing_while_a_room_heats(hass, freezer, client):
    entry = await start(hass, freezer, client, "2026-07-01 10:00:00")
    client.zones_json["demand"][0]["DemandPct"] = 30
    await refresh(hass, entry)
    await go(hass, freezer, "2026-07-01 11:00:01")
    assert strokes(client) == []
    client.zones_json["demand"][0]["DemandPct"] = 0
    await refresh(hass, entry)
    await go(hass, freezer, "2026-07-02 11:00:01")
    assert strokes(client) == [FIRST]


async def test_while_a_room_heats_when_allowed(hass, freezer, client):
    entry = await start(hass, freezer, client, "2026-07-01 10:00:00", while_heating=True)
    client.zones_json["demand"][0]["DemandPct"] = 30
    await refresh(hass, entry)
    await go(hass, freezer, "2026-07-01 11:00:01")
    assert strokes(client) == [FIRST]


async def test_only_in_the_chosen_profiles(hass, freezer, client):
    # The fixture has the profiles Winter, the active one, and Summer.
    entry = await start(
        hass, freezer, client, "2026-07-01 10:00:00",
        profile_select="select.freedo_profile", profiles=["Summer"],
    )
    assert entity_id(hass, entry, "select", "preset") == "select.freedo_profile"
    await go(hass, freezer, "2026-07-01 11:00:01")
    assert strokes(client) == []
    await client.activate_preset(2)
    await refresh(hass, entry)
    await go(hass, freezer, "2026-07-02 11:00:01")
    assert strokes(client) == [FIRST]


async def test_a_restart_in_a_run_goes_on_the_next_day(hass, freezer, client):
    await start(hass, freezer, client, "2026-07-01 10:00:00")
    await go(hass, freezer, "2026-07-01 11:00:01")
    assert strokes(client) == [FIRST]
    # The restart ends the run in its pause.
    for service in ("turn_off", "turn_on"):
        await hass.services.async_call("automation", service, {"entity_id": "all"}, blocking=True)
    await go(hass, freezer, hours=3)
    assert strokes(client) == [FIRST]
    # The next day moves the valve that is left, and only that one.
    await go(hass, freezer, "2026-07-02 11:00:01")
    await go(hass, freezer, minutes=10)
    assert strokes(client) == [FIRST, SECOND]
