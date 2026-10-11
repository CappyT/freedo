"""Behavior tests of the presence blueprint, against the Home Assistant
automation engine.

They run only with pytest-homeassistant-custom-component installed. The tests
use fake states for the people and a mocked Away switch.
"""

import asyncio
import pathlib
import shutil
from datetime import timedelta

import pytest

pytest.importorskip("pytest_homeassistant_custom_component")

from homeassistant.const import EVENT_HOMEASSISTANT_STARTED  # noqa: E402
from homeassistant.setup import async_setup_component  # noqa: E402
from pytest_homeassistant_custom_component.common import (  # noqa: E402
    async_fire_time_changed,
    async_mock_service,
)

BLUEPRINT = (
    pathlib.Path(__file__).resolve().parents[1]
    / "blueprints"
    / "automation"
    / "freedo"
    / "presence_away.yaml"
)
AWAY = "switch.freedo_away"
ALICE = "person.alice"
BOB = "device_tracker.bob"
GUEST = "input_boolean.guest_mode"
APPROACH = {"approach_zone": "zone.approach"}


def track(hass, entity_id, where, **attrs):
    """Set a person state: 'home', 'away', a zone object id or 'unavailable'."""
    if where == "unavailable":
        hass.states.async_set(entity_id, "unavailable")
    elif where == "away":
        hass.states.async_set(entity_id, "not_home", {"in_zones": [], **attrs})
    else:
        hass.states.async_set(entity_id, where, {"in_zones": [f"zone.{where}"], **attrs})


def nested_home(hass, entity_id):
    """A person at home when the approach zone contains the home: in_zones
    lists every zone that holds the person, the smallest first."""
    track(hass, entity_id, "home", in_zones=["zone.home", "zone.approach"])


async def flush(hass):
    """Let the pending callbacks run. async_block_till_done would wait for a
    script that sleeps in a delay, so it does not fit here."""
    for _ in range(20):
        await asyncio.sleep(0)


async def settle(hass, freezer, **elapsed):
    freezer.tick(timedelta(**elapsed))
    async_fire_time_changed(hass)
    await flush(hass)


async def start(hass, away="off", alice="home", bob="home", **inputs):
    """Install the blueprint, create the automation, return the service calls."""
    dest = pathlib.Path(hass.config.path("blueprints/automation/freedo"))
    await hass.async_add_executor_job(
        lambda: (dest.mkdir(parents=True, exist_ok=True), shutil.copy(BLUEPRINT, dest))
    )
    hass.states.async_set(AWAY, away)
    hass.states.async_set(GUEST, "off")
    track(hass, ALICE, alice)
    track(hass, BOB, bob)
    turn_on = async_mock_service(hass, "switch", "turn_on")
    turn_off = async_mock_service(hass, "switch", "turn_off")
    assert await async_setup_component(
        hass,
        "automation",
        {
            "automation": {
                "use_blueprint": {
                    "path": "freedo/presence_away.yaml",
                    "input": {"away_switch": AWAY, "people": [ALICE, BOB], **inputs},
                }
            }
        },
    )
    await hass.async_block_till_done()
    return turn_on, turn_off


async def test_all_away_turns_on_after_the_delay(hass, freezer):
    turn_on, turn_off = await start(hass)
    track(hass, ALICE, "away")
    track(hass, BOB, "away")
    await settle(hass, freezer, minutes=9)
    assert not turn_on
    await settle(hass, freezer, minutes=2)
    assert [c.data["entity_id"] for c in turn_on] == [[AWAY]]
    assert not turn_off


async def test_one_person_home_keeps_the_home(hass, freezer):
    turn_on, _ = await start(hass)
    track(hass, ALICE, "away")
    await settle(hass, freezer, minutes=30)
    assert not turn_on


async def test_return_cancels_the_pending_away(hass, freezer):
    turn_on, _ = await start(hass)
    track(hass, ALICE, "away")
    track(hass, BOB, "away")
    await settle(hass, freezer, minutes=5)
    track(hass, BOB, "home")
    await settle(hass, freezer, minutes=30)
    assert not turn_on


async def test_gps_update_does_not_restart_the_delay(hass, freezer):
    turn_on, _ = await start(hass)
    track(hass, ALICE, "away")
    track(hass, BOB, "away")
    await settle(hass, freezer, minutes=9)
    track(hass, ALICE, "away", latitude=1.0)
    await settle(hass, freezer, minutes=2)
    assert len(turn_on) == 1


async def test_no_call_when_the_switch_has_the_state(hass, freezer):
    turn_on, turn_off = await start(hass, away="on", alice="away", bob="away")
    track(hass, BOB, "work")  # from one zone outside the home to another
    await settle(hass, freezer, minutes=30)
    assert not turn_on and not turn_off
    hass.states.async_set(AWAY, "off")
    track(hass, ALICE, "home")
    await flush(hass)
    assert not turn_off


