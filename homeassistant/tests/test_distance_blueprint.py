"""Behavior tests of the distance setback blueprint, against the Home Assistant
automation engine.

They run only with pytest-homeassistant-custom-component installed. The tests
use fake rooms, a fake distance sensor and a number service that records the
writes.
"""

import pathlib
import shutil

import pytest

pytest.importorskip("pytest_homeassistant_custom_component")

from homeassistant.const import EVENT_HOMEASSISTANT_STARTED  # noqa: E402
from homeassistant.helpers import (  # noqa: E402
    device_registry as dr,
    entity_registry as er,
    template,
)
from homeassistant.setup import async_setup_component  # noqa: E402
from pytest_homeassistant_custom_component.common import MockConfigEntry  # noqa: E402

from test_blueprint import flush, settle, track  # noqa: E402
from test_ha import (  # noqa: E402,F401
    auto_enable_custom_integrations,
    client,
    entity_id,
    setup_entry,
)

BLUEPRINT = (
    pathlib.Path(__file__).resolve().parents[1]
    / "blueprints"
    / "automation"
    / "freedo"
    / "distance_setback.yaml"
)
DISTANCE = "sensor.home_nearest_distance"
LIVING = "climate.living"
BEDROOM = "climate.bedroom"
LIVING_AWAY = "number.living_away"
BEDROOM_AWAY = "number.bedroom_away"
ALICE = "person.alice"
GUEST = "input_boolean.guest_mode"
NO_WAIT = {"min_interval": {"seconds": 0}}


def room(hass, entry, name, program, away):
    """A room device with its climate, its number Away and a number of another
    kind, as the integration creates them."""
    device = dr.async_get(hass).async_get_or_create(
        config_entry_id=entry.entry_id, identifiers={("test", name)}
    )
    registry = er.async_get(hass)
    for domain, key in (("climate", ""), ("number", "_window_time"), ("number", "_away")):
        registry.async_get_or_create(
            domain, "test", f"{name}{key}", device_id=device.id, suggested_object_id=f"{name}{key}"
        )
    hass.states.async_set(f"number.{name}_window_time", "15", {"device_class": "duration"})
    hass.states.async_set(f"number.{name}_away", str(away), {"device_class": "temperature"})
    program_is(hass, f"climate.{name}", program)


def program_is(hass, climate, program):
    hass.states.async_set(climate, "auto", {"program_temperature": program})


def distance(hass, value, unit="m"):
    hass.states.async_set(
        DISTANCE, str(value), {"device_class": "distance", "unit_of_measurement": unit}
    )


async def start(hass, meters=0, living=(21, 21), bedroom=(18, 18), **inputs):
    """Install the blueprint, create the automation and return the writes as
    (number, value). living and bedroom are (program, number Away)."""
    dest = pathlib.Path(hass.config.path("blueprints/automation/freedo"))
    await hass.async_add_executor_job(
        lambda: (dest.mkdir(parents=True, exist_ok=True), shutil.copy(BLUEPRINT, dest))
    )
    entry = MockConfigEntry(domain="test")
    entry.add_to_hass(hass)
    room(hass, entry, "living", *living)
    room(hass, entry, "bedroom", *bedroom)
    distance(hass, meters)
    track(hass, ALICE, "home")
    hass.states.async_set(GUEST, "off")
    writes = []

    async def set_value(call):
        for number in call.data["entity_id"]:
            writes.append((number, call.data["value"]))
            hass.states.async_set(
                number, str(call.data["value"]), {"device_class": "temperature"}
            )

    hass.services.async_register("number", "set_value", set_value)
    assert await async_setup_component(
        hass,
        "automation",
        {
            "automation": {
                "use_blueprint": {
                    "path": "freedo/distance_setback.yaml",
                    "input": {
                        "rooms": [LIVING, BEDROOM],
                        "distance_sensor": DISTANCE,
                        **inputs,
                    },
                }
            }
        },
    )
    await hass.async_block_till_done()
    return writes


