"""The examples of the Reports section of the integration README, against the
schemas of the installed Home Assistant and the entity names of the integration.

They run only with pytest-homeassistant-custom-component installed.
"""

import pathlib
import re
from unittest.mock import patch

import pytest

pytest.importorskip("pytest_homeassistant_custom_component")

from homeassistant.components.history_stats.helpers import async_calculate_period  # noqa: E402
from homeassistant.helpers import entity_registry as er  # noqa: E402
from homeassistant.helpers.check_config import async_check_ha_config_file  # noqa: E402
from homeassistant.setup import async_setup_component  # noqa: E402
from homeassistant.util import dt as dt_util, slugify  # noqa: E402
from homeassistant.util.yaml import parse_yaml  # noqa: E402

from test_ha import (  # noqa: E402,F401
    auto_enable_custom_integrations,
    client,
    entity_id,
    setup_entry,
    with_gas,
)

README = pathlib.Path(__file__).resolve().parents[1] / "custom_components" / "freedo" / "README.md"
# The zone Living and the device Living (VA0000000001) of the test fixtures.
PLACEHOLDERS = {"<zone>": "living", "<device>": "living_va0000000001"}
# The options of the statistics graph card in the dashboard documentation.
CARD_KEYS = {
    "type", "entities", "title", "days_to_show", "chart_type", "stat_types", "period",
    "hide_legend", "logarithmic_scale", "min_y_axis", "max_y_axis", "fit_y_data",
    "expand_legend", "energy_date_selection", "collection_key",
}
CARD_STATS = {"min", "max", "mean", "sum", "state", "change"}
CARD_PERIODS = {"5minute", "hour", "day", "week", "month", "year"}


def examples() -> list[dict]:
    """The YAML blocks of the section Reports, placeholders filled in."""
    text = README.read_text()
    section = re.search(r"^## Reports\n(.*?)(?=^## )", text, re.S | re.M)
    assert section, "the README has no section Reports"
    blocks = re.findall(r"^```yaml\n(.*?)^```", section.group(1), re.S | re.M)
    for placeholder, value in PLACEHOLDERS.items():
        blocks = [block.replace(placeholder, value) for block in blocks]
    return [{"text": block, "data": parse_yaml(block)} for block in blocks]


def history_stats_entries() -> list[dict]:
    return [
        entry
        for ex in examples()
        for entry in ex["data"].get("sensor", [])
        if entry["platform"] == "history_stats"
    ]


def utility_meters() -> dict[str, dict]:
    return {k: v for ex in examples() for k, v in ex["data"].get("utility_meter", {}).items()}


def cards() -> list[dict]:
    return [ex["data"] for ex in examples() if ex["data"].get("type") == "statistics-graph"]


def test_the_section_holds_every_kind_of_example():
    assert history_stats_entries()
    assert {m["cycle"] for m in utility_meters().values()} == {"daily", "monthly"}
    assert {e.split(".")[1].rsplit("_", 1)[1] for c in cards() for e in c["entities"]} == {
        "temperature",
        "humidity",
    }


async def test_the_configuration_examples_pass_the_config_check(hass, tmp_path):
    hass.config.config_dir = str(tmp_path)
    for ex in examples():
        if ex["data"].get("type") == "statistics-graph":
            continue
        (tmp_path / "configuration.yaml").write_text(ex["text"])
        result = await async_check_ha_config_file(hass)
        assert not result.errors and not result.warnings, ex["text"]
        # An entry that fails its schema is dropped from the result.
        for domain, expected in ex["data"].items():
            assert len(result[domain]) == len(expected), ex["text"]


async def test_the_history_stats_entries_count_on_for_today(hass, tmp_path):
    hass.config.config_dir = str(tmp_path)
    (tmp_path / "configuration.yaml").write_text(
        "".join(ex["text"] for ex in examples() if "history_stats" in ex["text"])
    )
    result = await async_check_ha_config_file(hass)
    assert not result.errors and not result.warnings
    now = dt_util.now()
    midnight = now.replace(hour=0, minute=0, second=0, microsecond=0)
    for entry in result["sensor"]:
        # An unquoted on is the YAML boolean and becomes the state True.
        assert entry["state"] == ["on"]
        for template in (entry["start"], entry["end"]):
            template.hass = hass
        start, end = async_calculate_period(None, entry["start"], entry["end"])
        assert start == midnight
        assert midnight <= end and abs((end - now).total_seconds()) < 60


# A meter keeps a timer for the end of its cycle.
@pytest.mark.parametrize("expected_lingering_timers", [True])
async def test_the_utility_meters_are_created(hass, expected_lingering_timers):
    meters = utility_meters()
    for source in {m["source"] for m in meters.values()}:
        hass.states.async_set(source, "10", {"state_class": "total_increasing"})
    assert await async_setup_component(hass, "utility_meter", {"utility_meter": meters})
    await hass.async_block_till_done()
    for meter in meters.values():
        assert hass.states.get(f"sensor.{slugify(meter['name'])}"), meter["name"]


async def test_the_examples_name_the_entities_of_the_integration(hass, client):
    # The device Living (VA0000000001) reports a humidity.
    client.nodes_json[0]["HumidityPct"] = 41.5
    client.nodes_json[0]["Telemetry"]["humidity_pct"] = "41.50%"
    with_gas(client)
    entry = await setup_entry(hass, client)
    registry = er.async_get(hass)
    known = {e.entity_id for e in er.async_entries_for_config_entry(registry, entry.entry_id)}

    sources = [e["entity_id"] for e in history_stats_entries()]
    sources += [m["source"] for m in utility_meters().values()]
    sources += [e for c in cards() for e in c["entities"]]
    assert set(sources) <= known
    assert "binary_sensor.living_heating" in sources
    assert {"sensor.freedo_gas_volume", "sensor.freedo_heat_output_energy"} <= set(sources)

    # The device readings are off by default: enable the ones the cards use.
    card_entities = {e for c in cards() for e in c["entities"]}
    for entity in card_entities:
        registry.async_update_entity(entity, disabled_by=None)
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        await hass.config_entries.async_reload(entry.entry_id)
        await hass.async_block_till_done()
    for entity in card_entities:
        assert hass.states.get(entity).attributes["state_class"] == "measurement", entity
    for source in {m["source"] for m in utility_meters().values()}:
        assert hass.states.get(source).attributes["state_class"] == "total_increasing"
    # history_stats counts the on state of a binary sensor.
    assert hass.states.get("binary_sensor.living_heating").state in ("on", "off")


def test_the_cards_use_the_options_of_the_card():
    for card in cards():
        assert set(card) <= CARD_KEYS
        assert set(card["stat_types"]) <= CARD_STATS
        assert card["period"] in CARD_PERIODS
        assert card["entities"] and card["days_to_show"] >= 1
