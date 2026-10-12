"""Behavior tests of the season profile blueprint, against the Home Assistant
automation engine.

They run only with pytest-homeassistant-custom-component installed. The tests
use a fake profile select and a fake outdoor sensor.
"""

import pathlib
import shutil
from datetime import timedelta

import pytest

pytest.importorskip("pytest_homeassistant_custom_component")

from homeassistant.const import EVENT_HOMEASSISTANT_STARTED  # noqa: E402
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
    / "season_profile.yaml"
)
# A new install names it select.freedo_profile: the blueprint takes any select.
PROFILE = "select.freedo_preset"
OUTDOOR = "sensor.outdoor"
OPTIONS = ["Winter", "Summer", "Holiday"]


@pytest.fixture(autouse=True)
async def detach_triggers(hass):
    """Turn the automation off at the end, so its daily time trigger leaves no
    timer behind."""
    yield
    if hass.services.has_service("automation", "turn_off"):
        await hass.services.async_call(
            "automation", "turn_off", {"entity_id": "all"}, blocking=True
        )


async def start(hass, freezer, day, active="Winter", **inputs):
    """Install the blueprint on day (YYYY-MM-DD, 12:00), create the automation
    and return the select calls."""
    freezer.move_to(f"{day} 12:00:00")
    dest = pathlib.Path(hass.config.path("blueprints/automation/freedo"))
    await hass.async_add_executor_job(
        lambda: (dest.mkdir(parents=True, exist_ok=True), shutil.copy(BLUEPRINT, dest))
    )
    hass.states.async_set(PROFILE, active, {"options": OPTIONS})
    calls = async_mock_service(hass, "select", "select_option")
    assert await async_setup_component(
        hass,
        "automation",
        {
            "automation": {
                "use_blueprint": {
                    "path": "freedo/season_profile.yaml",
                    "input": {"profile_select": PROFILE, **inputs},
                }
            }
        },
    )
    await hass.async_block_till_done()
    return calls


def chosen(calls):
    return [(c.data["entity_id"], c.data["option"]) for c in calls]


async def next_midnight(hass, freezer):
    now = dt_util.now()
    freezer.move_to((now + timedelta(days=1)).replace(hour=0, minute=1, second=0))
    async_fire_time_changed(hass)
    await flush(hass)


@pytest.mark.parametrize(
    ("day", "active", "want"),
    [
        ("2026-10-14", "Winter", "Summer"),
        ("2026-10-15", "Summer", "Winter"),
        ("2027-01-20", "Summer", "Winter"),
        ("2027-04-14", "Summer", "Winter"),
        ("2027-04-15", "Winter", "Summer"),
    ],
)
async def test_dates_choose_the_profile_at_start(hass, freezer, day, active, want):
    calls = await start(hass, freezer, day, active=active)
    hass.bus.async_fire(EVENT_HOMEASSISTANT_STARTED)
    await flush(hass)
    assert chosen(calls) == [([PROFILE], want)]


async def test_dates_change_the_profile_after_midnight(hass, freezer):
    calls = await start(hass, freezer, "2026-10-14", active="Summer")
    await next_midnight(hass, freezer)
    assert chosen(calls) == [([PROFILE], "Winter")]


async def test_no_call_when_the_profile_is_active(hass, freezer):
    calls = await start(hass, freezer, "2026-12-01", active="Winter")
    hass.bus.async_fire(EVENT_HOMEASSISTANT_STARTED)
    await next_midnight(hass, freezer)
    assert calls == []


async def test_no_call_for_a_profile_that_does_not_exist(hass, freezer):
    calls = await start(hass, freezer, "2026-12-01", active="Summer", winter_profile="Inverno")
    hass.bus.async_fire(EVENT_HOMEASSISTANT_STARTED)
    await flush(hass)
    assert calls == []


async def test_dates_in_the_same_year_order(hass, freezer):
    # Winter from 01-10 to 03-01 when the winter start comes first in the year.
    inputs = {"winter_start": "2026-01-10", "summer_start": "2026-03-01"}
    calls = await start(hass, freezer, "2026-02-01", active="Summer", **inputs)
    hass.bus.async_fire(EVENT_HOMEASSISTANT_STARTED)
    await flush(hass)
    assert chosen(calls) == [([PROFILE], "Winter")]


TEMPERATURE = {"method": "temperature", "outdoor_sensor": OUTDOOR}


async def settle(hass, freezer, **elapsed):
    freezer.tick(timedelta(**elapsed))
    async_fire_time_changed(hass)
    await flush(hass)


async def test_cold_weather_starts_the_winter_profile(hass, freezer):
    calls = await start(hass, freezer, "2026-09-20", active="Summer", **TEMPERATURE)
    hass.states.async_set(OUTDOOR, "16")
    await flush(hass)
    hass.states.async_set(OUTDOOR, "12")
    await settle(hass, freezer, hours=11)
    assert calls == []
    await settle(hass, freezer, hours=2)
    assert chosen(calls) == [([PROFILE], "Winter")]


async def test_a_warm_afternoon_does_not_end_the_winter(hass, freezer):
    calls = await start(hass, freezer, "2026-11-20", active="Winter", **TEMPERATURE)
    hass.states.async_set(OUTDOOR, "10")
    await flush(hass)
    hass.states.async_set(OUTDOOR, "19")
    await settle(hass, freezer, hours=4)
    hass.states.async_set(OUTDOOR, "11")
    await settle(hass, freezer, hours=12)
    assert calls == []


async def test_warm_weather_starts_the_summer_profile(hass, freezer):
    calls = await start(hass, freezer, "2027-05-01", active="Winter", **TEMPERATURE)
    hass.states.async_set(OUTDOOR, "15")
    await flush(hass)
    hass.states.async_set(OUTDOOR, "21")
    await settle(hass, freezer, hours=13)
    assert chosen(calls) == [([PROFILE], "Summer")]


async def test_temperature_method_ignores_the_dates(hass, freezer):
    calls = await start(hass, freezer, "2026-12-01", active="Summer", **TEMPERATURE)
    hass.bus.async_fire(EVENT_HOMEASSISTANT_STARTED)
    await next_midnight(hass, freezer)
    assert calls == []