async def step(hass, freezer):
    """Let a run start, then pass the pause that follows its writes."""
    await flush(hass)
    await settle(hass, freezer, seconds=2)


async def move(hass, freezer, meters):
    distance(hass, meters)
    await step(hass, freezer)


async def test_each_distance_step_lowers_the_rooms(hass, freezer):
    writes = await start(hass, **NO_WAIT)
    await move(hass, freezer, 1500)  # inside the comfort distance of 2 km
    assert writes == []
    await move(hass, freezer, 5000)
    assert writes == [(LIVING_AWAY, 20.0), (BEDROOM_AWAY, 17.0)]
    await move(hass, freezer, 13000)  # one distance step of 10 km beyond the comfort distance
    assert writes[2:] == [(LIVING_AWAY, 19.0), (BEDROOM_AWAY, 16.0)]
    await move(hass, freezer, 23000)
    assert writes[4:] == [(LIVING_AWAY, 18.0)]  # the bedroom is at the lowest temperature
    await move(hass, freezer, 300000)
    assert writes[5:] == [(LIVING_AWAY, 16.0)]
    await move(hass, freezer, 900000)
    assert len(writes) == 6


async def test_an_approach_raises_the_rooms(hass, freezer):
    writes = await start(hass, meters=300000, living=(21, 16), bedroom=(18, 16), **NO_WAIT)
    await move(hass, freezer, 15000)
    assert writes == [(LIVING_AWAY, 19.0)]
    await move(hass, freezer, 8000)
    assert writes[1:] == [(LIVING_AWAY, 20.0), (BEDROOM_AWAY, 17.0)]
    await move(hass, freezer, 900)
    assert writes[3:] == [(LIVING_AWAY, 21.0), (BEDROOM_AWAY, 18.0)]


async def test_a_position_around_a_boundary_changes_nothing(hass, freezer):
    writes = await start(hass, meters=11000, living=(21, 20), bedroom=(18, 17), **NO_WAIT)
    await move(hass, freezer, 12100)  # the boundary is at 12 km
    assert writes == [(LIVING_AWAY, 19.0), (BEDROOM_AWAY, 16.0)]
    for meters in (11900, 12300, 10100, 12050, 11000):
        await move(hass, freezer, meters)
    assert len(writes) == 2
    await move(hass, freezer, 9900)  # 2 km of hysteresis below the boundary
    assert writes[2:] == [(LIVING_AWAY, 20.0), (BEDROOM_AWAY, 17.0)]
    for meters in (11900, 10100, 11000):
        await move(hass, freezer, meters)
    assert len(writes) == 4


async def test_the_comfort_boundary_has_half_its_distance_as_hysteresis(hass, freezer):
    writes = await start(hass, meters=5000, living=(21, 20), bedroom=(18, 17), **NO_WAIT)
    await move(hass, freezer, 1500)
    assert writes == []
    await move(hass, freezer, 1000)
    assert writes == [(LIVING_AWAY, 21.0), (BEDROOM_AWAY, 18.0)]


async def test_home_gives_the_program_with_no_comfort_distance(hass, freezer):
    writes = await start(
        hass, meters=400, living=(21, 20), bedroom=(18, 17), comfort_distance=0, **NO_WAIT
    )
    await move(hass, freezer, 300)
    assert writes == []
    await move(hass, freezer, 0)  # Proximity reports 0 inside the home zone
    assert writes == [(LIVING_AWAY, 21.0), (BEDROOM_AWAY, 18.0)]
    await move(hass, freezer, 100)
    assert writes[2:] == [(LIVING_AWAY, 20.0), (BEDROOM_AWAY, 17.0)]