async def test_unavailable_tracker_never_turns_away_on(hass, freezer):
    turn_on, _ = await start(hass, alice="away")
    track(hass, BOB, "unavailable")
    await settle(hass, freezer, minutes=30)
    track(hass, ALICE, "unavailable")
    await settle(hass, freezer, minutes=30)
    assert not turn_on


async def test_tracker_that_drops_out_restarts_the_delay(hass, freezer):
    turn_on, _ = await start(hass)
    track(hass, ALICE, "away")
    track(hass, BOB, "away")
    await settle(hass, freezer, minutes=5)
    track(hass, BOB, "unavailable")
    await settle(hass, freezer, minutes=30)
    assert not turn_on
    track(hass, BOB, "away")
    await settle(hass, freezer, minutes=9)
    assert not turn_on
    await settle(hass, freezer, minutes=2)
    assert len(turn_on) == 1


async def test_home_turns_away_off_at_once(hass):
    _, turn_off = await start(hass, away="on", alice="away", bob="away")
    track(hass, BOB, "home")
    await flush(hass)
    assert [c.data["entity_id"] for c in turn_off] == [[AWAY]]


async def test_unavailable_tracker_does_not_stop_the_return(hass):
    _, turn_off = await start(hass, away="on", alice="away", bob="unavailable")
    track(hass, ALICE, "home")
    await flush(hass)
    assert len(turn_off) == 1


async def test_home_delay(hass, freezer):
    _, turn_off = await start(
        hass, away="on", alice="away", bob="away", home_delay={"minutes": 2}
    )
    track(hass, ALICE, "home")
    await settle(hass, freezer, minutes=1)
    assert not turn_off
    await settle(hass, freezer, minutes=2)
    assert len(turn_off) == 1


async def test_home_zone_input(hass, freezer):
    turn_on, turn_off = await start(
        hass, away="on", alice="away", bob="away", home_zone="zone.base"
    )
    track(hass, ALICE, "home")  # in zone.home, not in zone.base
    await settle(hass, freezer, minutes=1)
    assert not turn_off
    track(hass, ALICE, "base")
    await flush(hass)
    assert len(turn_off) == 1


async def test_blocking_entity(hass, freezer):
    turn_on, _ = await start(hass, block_entities=[GUEST])
    hass.states.async_set(GUEST, "on")
    track(hass, ALICE, "away")
    track(hass, BOB, "away")
    await settle(hass, freezer, minutes=30)
    assert not turn_on
    # Switching the blocker off applies the presence again.
    hass.states.async_set(GUEST, "off")
    await settle(hass, freezer, minutes=11)
    assert len(turn_on) == 1


async def test_blocker_on_cancels_the_pending_away(hass, freezer):
    turn_on, _ = await start(hass, block_entities=[GUEST])
    track(hass, ALICE, "away")
    track(hass, BOB, "away")
    await settle(hass, freezer, minutes=5)
    hass.states.async_set(GUEST, "on")
    await settle(hass, freezer, minutes=30)
    assert not turn_on


async def test_start_applies_the_presence(hass, freezer):
    turn_on, _ = await start(hass, alice="away", bob="away")
    hass.bus.async_fire(EVENT_HOMEASSISTANT_STARTED)
    await settle(hass, freezer, minutes=11)
    assert len(turn_on) == 1


async def test_start_turns_away_off(hass):
    _, turn_off = await start(hass, away="on", alice="home", bob="away")
    hass.bus.async_fire(EVENT_HOMEASSISTANT_STARTED)
    await flush(hass)
    assert len(turn_off) == 1


async def test_in_zones_lists_the_overlapping_zones(hass):
    """The fact the approach zone logic relies on, from the zone integration."""
    from homeassistant.components.zone import async_in_zones

    assert await async_setup_component(
        hass,
        "zone",
        {
            "zone": [
                {
                    "name": "approach",
                    "latitude": hass.config.latitude,
                    "longitude": hass.config.longitude,
                    "radius": 2000,
                }
            ]
        },
    )
    await hass.async_block_till_done()
    lat, lon = hass.config.latitude, hass.config.longitude
    active, zones = async_in_zones(hass, lat, lon)
    assert active.entity_id == "zone.home"
    assert zones == ["zone.home", "zone.approach"]
    active, zones = async_in_zones(hass, lat + 0.009, lon)  # about 1 km north
    assert active.entity_id == "zone.approach"
    assert zones == ["zone.approach"]


async def test_approach_zone_turns_away_off_at_once(hass):
    _, turn_off = await start(hass, away="on", alice="away", bob="away", **APPROACH)
    track(hass, ALICE, "approach")
    await flush(hass)
    assert [c.data["entity_id"] for c in turn_off] == [[AWAY]]


async def test_approach_zone_ignores_the_home_delay(hass):
    _, turn_off = await start(
        hass, away="on", alice="away", bob="away", home_delay={"minutes": 2}, **APPROACH
    )
    track(hass, ALICE, "approach")
    await flush(hass)
    assert len(turn_off) == 1


async def test_home_delay_still_applies_inside_the_approach_zone(hass, freezer):
    _, turn_off = await start(
        hass, away="on", alice="away", bob="away", home_delay={"minutes": 2}, **APPROACH
    )
    nested_home(hass, ALICE)
    await settle(hass, freezer, minutes=1)
    assert not turn_off
    await settle(hass, freezer, minutes=2)
    assert len(turn_off) == 1


async def test_the_home_zone_counts_when_a_smaller_zone_is_the_state(hass, freezer):
    # The state names the smallest zone only. The membership decides.
    _, turn_off = await start(
        hass, away="on", alice="away", bob="away", home_delay={"minutes": 2}, **APPROACH
    )
    track(hass, ALICE, "approach", in_zones=["zone.approach", "zone.home"])
    await settle(hass, freezer, minutes=1)
    assert not turn_off
    await settle(hass, freezer, minutes=2)
    assert len(turn_off) == 1


async def test_leaving_the_approach_zone_turns_away_on_again(hass, freezer):
    turn_on, turn_off = await start(hass, away="on", alice="away", bob="away", **APPROACH)
    track(hass, ALICE, "approach")
    await flush(hass)
    assert len(turn_off) == 1
    hass.states.async_set(AWAY, "off")
    track(hass, ALICE, "away")
    await settle(hass, freezer, minutes=9)
    assert not turn_on
    await settle(hass, freezer, minutes=2)
    assert len(turn_on) == 1


async def test_reentering_the_approach_zone_cancels_the_pending_away(hass, freezer):
    turn_on, _ = await start(hass, alice="approach", bob="away", **APPROACH)
    track(hass, ALICE, "away")
    await settle(hass, freezer, minutes=5)
    track(hass, ALICE, "approach")
    await settle(hass, freezer, minutes=30)
    assert not turn_on


async def test_leaving_home_inside_the_approach_zone_keeps_the_home(hass, freezer):
    turn_on, _ = await start(hass, alice="home", bob="away", **APPROACH)
    track(hass, ALICE, "approach")
    await settle(hass, freezer, minutes=30)
    assert not turn_on
    track(hass, ALICE, "away")
    await settle(hass, freezer, minutes=9)
    assert not turn_on
    await settle(hass, freezer, minutes=2)
    assert len(turn_on) == 1


async def test_a_person_in_the_approach_zone_blocks_away(hass, freezer):
    turn_on, _ = await start(hass, alice="approach", bob="home", **APPROACH)
    track(hass, BOB, "away")
    await settle(hass, freezer, minutes=30)
    assert not turn_on


async def test_approach_zone_with_an_unavailable_tracker(hass, freezer):
    turn_on, _ = await start(hass, alice="away", **APPROACH)
    track(hass, BOB, "unavailable")
    await settle(hass, freezer, minutes=30)
    assert not turn_on


async def test_unavailable_tracker_does_not_stop_the_approach(hass):
    _, turn_off = await start(
        hass, away="on", alice="away", bob="unavailable", **APPROACH
    )
    track(hass, ALICE, "approach")
    await flush(hass)
    assert len(turn_off) == 1


async def test_approach_zone_with_a_blocking_entity(hass):
    _, turn_off = await start(
        hass, away="on", alice="away", bob="away", block_entities=[GUEST], **APPROACH
    )
    hass.states.async_set(GUEST, "on")
    track(hass, ALICE, "approach")
    await flush(hass)
    assert not turn_off
    # Switching the blocker off applies the presence again.
    hass.states.async_set(GUEST, "off")
    await flush(hass)
    assert len(turn_off) == 1


async def test_start_applies_the_approach_zone(hass):
    _, turn_off = await start(hass, away="on", alice="approach", bob="away", **APPROACH)
    hass.bus.async_fire(EVENT_HOMEASSISTANT_STARTED)
    await flush(hass)
    assert len(turn_off) == 1


async def test_without_an_approach_zone_the_approach_is_outside_the_home(hass, freezer):
    turn_on, turn_off = await start(hass, alice="approach", bob="home")
    track(hass, BOB, "approach")
    await settle(hass, freezer, minutes=11)
    assert len(turn_on) == 1
    assert not turn_off


async def test_without_an_approach_zone_the_approach_does_not_turn_away_off(hass):
    _, turn_off = await start(hass, away="on", alice="away", bob="away")
    track(hass, ALICE, "approach")
    await flush(hass)
    assert not turn_off