async def test_a_room_never_goes_above_its_program(hass, freezer):
    # The night block of the program is below the lowest temperature.
    writes = await start(hass, living=(15, 15), bedroom=(16.5, 16.5), **NO_WAIT)
    await move(hass, freezer, 300000)
    assert writes == [(BEDROOM_AWAY, 16.0)]
    program_is(hass, LIVING, 14)
    await step(hass, freezer)
    assert writes[1:] == [(LIVING_AWAY, 14.0)]


async def test_a_program_block_moves_the_room(hass, freezer):
    writes = await start(hass, meters=5000, living=(21, 20), bedroom=(18, 17), **NO_WAIT)
    program_is(hass, LIVING, 18)
    await step(hass, freezer)
    assert writes == [(LIVING_AWAY, 17.0)]
    program_is(hass, LIVING, 21)
    await step(hass, freezer)
    assert writes[1:] == [(LIVING_AWAY, 20.0)]
    # Another attribute of the room starts no run.
    hass.states.async_set(LIVING, "auto", {"program_temperature": 21, "demand_pct": 40})
    await step(hass, freezer)
    assert len(writes) == 2


async def test_inputs_set_the_steps(hass, freezer):
    writes = await start(
        hass,
        lowest_temperature=12,
        comfort_distance=1,
        step_distance=5,
        step_temperature=0.5,
        hysteresis=0,
        **NO_WAIT,
    )
    await move(hass, freezer, 16100)  # 1 + 3 distance steps
    assert writes == [(LIVING_AWAY, 19.0), (BEDROOM_AWAY, 16.0)]
    await move(hass, freezer, 15900)
    assert writes[2:] == [(LIVING_AWAY, 19.5), (BEDROOM_AWAY, 16.5)]


async def test_the_unit_of_the_sensor(hass, freezer):
    writes = await start(hass, **NO_WAIT)
    distance(hass, 13, "km")
    await step(hass, freezer)
    assert writes == [(LIVING_AWAY, 19.0), (BEDROOM_AWAY, 16.0)]
    distance(hass, 4, "mi")  # 6.4 km
    await step(hass, freezer)
    assert writes[2:] == [(LIVING_AWAY, 20.0), (BEDROOM_AWAY, 17.0)]


@pytest.mark.parametrize("state", ["unknown", "unavailable"])
async def test_an_unknown_distance_gives_the_program(hass, freezer, state):
    writes = await start(hass, meters=300000, living=(21, 16), bedroom=(18, 16), **NO_WAIT)
    hass.states.async_set(DISTANCE, state)
    await step(hass, freezer)
    assert writes == [(LIVING_AWAY, 21.0), (BEDROOM_AWAY, 18.0)]
    await move(hass, freezer, 300000)
    assert writes[2:] == [(LIVING_AWAY, 16.0), (BEDROOM_AWAY, 16.0)]


async def test_a_unit_that_is_no_length_gives_the_program(hass, freezer):
    writes = await start(hass, meters=5000, living=(21, 20), bedroom=(18, 17), **NO_WAIT)
    distance(hass, 300000, "s")
    await step(hass, freezer)
    assert writes == [(LIVING_AWAY, 21.0), (BEDROOM_AWAY, 18.0)]


@pytest.mark.parametrize("state", ["unknown", "unavailable"])
async def test_an_unavailable_person_gives_the_program(hass, freezer, state):
    writes = await start(
        hass, meters=300000, living=(21, 16), bedroom=(18, 16), people=[ALICE], **NO_WAIT
    )
    hass.states.async_set(ALICE, state)
    await step(hass, freezer)
    assert writes == [(LIVING_AWAY, 21.0), (BEDROOM_AWAY, 18.0)]
    track(hass, ALICE, "away")
    await step(hass, freezer)
    assert writes[2:] == [(LIVING_AWAY, 16.0), (BEDROOM_AWAY, 16.0)]


async def test_a_blocking_entity_gives_the_program(hass, freezer):
    writes = await start(
        hass, meters=300000, living=(21, 16), bedroom=(18, 16), block_entities=[GUEST], **NO_WAIT
    )
    hass.states.async_set(GUEST, "on")
    await step(hass, freezer)
    assert writes == [(LIVING_AWAY, 21.0), (BEDROOM_AWAY, 18.0)]
    await move(hass, freezer, 250000)
    assert len(writes) == 2
    hass.states.async_set(GUEST, "off")
    await step(hass, freezer)
    assert writes[2:] == [(LIVING_AWAY, 16.0), (BEDROOM_AWAY, 16.0)]


async def test_start_applies_the_distance(hass, freezer):
    writes = await start(hass, meters=13000, **NO_WAIT)
    assert writes == []
    hass.bus.async_fire(EVENT_HOMEASSISTANT_STARTED)
    await step(hass, freezer)
    assert writes == [(LIVING_AWAY, 19.0), (BEDROOM_AWAY, 16.0)]


async def test_a_room_changes_once_in_the_minimum_interval(hass, freezer):
    writes = await start(hass)
    await move(hass, freezer, 5000)
    assert writes == [(LIVING_AWAY, 20.0), (BEDROOM_AWAY, 17.0)]
    # Two more steps arrive inside the interval of 5 minutes.
    await move(hass, freezer, 13000)
    await settle(hass, freezer, minutes=2)
    await move(hass, freezer, 23000)
    await settle(hass, freezer, minutes=2)
    assert len(writes) == 2
    await settle(hass, freezer, minutes=2)
    assert writes[2:] == [(LIVING_AWAY, 18.0), (BEDROOM_AWAY, 16.0)]
    # Nothing is left to apply: the run ends after one more interval.
    await settle(hass, freezer, minutes=6)
    await move(hass, freezer, 900)
    assert writes[4:] == [(LIVING_AWAY, 21.0), (BEDROOM_AWAY, 18.0)]


async def test_a_step_that_returns_inside_the_interval_writes_nothing(hass, freezer):
    writes = await start(hass)
    await move(hass, freezer, 5000)
    await move(hass, freezer, 13000)
    await settle(hass, freezer, minutes=2)
    await move(hass, freezer, 5000)
    await settle(hass, freezer, minutes=10)
    assert writes == [(LIVING_AWAY, 20.0), (BEDROOM_AWAY, 17.0)]


async def test_a_room_with_no_value_is_left_alone(hass, freezer):
    writes = await start(hass, **NO_WAIT)
    hass.states.async_set(LIVING_AWAY, "unavailable", {"device_class": "temperature"})
    hass.states.async_set(BEDROOM, "auto", {"program_temperature": None})
    await move(hass, freezer, 13000)
    assert writes == []


async def test_a_round_trip_costs_two_writes_for_each_step(hass, freezer):
    # The bound on the writes: out and back over three steps, with a position
    # report every 500 m.
    writes = await start(hass, lowest_temperature=5, **NO_WAIT)
    out = list(range(0, 30001, 500))
    for meters in out + out[::-1]:
        await move(hass, freezer, meters)
    assert [value for number, value in writes if number == LIVING_AWAY] == [
        20.0, 19.0, 18.0, 19.0, 20.0, 21.0,
    ]
    assert len(writes) == 12


async def test_the_number_away_of_a_freedo_room(hass, client):
    """The lookup the blueprint uses, against the entities of the integration:
    the number of the room device with the device class temperature."""
    entry = await setup_entry(hass, client)
    found = template.Template(
        "{{ device_entities(device_id(room)) | select('match', '^number[.]')"
        " | select('is_state_attr', 'device_class', 'temperature') | list }}",
        hass,
    )
    for zone in (1, 2):
        climate = entity_id(hass, entry, "climate", f"zone{zone}_climate")
        away = entity_id(hass, entry, "number", f"zone{zone}_away_temperature_c")
        assert found.async_render({"room": climate}) == [away]
        assert "program_temperature" in hass.states.get(climate).attributes
