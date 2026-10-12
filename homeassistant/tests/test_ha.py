"""Home Assistant tests of the entities, the device registry and the
environment inputs, against a fake daemon client.

They run only with pytest-homeassistant-custom-component installed.
"""

import asyncio
import copy
import json
import pathlib
from datetime import timedelta
from unittest.mock import patch

import pytest
import voluptuous as vol

pytest.importorskip("pytest_homeassistant_custom_component")

from homeassistant.components.climate import (  # noqa: E402
    ATTR_CURRENT_TEMPERATURE,
    ATTR_HVAC_MODE,
    ATTR_PRESET_MODE,
    DOMAIN as CLIMATE_DOMAIN,
    PRESET_BOOST,
    SERVICE_SET_HVAC_MODE,
    SERVICE_SET_PRESET_MODE,
    SERVICE_SET_TEMPERATURE,
    SERVICE_TURN_ON,
    HVACMode,
)
from homeassistant.const import (  # noqa: E402
    ATTR_ENTITY_ID,
    ATTR_TEMPERATURE,
    CONF_HOST,
    CONF_PORT,
)
from homeassistant.data_entry_flow import FlowResultType  # noqa: E402
from homeassistant.exceptions import HomeAssistantError, ServiceValidationError  # noqa: E402
from homeassistant.helpers import (  # noqa: E402
    area_registry as ar,
    device_registry as dr,
    entity_registry as er,
    issue_registry as ir,
)
from homeassistant.util import dt as dt_util  # noqa: E402
from pytest_homeassistant_custom_component.common import (  # noqa: E402
    MockConfigEntry,
    async_fire_time_changed,
)

import custom_components.freedo  # noqa: E402,F401  # the target of the client patch
from custom_components.freedo.api import FreedoApiError, FreedoConnectionError  # noqa: E402
from custom_components.freedo.const import CONF_ROOM_TEMP_PAUSED  # noqa: E402
from custom_components.freedo.model import OFFSET_MODES, ZONE_OVERLAY_RULES  # noqa: E402
from test_model import (  # noqa: E402
    BOILER,
    CIRCUITS,
    ENERGY,
    FLOW_CIRCUIT,
    GAS,
    HEALTH,
    HOME,
    INSTALL,
    INSTALL_DEVICES,
    INSTALL_ZONES,
    NODES,
    HOT_WATER_WEEK,
    PRESETS,
    SCHEDULE,
    WEEK,
    ZONES,
)

DOMAIN = "freedo"
LIVING = "climate.living"
BEDROOM = "climate.bedroom"
# The hot water device takes its translated name, so a new entity id follows it.
HOT_WATER = "water_heater.hot_water"
OFFLINE_SCHEDULE = "switch.freedo_offline_schedule"


class FakeClient:
    """The daemon API: reads serve the fixtures, writes are recorded."""

    def __init__(self) -> None:
        self.health_json = copy.deepcopy(HEALTH)
        self.zones_json = copy.deepcopy(ZONES)
        self.nodes_json = copy.deepcopy(NODES)
        self.boiler_json = copy.deepcopy(BOILER)
        self.home_json = copy.deepcopy(HOME)
        self.install_json = copy.deepcopy(INSTALL_ZONES)
        self.install_devices_json = copy.deepcopy(INSTALL_DEVICES)
        self.settings_json = copy.deepcopy(INSTALL)
        self.circuits_json = copy.deepcopy(CIRCUITS)
        # GET /api/zones/{n}/schedule by zone: zone 1 holds a schedule, zone 2 none.
        self.schedules = {1: copy.deepcopy(SCHEDULE)}
        # The error GET /api/install raises, None to answer settings_json.
        self.settings_error: Exception | None = None
        self.calls: list[tuple] = []
        # The end rule of each setpoint and off write: (name, zone, mode, durationSec).
        self.end_rules: list[tuple] = []
        # Window pushes as the daemon receives them, in arrival order.
        self.windows: list[tuple[int, bool]] = []
        self.window_gate: asyncio.Event | None = None
        # Errors the next window pushes raise, one per push.
        self.window_errors: list[Exception] = []
        # Room temperature pushes and clears as the daemon receives them.
        self.room_temps: list[tuple[int, float]] = []
        self.room_temp_clears: list[int] = []
        # The source label of each push, in the order of room_temps.
        self.room_temp_sources: list[tuple[int, str | None]] = []
        # The order of pushes and clears as the daemon receives them.
        self.room_temp_events: list[tuple] = []
        # A push that waits on the gate reaches the daemon late.
        self.room_temp_gate: asyncio.Event | None = None
        self.room_temp_reached: asyncio.Event = asyncio.Event()
        # Errors the next room temperature pushes and clears raise, one per call.
        self.room_temp_errors: list[Exception] = []
        # GET /api/pair/window, None for a daemon without the route. The times are
        # left out while the window never opened.
        self.pair_window_json: dict | None = {"open": False, "responder": False, "joiners": []}
        # GET /api/presets, None for a daemon without the route.
        self.presets_json: dict | None = copy.deepcopy(PRESETS)
        # The reads of GET /api/presets, to see a selection poll again.
        self.preset_reads = 0

    async def health(self):
        return copy.deepcopy(self.health_json)

    async def zones(self):
        return copy.deepcopy(self.zones_json)

    async def nodes(self):
        return copy.deepcopy(self.nodes_json)

    async def boiler(self):
        return copy.deepcopy(self.boiler_json)

    async def home(self):
        return copy.deepcopy(self.home_json)

    async def install_zones(self):
        return copy.deepcopy(self.install_json)

    async def install_circuits(self):
        return copy.deepcopy(self.circuits_json)

    async def install_devices(self):
        return copy.deepcopy(self.install_devices_json)

    async def pair_window(self):
        return copy.deepcopy(self.pair_window_json)

    async def presets(self):
        self.preset_reads += 1
        return copy.deepcopy(self.presets_json)

    async def activate_preset(self, preset_id):
        self.calls.append(("activate_preset", preset_id))
        presets = (self.presets_json or {}).get("presets", [])
        for row in presets:
            row["active"] = row["id"] == preset_id
        name = next((p["name"] for p in presets if p["id"] == preset_id), "")
        return {"id": preset_id, "name": name, "active": True, "queued": 1}

    async def set_pair_window(self, seconds):
        self.calls.append(("pair_window", seconds))
        self.pair_window_json = {
            "open": seconds > 0, "responder": False, "joiners": [],
            "opened": "2026-01-01T00:00:00Z", "until": f"2026-01-01T00:{seconds // 60:02d}:00Z",
        }
        return copy.deepcopy(self.pair_window_json)

    async def install(self):
        if self.settings_error is not None:
            raise self.settings_error
        return copy.deepcopy(self.settings_json)

    async def patch_install(self, **fields):
        self.calls.append(("patch_install", dict(fields)))
        merged = {**self.settings_json, **fields}
        # The daemon keeps the minimum below the maximum.
        low, high = merged["boiler_min_power_kw"], merged["boiler_max_power_kw"]
        if high is not None and low >= high:
            raise FreedoApiError(400, f"options.boiler_min_power_kw: {low} is not below boiler_max_power_kw {high}")
        low, high = merged.get("boiler_min_gas_m3h"), merged.get("boiler_max_gas_m3h")
        if high is not None and low is not None and low >= high:
            raise FreedoApiError(400, f"options.boiler_min_gas_m3h: {low} is not below boiler_max_gas_m3h {high}")
        # The default curve reduces the manual limit and refuses to run without it.
        if merged.get("flow_default_curve") and merged.get("flow_limit_c") is None:
            raise FreedoApiError(400, "options.flow_default_curve: needs options.flow_limit_c")
        self.settings_json = merged
        return copy.deepcopy(merged)

    async def set_offline_schedule(self, enabled):
        self.calls.append(("offline_schedule", enabled))
        self.settings_json["offline_schedule"] = enabled
        return copy.deepcopy(self.settings_json)

    async def zone_setpoint(self, zone, temp_c, mode="manual", duration_sec=None):
        self.calls.append(("setpoint", zone, temp_c))
        self.end_rules.append(("setpoint", zone, mode, duration_sec))
        return {"queued": True}

    async def zone_auto(self, zone):
        self.calls.append(("auto", zone))
        return {"queued": True}

    async def zone_off(self, zone, mode="manual", duration_sec=None):
        self.calls.append(("off", zone))
        self.end_rules.append(("off", zone, mode, duration_sec))
        return {"queued": True}

    async def zone_boost(self, zone, on):
        self.calls.append(("boost", zone, on))
        return {"queued": True}

    async def set_child_lock(self, node, on):
        self.calls.append(("child_lock", node, on))
        return {"queued": True}

    async def identify(self, node):
        self.calls.append(("identify", node))
        return {"queued": True}

    async def calibrate(self, node):
        self.calls.append(("calibrate", node))
        return {"queued": True}

    async def set_zone_offset(self, zone, offset_c):
        self.calls.append(("offset", zone, offset_c))
        return {"rev": 8}

    async def patch_install_zone(self, zone, **fields):
        self.calls.append(("patch_zone", zone, dict(fields)))
        row = next(r for r in self.install_json["zones"] if r["number"] == zone)
        # The daemon checks the correction fields against the values the
        # request leaves.
        mode = fields.get("offset_mode", row.get("offset_mode"))
        auto = fields.get("auto_offset", row.get("auto_offset"))
        if "offset_mode" in fields and mode == "setpoint" and auto:
            raise FreedoApiError(400, f"offset_mode: zone {zone} has auto_offset on, switch it off first")
        if fields.get("auto_offset") and mode == "setpoint":
            raise FreedoApiError(400, f"auto_offset: zone {zone} is in offset mode setpoint")
        mode = fields.get("device_overlay_end")
        # The daemon drops the duration of a rule other than timer and refuses
        # a timer without one.
        if mode is not None and mode != "timer":
            row["device_overlay_end_sec"] = None
        if mode == "timer" and not (fields.get("device_overlay_end_sec") or row["device_overlay_end_sec"]):
            raise FreedoApiError(400, "device_overlay_end: mode timer needs a duration_sec")
        row.update(fields)
        return {"rev": 9, "warnings": [], "zone": copy.deepcopy(row)}

    async def set_zone_measuring(self, zone, ref):
        self.calls.append(("measuring", zone, ref))
        for view in self.zones_json["declared"]:
            if view["number"] == zone:
                view["measuringDevice"] = ref
        row = next(r for r in self.install_json["zones"] if r["number"] == zone)
        row["measuring_device"] = ref
        return {"rev": 10, "zone": zone, "measuring_device": ref}

    async def zone_schedule(self, zone):
        self.calls.append(("get_schedule", zone))
        return copy.deepcopy(self.schedules.get(zone))

    async def set_zone_schedule(self, zone, programs):
        self.calls.append(("set_schedule", zone, programs))
        return {"zone": zone, "programs": programs}

    async def set_flow_limit(self, circuit, temp_c, target):
        self.calls.append(("flow", circuit, temp_c, target))
        return {"queued": True}

    async def push_window(self, zone, is_open):
        if self.window_errors:
            raise self.window_errors.pop(0)
        # An open push that waits on the gate reaches the daemon late.
        if is_open and self.window_gate is not None:
            await self.window_gate.wait()
        self.windows.append((zone, is_open))
        return {"open": is_open}

    async def push_outdoor(self, temp_c):
        return {}

    async def push_room_temp(self, zone, temp_c, source=None):
        if self.room_temp_errors:
            raise self.room_temp_errors.pop(0)
        if self.room_temp_gate is not None:
            self.room_temp_reached.set()
            await self.room_temp_gate.wait()
        self.room_temps.append((zone, temp_c))
        self.room_temp_sources.append((zone, source))
        self.room_temp_events.append(("push", zone, temp_c))
        return {"tempC": temp_c}

    async def clear_room_temp(self, zone):
        if self.room_temp_errors:
            raise self.room_temp_errors.pop(0)
        self.room_temp_clears.append(zone)
        self.room_temp_events.append(("clear", zone))
        return {"tempC": None}

    async def set_away(self, away):
        return {"away": away}

    async def set_display(self, node, orientation=None, notification=None):
        return {}

    async def record_gas_meter(self, reading, estimate=None, at=None, replaced=False):
        self.calls.append(("gas_meter", reading, estimate, at, replaced))
        meter = {"value": reading, "lastReading": reading,
                 "lastReadingAt": at.isoformat() if at is not None else "2026-01-01T00:00:00Z",
                 "readings": 1, "factor": 1.0, "factorReadings": 0}
        self.boiler_json.setdefault("energy", {})["gas"] = dict(GAS, meter=meter)
        return {"meter": copy.deepcopy(meter)}

    async def undo_gas_meter_reading(self):
        self.calls.append(("undo_gas_meter",))
        if self.boiler_json.get("energy", {}).get("gas"):
            self.boiler_json["energy"]["gas"]["meter"] = None
        return {"meter": None}


@pytest.fixture(autouse=True)
def auto_enable_custom_integrations(enable_custom_integrations):
    yield


@pytest.fixture
def client() -> FakeClient:
    return FakeClient()


async def setup_entry(hass, client, options=None, enabled=()) -> MockConfigEntry:
    """Set up an entry on the fake client. enabled lists (platform, key) of the
    entities disabled by default to register enabled first, as an install that
    had them before keeps them."""
    entry = MockConfigEntry(
        domain=DOMAIN,
        data={CONF_HOST: "127.0.0.1", CONF_PORT: 8080},
        options=options or {},
    )
    entry.add_to_hass(hass)
    for platform, key in enabled:
        er.async_get(hass).async_get_or_create(
            platform, DOMAIN, f"{entry.entry_id}_{key}", config_entry=entry
        )
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        assert await hass.config_entries.async_setup(entry.entry_id)
        await hass.async_block_till_done()
    return entry


def entity_id(hass, entry, platform, key):
    """The entity id of the entity with the unique id <entry>_<key>."""
    found = er.async_get(hass).async_get_entity_id(platform, DOMAIN, f"{entry.entry_id}_{key}")
    assert found, f"no {platform} entity {key}"
    return found


def has_entity(hass, entry, platform, key):
    return er.async_get(hass).async_get_entity_id(platform, DOMAIN, f"{entry.entry_id}_{key}") is not None


async def refresh(hass, entry):
    await entry.runtime_data.coordinator.async_refresh()
    await hass.async_block_till_done()


async def call(hass, service, data):
    await hass.services.async_call(CLIMATE_DOMAIN, service, data, blocking=True)
    await hass.async_block_till_done()


def zone_device(hass, entry, number):
    return dr.async_get(hass).async_get_device_by_identifier((DOMAIN, f"{entry.entry_id}_zone{number}"), entry.entry_id)


def node_device(hass, entry, short):
    return dr.async_get(hass).async_get_device_by_identifier((DOMAIN, f"{entry.entry_id}_{short}"), entry.entry_id)


def place_zone(hass, entry, number, area_name):
    """Set the area of a zone device as the device page does. The integration
    reads it at the next poll."""
    area = ar.async_get(hass).async_get_or_create(area_name)
    dr.async_get(hass).async_update_device(zone_device(hass, entry, number).id, area_id=area.id)
    return area.id


def add_sensor(hass, entry, entity, name, value, area=None, device_area=None):
    """A temperature sensor of another integration, in area itself or on a
    device in device_area."""
    entities = er.async_get(hass)
    registered = entities.async_get_or_create(
        "sensor", "test", entity, suggested_object_id=entity.split(".", 1)[1]
    )
    assert registered.entity_id == entity
    device_id = None
    if device_area is not None:
        other = MockConfigEntry(domain="test")
        other.add_to_hass(hass)
        device = dr.async_get(hass).async_get_or_create(
            config_entry_id=other.entry_id, identifiers={("test", entity)}
        )
        dr.async_get(hass).async_update_device(device.id, area_id=device_area)
        device_id = device.id
    entities.async_update_entity(entity, area_id=area, device_id=device_id)
    hass.states.async_set(
        entity,
        value,
        {"device_class": "temperature", "unit_of_measurement": "°C", "friendly_name": name},
    )


def no_area_issue(hass, entry):
    """The rooms the open no-area issue lists, None without one or once the
    user ignored it."""
    issue = ir.async_get(hass).async_get_issue(DOMAIN, f"rooms_no_area_{entry.entry_id}")
    if issue is None or issue.dismissed_version is not None:
        return None
    return issue.translation_placeholders["rooms"]


# --- item 1 and 4: current temperature ---


async def test_current_temperature_waits_for_4060(hass, client):
    # Zone 1 sent 40a0 but no 4060: the daemon reports TempC = 0 + 0.5.
    for view in client.zones_json["demand"]:
        if view["Number"] == 1:
            view["TempC"] = 0.5
    await setup_entry(hass, client)
    # bbbb, the measuring device, reads 20.7 on the wire scale.
    assert hass.states.get(LIVING).attributes[ATTR_CURRENT_TEMPERATURE] == 21.2


async def test_current_temperature_unknown_without_012d(hass, client):
    # dddd, the only member of zone 2, is heard but never sent 012d.
    client.nodes_json[2]["TempC"] = 0
    client.nodes_json[2]["Telemetry"] = {}
    await setup_entry(hass, client)
    assert hass.states.get(BEDROOM).attributes[ATTR_CURRENT_TEMPERATURE] is None


# --- item 2: climate commands ---


@pytest.mark.parametrize(
    ("mode", "call_name"), [(HVACMode.AUTO, "auto"), (HVACMode.OFF, "off")]
)
async def test_mode_change_drops_pending_target(hass, client, mode, call_name):
    await setup_entry(hass, client)
    await call(hass, SERVICE_SET_TEMPERATURE, {ATTR_ENTITY_ID: BEDROOM, ATTR_TEMPERATURE: 22})
    assert hass.states.get(BEDROOM).attributes[ATTR_TEMPERATURE] == 22
    await call(hass, SERVICE_SET_HVAC_MODE, {ATTR_ENTITY_ID: BEDROOM, ATTR_HVAC_MODE: mode})
    assert client.calls[-1] == (call_name, 2)
    state = hass.states.get(BEDROOM)
    assert state.state == mode
    # The daemon still reports the program target 17.
    assert state.attributes[ATTR_TEMPERATURE] == 17


async def test_boost_drops_pending_target(hass, client):
    await setup_entry(hass, client)
    await call(hass, SERVICE_SET_TEMPERATURE, {ATTR_ENTITY_ID: BEDROOM, ATTR_TEMPERATURE: 22})
    await call(
        hass, SERVICE_SET_PRESET_MODE, {ATTR_ENTITY_ID: BEDROOM, ATTR_PRESET_MODE: PRESET_BOOST}
    )
    assert client.calls[-1] == ("boost", 2, True)
    state = hass.states.get(BEDROOM)
    assert state.attributes[ATTR_TEMPERATURE] == 17
    assert state.state == HVACMode.AUTO


@pytest.mark.parametrize(
    ("mode", "expected"), [(HVACMode.AUTO, ("auto", 2)), (HVACMode.OFF, ("off", 2))]
)
async def test_set_temperature_honours_mode(hass, client, mode, expected):
    await setup_entry(hass, client)
    await call(
        hass,
        SERVICE_SET_TEMPERATURE,
        {ATTR_ENTITY_ID: BEDROOM, ATTR_TEMPERATURE: 21, ATTR_HVAC_MODE: mode},
    )
    assert client.calls == [expected]
    state = hass.states.get(BEDROOM)
    assert state.state == mode
    assert state.attributes[ATTR_TEMPERATURE] == 17


async def test_set_temperature_with_heat(hass, client):
    await setup_entry(hass, client)
    await call(
        hass,
        SERVICE_SET_TEMPERATURE,
        {ATTR_ENTITY_ID: BEDROOM, ATTR_TEMPERATURE: 21, ATTR_HVAC_MODE: HVACMode.HEAT},
    )
    assert client.calls == [("setpoint", 2, 21.0)]
    state = hass.states.get(BEDROOM)
    assert state.state == HVACMode.HEAT
    assert state.attributes[ATTR_TEMPERATURE] == 21


async def test_heat_clamps_corrected_target(hass, client):
    # Zone 1 holds a manual overlay at 26 C on the corrected scale (25 + 1).
    target = client.zones_json["declared"][0]["target"]
    target.update(targetC=26.0, source="overlay", boost=False)
    target["overlay"] = {"kind": "manual", "on": True, "setpointC": 26.0, "endsAt": None}
    await setup_entry(hass, client)
    await call(hass, SERVICE_SET_HVAC_MODE, {ATTR_ENTITY_ID: LIVING, ATTR_HVAC_MODE: HVACMode.HEAT})
    assert client.calls == [("setpoint", 1, 25.0)]


async def test_turn_on_returns_to_program(hass, client):
    await setup_entry(hass, client)
    await call(hass, SERVICE_TURN_ON, {ATTR_ENTITY_ID: BEDROOM})
    assert client.calls == [("auto", 2)]


# --- item 3: device registry ---


async def test_devices_link_to_hub(hass, client, caplog):
    entry = await setup_entry(hass, client)
    registry = dr.async_get(hass)
    hub = registry.async_get_device_by_identifier((DOMAIN, entry.entry_id), entry.entry_id)
    assert hub is not None
    assert hub.via_device_id is None
    children = [
        d for d in dr.async_entries_for_config_entry(registry, entry.entry_id) if d.id != hub.id
    ]
    names = {d.name for d in children}
    # The hot water zone takes its translated name, not the stored one.
    assert {"Living", "Bedroom", "Hot water"} <= names
    assert any(d.serial_number == "VA0000000001" for d in children)
    # The node fw/state telemetry sets the firmware of the device.
    by_serial = {d.serial_number: d for d in children}
    assert by_serial["VA0000000001"].sw_version == "2.5"
    assert all(d.via_device_id == hub.id for d in children), [
        (d.name, d.via_device_id) for d in children
    ]
    assert "via_device" not in caplog.text


async def test_firmware_change_reaches_the_device_registry(hass, client):
    entry = await setup_entry(hass, client)
    registry = dr.async_get(hass)
    device = registry.async_get_device_by_identifier((DOMAIN, f"{entry.entry_id}_aaaa"), entry.entry_id)
    assert device.sw_version == "2.5"

    # The device reports a new firmware after an OTA, with no reload.
    client.nodes_json[0]["Telemetry"]["fw_run_version"] = "3.1"
    await refresh(hass, entry)
    assert registry.async_get(device.id).sw_version == "3.1"


# --- rooms: zone areas, names and repair issues ---


async def test_area_set_on_the_device_page_is_read_at_the_poll(hass, client):
    entry = await setup_entry(hass, client)
    client.calls.clear()
    living_room = place_zone(hass, entry, 1, "Soggiorno")
    await refresh(hass, entry)
    # The valve with no area takes the area of its zone, and the daemon gets
    # the area as its display label.
    assert node_device(hass, entry, "aaaa").area_id == living_room
    assert client.calls == [("patch_zone", 1, {"ha_area": {"id": living_room, "name": "Soggiorno"}})]
    # The label went through: the next poll sends nothing.
    client.calls.clear()
    await refresh(hass, entry)
    assert client.calls == []


async def test_area_rename_reaches_the_daemon(hass, client):
    entry = await setup_entry(hass, client)
    living_room = place_zone(hass, entry, 1, "Soggiorno")
    await refresh(hass, entry)
    client.calls.clear()
    ar.async_get(hass).async_update(living_room, name="Salotto")
    await refresh(hass, entry)
    assert client.calls == [("patch_zone", 1, {"ha_area": {"id": living_room, "name": "Salotto"}})]


async def test_area_follows_the_rule_for_each_device(hass, client):
    # bbbb and dddd become node devices: bbbb follows zone 1, and dddd (zone 2)
    # joins zone 1 later.
    health = client.health_json["devices"]
    health.append(dict(health[0], device="VA0000000002", short="bbbb", label=""))
    health.append(dict(health[0], device="VA0000000004", short="dddd", label="Bedroom"))
    entry = await setup_entry(hass, client)
    areas = ar.async_get(hass)
    office = areas.async_create("Office").id
    devices = dr.async_get(hass)

    first = place_zone(hass, entry, 1, "Soggiorno")
    await refresh(hass, entry)
    assert node_device(hass, entry, "aaaa").area_id == first
    assert node_device(hass, entry, "bbbb").area_id == first

    # The user places bbbb in the office; a later poll leaves it there.
    devices.async_update_device(node_device(hass, entry, "bbbb").id, area_id=office)
    await refresh(hass, entry)
    assert node_device(hass, entry, "bbbb").area_id == office

    # dddd moves to zone 1 with no area of its own, and the zone moves area: a
    # device with the previous area moves, a free one moves, a placed one stays.
    client.zones_json["declared"][0]["members"] = ["aaaa", "bbbb", "dddd"]
    client.zones_json["declared"][1]["members"] = []
    client.install_devices_json["devices"][2]["zone"] = 1
    second = place_zone(hass, entry, 1, "Salotto")
    await refresh(hass, entry)
    assert node_device(hass, entry, "aaaa").area_id == second
    assert node_device(hass, entry, "dddd").area_id == second
    assert node_device(hass, entry, "bbbb").area_id == office


async def test_a_cleared_device_stays_cleared(hass, client):
    entry = await setup_entry(hass, client)
    place_zone(hass, entry, 1, "Soggiorno")
    await refresh(hass, entry)
    valve = node_device(hass, entry, "aaaa")
    assert valve.area_id is not None
    # The user clears the area of the valve: the polls leave it so.
    dr.async_get(hass).async_update_device(valve.id, area_id=None)
    for _ in range(3):
        await refresh(hass, entry)
    assert node_device(hass, entry, "aaaa").area_id is None


async def test_a_new_device_takes_the_area_of_its_zone(hass, client):
    entry = await setup_entry(hass, client)
    living_room = place_zone(hass, entry, 1, "Soggiorno")
    await refresh(hass, entry)
    dr.async_get(hass).async_update_device(node_device(hass, entry, "aaaa").id, area_id=None)
    # bbbb is paired: the reload creates its device, with no area.
    health = client.health_json["devices"]
    health.append(dict(health[0], device="VA0000000002", short="bbbb", label=""))
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        await hass.config_entries.async_reload(entry.entry_id)
        await hass.async_block_till_done()
    assert node_device(hass, entry, "bbbb").area_id == living_room
    # The valve the user cleared is not new and stays cleared.
    assert node_device(hass, entry, "aaaa").area_id is None


async def test_a_restart_with_the_same_area_moves_nothing(hass, client):
    entry = await setup_entry(hass, client)
    place_zone(hass, entry, 1, "Soggiorno")
    await refresh(hass, entry)
    dr.async_get(hass).async_update_device(node_device(hass, entry, "aaaa").id, area_id=None)
    await hass.config_entries.async_unload(entry.entry_id)
    # A restart: the memory of the devices is gone, and every device is older
    # than the load of the integration.
    hass.data.pop("freedo_seen_devices")
    with (
        patch("custom_components.freedo.rooms._LOADED_AT", dt_util.utcnow()),
        patch("custom_components.freedo.FreedoClient", return_value=client),
    ):
        await hass.config_entries.async_reload(entry.entry_id)
        await hass.async_block_till_done()
    assert node_device(hass, entry, "aaaa").area_id is None


async def test_previous_area_survives_a_restart_on_the_daemon(hass, client):
    # The daemon holds the area the integration sent before the restart. The
    # zone moved while Home Assistant was down: the valve with the old area
    # follows.
    entry = await setup_entry(hass, client)
    old = place_zone(hass, entry, 1, "Soggiorno")
    await refresh(hass, entry)
    assert client.install_json["zones"][0]["ha_area"] == {"id": old, "name": "Soggiorno"}
    await hass.config_entries.async_unload(entry.entry_id)
    new = place_zone(hass, entry, 1, "Salotto")
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        await hass.config_entries.async_reload(entry.entry_id)
        await hass.async_block_till_done()
    assert node_device(hass, entry, "aaaa").area_id == new


async def test_wall_thermostat_takes_the_area_of_its_heating_zone(hass, client):
    # cccc serves zone 1 and the hot water zone 9; its row names zone 9.
    client.zones_json["declared"][0]["members"] = ["aaaa", "bbbb", "cccc"]
    entry = await setup_entry(hass, client)
    living_room = place_zone(hass, entry, 1, "Soggiorno")
    place_zone(hass, entry, 9, "Utility")
    await refresh(hass, entry)
    assert node_device(hass, entry, "cccc").area_id == living_room


async def test_hot_water_zone_takes_an_area_without_an_issue(hass, client):
    entry = await setup_entry(hass, client)
    utility = place_zone(hass, entry, 9, "Utility")
    await refresh(hass, entry)
    # cccc follows the hot water zone alone.
    assert node_device(hass, entry, "cccc").area_id == utility
    assert ("patch_zone", 9, {"ha_area": {"id": utility, "name": "Utility"}}) in client.calls
    assert no_area_issue(hass, entry) == "Living, Bedroom"


async def test_no_area_label_on_a_daemon_without_the_field(hass, client):
    for row in client.install_json["zones"]:
        del row["ha_area"]
    entry = await setup_entry(hass, client)
    living_room = place_zone(hass, entry, 1, "Soggiorno")
    await refresh(hass, entry)
    assert node_device(hass, entry, "aaaa").area_id == living_room
    assert client.calls == []


async def test_a_refused_area_label_is_sent_again(hass, client, caplog):
    entry = await setup_entry(hass, client)
    patch_zone = client.patch_install_zone

    async def refuse(zone, **fields):
        raise FreedoConnectionError("daemon down")

    client.patch_install_zone = refuse
    living_room = place_zone(hass, entry, 1, "Soggiorno")
    await refresh(hass, entry)
    await refresh(hass, entry)
    assert caplog.text.count("Area of zone 1 not sent to the daemon") == 1
    client.patch_install_zone = patch_zone
    await refresh(hass, entry)
    assert client.install_json["zones"][0]["ha_area"] == {"id": living_room, "name": "Soggiorno"}


async def test_zone_rename_reaches_the_device(hass, client):
    entry = await setup_entry(hass, client)
    devices = dr.async_get(hass)
    client.zones_json["declared"][0]["name"] = "Lounge"
    await refresh(hass, entry)
    device = zone_device(hass, entry, 1)
    assert device.name == "Lounge"
    # The entity ids stay.
    assert hass.states.get(LIVING) is not None

    # A name set in Home Assistant wins.
    devices.async_update_device(device.id, name_by_user="Sala")
    client.zones_json["declared"][0]["name"] = "Salon"
    await refresh(hass, entry)
    device = zone_device(hass, entry, 1)
    assert (device.name, device.name_by_user) == ("Lounge", "Sala")
    # The hot water zone keeps its translated name.
    client.zones_json["declared"][2]["name"] = "Boiler"
    await refresh(hass, entry)
    assert zone_device(hass, entry, 9).name == "Domestic hot water"


async def test_rooms_without_an_area_raise_one_repair_issue(hass, client):
    entry = await setup_entry(hass, client)
    # One issue lists the heating rooms; the hot water zone is not a room.
    assert no_area_issue(hass, entry) == "Living, Bedroom"
    issues = [i for (d, _), i in ir.async_get(hass).issues.items() if d == DOMAIN]
    assert len(issues) == 1
    assert issues[0].translation_key == "rooms_no_area"
    assert issues[0].is_fixable is False
    assert issues[0].severity is ir.IssueSeverity.WARNING

    # The list follows the areas, and the issue goes with the last room.
    place_zone(hass, entry, 1, "Soggiorno")
    await refresh(hass, entry)
    assert no_area_issue(hass, entry) == "Bedroom"
    place_zone(hass, entry, 2, "Camera")
    await refresh(hass, entry)
    assert ir.async_get(hass).async_get_issue(DOMAIN, f"rooms_no_area_{entry.entry_id}") is None


async def test_an_ignored_no_area_issue_stays_ignored(hass, client):
    entry = await setup_entry(hass, client)
    ir.async_ignore_issue(hass, DOMAIN, f"rooms_no_area_{entry.entry_id}", True)
    await refresh(hass, entry)
    assert no_area_issue(hass, entry) is None
    # A change of the list keeps it ignored.
    place_zone(hass, entry, 1, "Soggiorno")
    await refresh(hass, entry)
    issue = ir.async_get(hass).async_get_issue(DOMAIN, f"rooms_no_area_{entry.entry_id}")
    assert issue.translation_placeholders["rooms"] == "Bedroom"
    assert issue.dismissed_version is not None


async def test_removing_the_entry_removes_its_issue(hass, client):
    entry = await setup_entry(hass, client)
    assert no_area_issue(hass, entry)
    await hass.config_entries.async_remove(entry.entry_id)
    await hass.async_block_till_done()
    assert ir.async_get(hass).async_get_issue(DOMAIN, f"rooms_no_area_{entry.entry_id}") is None


# --- item 5: window pushes ---


async def test_window_pushes_keep_order(hass, client):
    sensor = "binary_sensor.living_window"
    hass.states.async_set(sensor, "off")
    await setup_entry(hass, client, options={"windows": {sensor: 1}})
    assert client.windows == [(1, False)]

    client.window_gate = asyncio.Event()
    hass.states.async_set(sensor, "on")
    for _ in range(5):
        await asyncio.sleep(0)
    hass.states.async_set(sensor, "off")
    for _ in range(5):
        await asyncio.sleep(0)
    client.window_gate.set()
    await hass.async_block_till_done()
    # The daemon ends on the state the sensor has now.
    assert client.windows[-1] == (1, False)
    assert client.windows == [(1, False), (1, True), (1, False)]


async def test_failed_window_push_is_retried(hass, client):
    sensor = "binary_sensor.living_window"
    hass.states.async_set(sensor, "off")
    await setup_entry(hass, client, options={"windows": {sensor: 1}})
    assert client.windows == [(1, False)]

    # The daemon is down: the open does not reach it on the change.
    client.window_errors = [FreedoConnectionError("daemon down")]
    hass.states.async_set(sensor, "on")
    await hass.async_block_till_done()
    assert client.windows == [(1, False)]

    # It reaches it on the retry, and a delivered zone is not pushed again.
    now = dt_util.utcnow()
    async_fire_time_changed(hass, now + timedelta(minutes=1))
    await hass.async_block_till_done()
    assert client.windows == [(1, False), (1, True)]
    async_fire_time_changed(hass, now + timedelta(minutes=2))
    await hass.async_block_till_done()
    assert client.windows == [(1, False), (1, True)]


async def test_refused_window_push_is_not_retried(hass, client):
    sensor = "binary_sensor.living_window"
    hass.states.async_set(sensor, "off")
    await setup_entry(hass, client, options={"windows": {sensor: 1}})

    # A 4xx refuses the request itself: the same push would fail again.
    client.window_errors = [FreedoApiError(400, "zone 1: the zone has no open window")]
    hass.states.async_set(sensor, "on")
    await hass.async_block_till_done()
    async_fire_time_changed(hass, dt_util.utcnow() + timedelta(minutes=1))
    await hass.async_block_till_done()
    assert client.windows == [(1, False)]


# --- room temperature pushes ---

ROOM_SENSOR = "sensor.living_temperature"
HALL_SENSOR = "sensor.hall_temperature"


def set_room_sensor(hass, state, unit="°C", entity=ROOM_SENSOR):
    hass.states.async_set(entity, state, {"unit_of_measurement": unit})


async def tick(hass, minutes):
    async_fire_time_changed(hass, dt_util.utcnow() + timedelta(minutes=minutes))
    await hass.async_block_till_done()


async def select_option(hass, entity, option):
    await hass.services.async_call(
        "select", "select_option", {ATTR_ENTITY_ID: entity, "option": option}, blocking=True
    )
    await hass.async_block_till_done()


async def test_room_temperature_is_pushed_to_its_zone(hass, client):
    set_room_sensor(hass, "21.4")
    await setup_entry(hass, client, options={"room_temperatures": {"1": ROOM_SENSOR}})
    # The integration sends the state it finds at start, with the sensor name.
    assert client.room_temps == [(1, 21.4)]
    assert client.room_temp_sources == [(1, "living temperature")]

    set_room_sensor(hass, "21.9")
    await hass.async_block_till_done()
    assert client.room_temps == [(1, 21.4), (1, 21.9)]

    # The periodic push keeps the reading fresh while the sensor stays still.
    await tick(hass, 16)
    assert client.room_temps == [(1, 21.4), (1, 21.9), (1, 21.9)]


async def test_room_temperature_without_a_source_on_an_older_daemon(hass, client):
    # A daemon that does not serve externalTemp.source refuses the field.
    del client.zones_json["declared"][0]["externalTemp"]["source"]
    set_room_sensor(hass, "21.4")
    await setup_entry(hass, client, options={"room_temperatures": {"1": ROOM_SENSOR}})
    assert client.room_temp_sources == [(1, None)]


async def test_room_temperature_in_fahrenheit_is_converted(hass, client):
    set_room_sensor(hass, "70.7", "°F")
    await setup_entry(hass, client, options={"room_temperatures": {"1": ROOM_SENSOR}})
    assert client.room_temps == [(1, 21.5)]


async def test_room_temperature_that_is_not_a_reading_is_not_pushed(hass, client):
    set_room_sensor(hass, "21.4")
    await setup_entry(hass, client, options={"room_temperatures": {"1": ROOM_SENSOR}})
    assert client.room_temps == [(1, 21.4)]

    # The daemon answers 400 to a value outside -10..50 C.
    for state in ("unavailable", "unknown", "not a number", "99"):
        set_room_sensor(hass, state)
        await hass.async_block_till_done()
    await tick(hass, 16)
    assert client.room_temps == [(1, 21.4)]

    hass.states.async_remove(ROOM_SENSOR)
    await tick(hass, 32)
    assert client.room_temps == [(1, 21.4)]


async def test_one_sensor_can_serve_two_zones(hass, client):
    set_room_sensor(hass, "20.5", entity=HALL_SENSOR)
    await setup_entry(
        hass, client, options={"room_temperatures": {"1": HALL_SENSOR, "2": HALL_SENSOR}}
    )
    assert sorted(client.room_temps) == [(1, 20.5), (2, 20.5)]
    set_room_sensor(hass, "20.8", entity=HALL_SENSOR)
    await hass.async_block_till_done()
    assert sorted(client.room_temps[2:]) == [(1, 20.8), (2, 20.8)]


async def test_failed_room_temperature_push_does_not_break_the_listener(hass, client, caplog):
    set_room_sensor(hass, "21.4")
    await setup_entry(hass, client, options={"room_temperatures": {"1": ROOM_SENSOR}})

    # A refusal and a lost connection: the zone logs the first one only.
    client.room_temp_errors = [
        FreedoApiError(409, "zone 1: an external room temperature needs offset_mode setpoint"),
        FreedoConnectionError("daemon down"),
    ]
    for state in ("21.5", "21.6"):
        set_room_sensor(hass, state)
        await hass.async_block_till_done()
    assert client.room_temps == [(1, 21.4)]
    warnings = [r for r in caplog.records if "Room temperature push for zone 1 failed" in r.message]
    assert len(warnings) == 1
    assert "needs offset_mode setpoint" in warnings[0].message

    # The next change reaches the daemon.
    set_room_sensor(hass, "21.7")
    await hass.async_block_till_done()
    assert client.room_temps == [(1, 21.4), (1, 21.7)]


async def test_no_room_temperature_push_without_a_mapping(hass, client):
    set_room_sensor(hass, "21.4")
    await setup_entry(hass, client)
    set_room_sensor(hass, "21.9")
    await tick(hass, 16)
    assert client.room_temps == []


# --- temperature source select ---

LIVING_SOURCE = "zone1_temperature_source"
BEDROOM_SOURCE = "zone2_temperature_source"
LIVING_DEVICE = "Living (VA0000000001)"
# The label of the mapped sensors: their name from the entity id, and the reading.
ROOM_LABEL = "living temperature (21.4 °C)"
OTHER = "other_sensor"


async def test_temperature_source_options_and_current(hass, client):
    set_room_sensor(hass, "21.4")
    set_room_sensor(hass, "20.1", entity=HALL_SENSOR)
    # Zone 2 measures on dddd (offset mode config), zone 1 maps a sensor
    # (offset mode setpoint). Neither zone has an area.
    client.zones_json["declared"][1]["measuringDevice"] = "dddd"
    entry = await setup_entry(
        hass, client, options={"room_temperatures": {"1": ROOM_SENSOR, "2": HALL_SENSOR}}
    )
    living = entity_id(hass, entry, "select", LIVING_SOURCE)
    bedroom = entity_id(hass, entry, "select", BEDROOM_SOURCE)
    # The mapped sensor, the devices of the room, then the other sensor.
    assert hass.states.get(living).attributes["options"] == [
        ROOM_LABEL, LIVING_DEVICE, "VA0000000002", OTHER,
    ]
    assert er.async_get(hass).async_get(living).entity_category == "config"
    # Zone 1 regulates on the mapped sensor: it is in use, the mode is setpoint.
    assert hass.states.get(living).state == ROOM_LABEL
    # Zone 2 has offset mode config: the daemon refuses a reading there.
    assert hass.states.get(bedroom).state == "Bedroom (VA0000000004)"


async def test_temperature_source_without_an_area_or_a_sensor(hass, client):
    entry = await setup_entry(hass, client)
    living = entity_id(hass, entry, "select", LIVING_SOURCE)
    assert hass.states.get(living).attributes["options"] == [LIVING_DEVICE, "VA0000000002", OTHER]
    assert hass.states.get(living).state == "VA0000000002"


async def test_temperature_source_exists_without_a_device_of_the_room(hass, client):
    # No member of zone 2 measures in its room: the select still offers the
    # other sensor.
    client.install_devices_json["devices"][2]["zone"] = 3
    entry = await setup_entry(hass, client)
    bedroom = entity_id(hass, entry, "select", BEDROOM_SOURCE)
    assert hass.states.get(bedroom).attributes["options"] == [OTHER]


async def test_temperature_source_offers_members_without_an_install_zone(hass, client):
    # Most installations leave the install zone of a device empty: the declared
    # members are the candidates.
    for row in client.install_devices_json["devices"]:
        row["zone"] = None
    entry = await setup_entry(hass, client)
    bedroom = entity_id(hass, entry, "select", BEDROOM_SOURCE)
    assert hass.states.get(bedroom).attributes["options"] == ["Bedroom (VA0000000004)", OTHER]


async def test_temperature_source_excludes_a_device_of_another_room(hass, client):
    # The wall thermostat eeee is a member of zone 2 but sits in room 3.
    client.zones_json["declared"][1]["members"] = ["dddd", "eeee"]
    client.install_devices_json["devices"].append(
        {"eui": "0000000000000005", "short": "eeee", "kind": "thermostat",
         "serial": "RU0000000002", "label": "Hall", "notes": "", "zone": 3}
    )
    entry = await setup_entry(hass, client)
    bedroom = entity_id(hass, entry, "select", BEDROOM_SOURCE)
    assert hass.states.get(bedroom).attributes["options"] == ["Bedroom (VA0000000004)", OTHER]


async def test_temperature_source_lists_the_sensors_of_the_area(hass, client):
    entry = await setup_entry(hass, client)
    living_room = place_zone(hass, entry, 1, "Soggiorno")
    office = ar.async_get(hass).async_create("Office").id
    add_sensor(hass, entry, "sensor.soggiorno_thermometer", "Soggiorno thermometer", "20.4", area=living_room)
    add_sensor(hass, entry, "sensor.aqara_living", "Aqara TH living", "20.1", device_area=living_room)
    add_sensor(hass, entry, "sensor.office", "Office thermometer", "19.0", area=office)
    # A sensor of another device class is no temperature.
    hass.states.async_set("sensor.living_humidity", "48", {"device_class": "humidity"})
    await refresh(hass, entry)
    living = entity_id(hass, entry, "select", LIVING_SOURCE)
    # The valve aaaa moved to the area with the zone, and its own temperature
    # sensor is a device option, not a sensor option.
    assert hass.states.get(living).attributes["options"] == [
        "Aqara TH living (20.1 °C)",
        "Soggiorno thermometer (20.4 °C)",
        LIVING_DEVICE,
        "VA0000000002",
        OTHER,
    ]


async def test_temperature_source_other_sensor_explains_where_to_map_one(hass, client):
    entry = await setup_entry(hass, client)
    living = entity_id(hass, entry, "select", LIVING_SOURCE)
    with pytest.raises(ServiceValidationError) as err:
        await select_option(hass, living, OTHER)
    assert err.value.translation_key == "another_sensor"
    assert err.value.translation_placeholders == {"zone": "Living"}
    assert client.calls == []
    assert hass.states.get(living).state == "VA0000000002"


async def test_temperature_source_choose_a_sensor_of_the_area(hass, client):
    entry = await setup_entry(hass, client)
    bedroom_area = place_zone(hass, entry, 2, "Camera")
    add_sensor(hass, entry, "sensor.camera", "Aqara bedroom", "18.1", area=bedroom_area)
    await refresh(hass, entry)
    bedroom = entity_id(hass, entry, "select", BEDROOM_SOURCE)
    client.calls.clear()

    await select_option(hass, bedroom, "Aqara bedroom (18.1 °C)")
    # Zone 2 had no correction: the choice starts the continuous shift, maps the
    # sensor in the options and sends its reading with its name.
    assert client.calls == [("patch_zone", 2, {"offset_mode": "setpoint"})]
    assert entry.options["room_temperatures"] == {"2": "sensor.camera"}
    assert 2 not in entry.options[CONF_ROOM_TEMP_PAUSED]
    assert client.room_temps == [(2, 18.1)]
    assert client.room_temp_sources == [(2, "Aqara bedroom")]
    assert hass.states.get(bedroom).state == "Aqara bedroom (18.1 °C)"

    # The new mapping follows the sensor without a reload.
    add_sensor(hass, entry, "sensor.camera", "Aqara bedroom", "18.4", area=bedroom_area)
    await hass.async_block_till_done()
    assert client.room_temps == [(2, 18.1), (2, 18.4)]


async def test_temperature_source_keeps_the_option_of_an_older_reading(hass, client):
    # The sensor reads 20.9 now, but the state still shows the label of the last
    # poll: the option maps back to the sensor until the next poll.
    entry = await setup_entry(hass, client)
    living_room = place_zone(hass, entry, 1, "Soggiorno")
    add_sensor(hass, entry, "sensor.soggiorno", "Soggiorno thermometer", "20.4", area=living_room)
    await refresh(hass, entry)
    living = entity_id(hass, entry, "select", LIVING_SOURCE)
    add_sensor(hass, entry, "sensor.soggiorno", "Soggiorno thermometer", "20.9", area=living_room)
    await hass.async_block_till_done()

    await select_option(hass, living, "Soggiorno thermometer (20.4 °C)")
    assert entry.options["room_temperatures"] == {"1": "sensor.soggiorno"}
    # Zone 1 already runs the continuous shift.
    assert not any(c[0] == "patch_zone" and "offset_mode" in c[2] for c in client.calls)
    assert hass.states.get(living).state == "Soggiorno thermometer (20.9 °C)"


async def test_temperature_source_choose_device(hass, client):
    set_room_sensor(hass, "21.4")
    entry = await setup_entry(hass, client, options={"room_temperatures": {"1": ROOM_SENSOR}})
    living = entity_id(hass, entry, "select", LIVING_SOURCE)
    assert hass.states.get(living).state == ROOM_LABEL

    await select_option(hass, living, LIVING_DEVICE)
    # The measuring device of the zone changes, the pushes pause and the reading
    # is cleared. The offset mode does not change.
    assert ("measuring", 1, "aaaa") in client.calls
    assert client.room_temp_clears == [1]
    assert entry.options[CONF_ROOM_TEMP_PAUSED] == [1]
    assert not any(call[0] == "patch_zone" for call in client.calls)
    assert hass.states.get(living).state == LIVING_DEVICE


async def test_temperature_source_device_choice_serializes_the_push(hass, client):
    # A push that started before the pause must not land after the clear of the
    # zone: the lock serializes the push with the pause and the clear.
    set_room_sensor(hass, "21.4")
    entry = await setup_entry(hass, client, options={"room_temperatures": {"1": ROOM_SENSOR}})
    living = entity_id(hass, entry, "select", LIVING_SOURCE)
    client.room_temp_events.clear()

    # A reading change starts a push that waits on the gate, holding the lock.
    client.room_temp_gate = asyncio.Event()
    set_room_sensor(hass, "21.9")
    await client.room_temp_reached.wait()

    # The device choice reaches the pause and the clear; with the lock it waits.
    select_task = asyncio.create_task(select_option(hass, living, LIVING_DEVICE))
    await asyncio.sleep(0.02)

    # Release the push. The lock keeps the clear behind it.
    client.room_temp_gate.set()
    await select_task
    await hass.async_block_till_done()
    client.room_temp_gate = None

    assert client.room_temp_events == [("push", 1, 21.9), ("clear", 1)]
    assert entry.options[CONF_ROOM_TEMP_PAUSED] == [1]


async def test_temperature_source_choose_the_paused_sensor(hass, client):
    set_room_sensor(hass, "21.8", entity=HALL_SENSOR)
    client.zones_json["declared"][1]["measuringDevice"] = "dddd"
    entry = await setup_entry(
        hass,
        client,
        options={"room_temperatures": {"2": HALL_SENSOR}, CONF_ROOM_TEMP_PAUSED: [2]},
    )
    bedroom = entity_id(hass, entry, "select", BEDROOM_SOURCE)
    assert hass.states.get(bedroom).state == "Bedroom (VA0000000004)"

    await select_option(hass, bedroom, "hall temperature (21.8 °C)")
    # The correction turns to continuous shift, the pause goes and the reading is
    # sent at once.
    assert ("patch_zone", 2, {"offset_mode": "setpoint"}) in client.calls
    assert 2 not in entry.options.get(CONF_ROOM_TEMP_PAUSED, [])
    assert client.room_temps == [(2, 21.8)]
    assert hass.states.get(bedroom).state == "hall temperature (21.8 °C)"


async def test_temperature_source_pause_survives_a_reload(hass, client):
    set_room_sensor(hass, "21.4")
    entry = await setup_entry(hass, client, options={"room_temperatures": {"1": ROOM_SENSOR}})
    living = entity_id(hass, entry, "select", LIVING_SOURCE)
    await select_option(hass, living, LIVING_DEVICE)
    assert entry.options[CONF_ROOM_TEMP_PAUSED] == [1]

    client.room_temps.clear()
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        await hass.config_entries.async_reload(entry.entry_id)
        await hass.async_block_till_done()
    # The paused zone sends no reading at start, and keeps its device option.
    assert client.room_temps == []
    living = entity_id(hass, entry, "select", LIVING_SOURCE)
    assert hass.states.get(living).state == LIVING_DEVICE


async def start_options(hass, entry, user_input=None):
    result = await hass.config_entries.options.async_init(entry.entry_id)
    assert result["step_id"] == "init"
    return await hass.config_entries.options.async_configure(result["flow_id"], user_input or {})


async def submit_rooms(hass, result, *pages):
    """Submit one page of the step Rooms per item of pages, in order."""
    for page in pages:
        assert result["type"] is FlowResultType.FORM
        assert result["step_id"] == "rooms"
        result = await hass.config_entries.options.async_configure(result["flow_id"], page)
        await hass.async_block_till_done()
    return result


def form_fields(result):
    return {str(key): key for key in result["data_schema"].schema}


def suggested(result, name):
    return (form_fields(result)[name].description or {}).get("suggested_value")


def sensor_choices(result):
    key = form_fields(result)["room_temperature"]
    return [o["value"] for o in result["data_schema"].schema[key].config["options"]]


async def test_rooms_step_has_a_page_for_each_heating_zone(hass, client):
    areas = ar.async_get(hass)
    living_room = areas.async_create("living").id
    entry = await setup_entry(hass, client, options={"room_temperatures": {"2": HALL_SENSOR}})
    bedroom_area = place_zone(hass, entry, 2, "Camera")
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        result = await start_options(hass, entry)
    # The zone without an area comes first. The hot water zone has no page.
    assert result["step_id"] == "rooms"
    assert result["description_placeholders"] == {"zone": "Living", "index": "1", "count": "2"}
    assert list(form_fields(result)) == ["area", "room_temperature", "correction"]
    # A zone with no area starts from the area of the same name, without case.
    assert suggested(result, "area") == living_room
    assert not suggested(result, "room_temperature")
    assert form_fields(result)["correction"].default() == "continuous_shift"
    result = await submit_rooms(hass, result, {"area": living_room, "correction": "continuous_shift"})
    assert result["description_placeholders"] == {"zone": "Bedroom", "index": "2", "count": "2"}
    assert suggested(result, "area") == bedroom_area
    assert suggested(result, "room_temperature") == HALL_SENSOR
    assert form_fields(result)["correction"].default() == "none"
    hass.config_entries.options.async_abort(result["flow_id"])


async def test_rooms_step_writes_the_areas_and_moves_the_devices(hass, client):
    areas = ar.async_get(hass)
    living_room = areas.async_create("Soggiorno").id
    bedroom_area = areas.async_create("Camera").id
    entry = await setup_entry(hass, client)
    client.calls.clear()
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        result = await start_options(hass, entry)
        result = await submit_rooms(
            hass, result,
            {"area": living_room, "correction": "continuous_shift"},
            {"area": bedroom_area, "correction": "none"},
        )
    assert result["type"] is FlowResultType.CREATE_ENTRY
    assert entry.options == {"windows": {}}
    assert zone_device(hass, entry, 1).area_id == living_room
    assert zone_device(hass, entry, 2).area_id == bedroom_area
    # The valve of zone 1 moves with it. The wall thermostat follows the hot
    # water zone, which has no area.
    assert node_device(hass, entry, "aaaa").area_id == living_room
    assert node_device(hass, entry, "cccc").area_id is None
    # The corrections did not change, and the daemon gets the areas as labels.
    assert sorted(c for c in client.calls if c[0] == "patch_zone") == [
        ("patch_zone", 1, {"ha_area": {"id": living_room, "name": "Soggiorno"}}),
        ("patch_zone", 2, {"ha_area": {"id": bedroom_area, "name": "Camera"}}),
    ]
    assert no_area_issue(hass, entry) is None


async def test_rooms_step_keeps_a_device_placed_elsewhere(hass, client):
    client.health_json["devices"].append(dict(client.health_json["devices"][0], device="VA0000000002", short="bbbb", label=""))
    areas = ar.async_get(hass)
    first = areas.async_create("Soggiorno").id
    second = areas.async_create("Salotto").id
    office = areas.async_create("Office").id
    entry = await setup_entry(hass, client)
    place_zone(hass, entry, 1, "Soggiorno")
    await refresh(hass, entry)
    assert node_device(hass, entry, "aaaa").area_id == first
    assert node_device(hass, entry, "bbbb").area_id == first
    dr.async_get(hass).async_update_device(node_device(hass, entry, "bbbb").id, area_id=office)

    with patch("custom_components.freedo.FreedoClient", return_value=client):
        result = await start_options(hass, entry)
        # Zone 2 has no area, so it comes first.
        result = await submit_rooms(
            hass, result, {"correction": "none"}, {"area": second, "correction": "continuous_shift"}
        )
    assert result["type"] is FlowResultType.CREATE_ENTRY
    # aaaa still had the previous area of the zone and moves; the user placed
    # bbbb in the office and it stays there.
    assert node_device(hass, entry, "aaaa").area_id == second
    assert node_device(hass, entry, "bbbb").area_id == office


async def test_rooms_step_lists_the_sensors_of_the_area_first(hass, client):
    set_room_sensor(hass, "21.4")
    entry = await setup_entry(hass, client)
    living_room = place_zone(hass, entry, 1, "Soggiorno")
    office = ar.async_get(hass).async_create("Office").id
    add_sensor(hass, entry, "sensor.soggiorno", "Soggiorno thermometer", "20.4", area=living_room)
    add_sensor(hass, entry, "sensor.office", "Office thermometer", "19.0", area=office)
    hass.states.async_set("sensor.attic", "15.0", {"device_class": "temperature", "friendly_name": "Attic"})
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        result = await start_options(hass, entry)
        # Zone 2 has no area, so it comes first; its page lists by name.
        assert sensor_choices(result) == ["sensor.attic", "sensor.office", "sensor.soggiorno"]
        result = await submit_rooms(hass, result, {"correction": "none"})
        assert sensor_choices(result) == ["sensor.soggiorno", "sensor.attic", "sensor.office"]
        key = form_fields(result)["room_temperature"]
        labels = [o["label"] for o in result["data_schema"].schema[key].config["options"]]
        assert labels == ["Soggiorno thermometer (20.4 °C)", "Attic (15.0)", "Office thermometer (19.0 °C), Office"]
        result = await submit_rooms(
            hass, result,
            {"area": living_room, "room_temperature": "sensor.soggiorno", "correction": "continuous_shift"},
        )
    assert result["type"] is FlowResultType.CREATE_ENTRY
    assert entry.options == {"windows": {}, "room_temperatures": {"1": "sensor.soggiorno"}}
    # The reload sends the reading with the sensor name.
    assert client.room_temps == [(1, 20.4)]
    assert client.room_temp_sources == [(1, "Soggiorno thermometer")]


async def test_rooms_step_falls_back_to_the_entity_selector(hass, client):
    # No sensor has the device class temperature and none is mapped.
    entry = await setup_entry(hass, client)
    result = await start_options(hass, entry)
    config = result["data_schema"].schema[form_fields(result)["room_temperature"]].config
    assert config["domain"] == ["sensor"]
    assert config["device_class"] == ["temperature"]
    hass.config_entries.options.async_abort(result["flow_id"])


async def test_rooms_step_starts_the_shift_for_a_new_sensor(hass, client):
    entry = await setup_entry(hass, client)
    add_sensor(hass, entry, HALL_SENSOR, "Hall", "20.1")
    client.calls.clear()
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        result = await start_options(hass, entry)
        # Zone 2 has no correction. A new sensor with the correction left at
        # none starts the continuous shift.
        result = await submit_rooms(
            hass, result,
            {"correction": "continuous_shift"},
            {"room_temperature": HALL_SENSOR, "correction": "none"},
        )
    assert result["type"] is FlowResultType.CREATE_ENTRY
    assert [c for c in client.calls if c[0] == "patch_zone"] == [
        ("patch_zone", 2, {"offset_mode": "setpoint"}),
    ]
    assert entry.options["room_temperatures"] == {"2": HALL_SENSOR}


async def test_rooms_step_ends_the_pause_of_a_new_sensor(hass, client):
    set_room_sensor(hass, "21.4")
    set_room_sensor(hass, "20.1", entity=HALL_SENSOR)
    entry = await setup_entry(
        hass,
        client,
        options={
            "room_temperatures": {"1": ROOM_SENSOR, "2": HALL_SENSOR},
            CONF_ROOM_TEMP_PAUSED: [1, 2],
        },
    )
    add_sensor(hass, entry, "sensor.soggiorno", "Soggiorno thermometer", "20.4")
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        result = await start_options(hass, entry)
        # Zone 1 gets a new sensor, zone 2 keeps its sensor and its pause.
        result = await submit_rooms(
            hass, result,
            {"room_temperature": "sensor.soggiorno", "correction": "continuous_shift"},
            {"room_temperature": HALL_SENSOR, "correction": "none"},
        )
    assert entry.options["room_temperatures"] == {"1": "sensor.soggiorno", "2": HALL_SENSOR}
    assert entry.options[CONF_ROOM_TEMP_PAUSED] == [2]
    assert client.room_temps == [(1, 20.4)]


async def test_rooms_step_writes_the_correction_in_one_request(hass, client):
    entry = await setup_entry(hass, client)
    client.calls.clear()
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        result = await start_options(hass, entry)
        result = await submit_rooms(
            hass, result,
            {"correction": "valve_calibration"},
            {"correction": "valve_calibration"},
        )
    assert result["type"] is FlowResultType.CREATE_ENTRY
    # Zone 1 leaves the shift for the calibration: the fake daemon refuses
    # auto_offset in mode setpoint, so both fields go together.
    assert [c for c in client.calls if c[0] == "patch_zone"] == [
        ("patch_zone", 1, {"offset_mode": "config", "auto_offset": True}),
        ("patch_zone", 2, {"auto_offset": True}),
    ]


async def test_rooms_step_shows_a_refused_correction(hass, client, caplog):
    entry = await setup_entry(hass, client)

    async def refuse(zone, **fields):
        raise FreedoApiError(503, "no installation")

    with patch("custom_components.freedo.FreedoClient", return_value=client):
        result = await start_options(hass, entry)
        client.patch_install_zone = refuse
        result = await hass.config_entries.options.async_configure(
            result["flow_id"], {"correction": "none"}
        )
    # The page of zone 1 comes back with the error, and the area is not written.
    assert result["step_id"] == "rooms"
    assert result["errors"] == {"base": "correction_failed"}
    assert result["description_placeholders"]["zone"] == "Living"
    assert "Correction of zone 1 refused" in caplog.text
    hass.config_entries.options.async_abort(result["flow_id"])


async def test_removing_the_sensor_clears_the_reading_and_stops_the_pushes(hass, client):
    set_room_sensor(hass, "21.4")
    set_room_sensor(hass, "20.1", entity=HALL_SENSOR)
    entry = await setup_entry(
        hass, client, options={"room_temperatures": {"1": ROOM_SENSOR, "2": HALL_SENSOR}}
    )
    assert sorted(client.room_temps) == [(1, 21.4), (2, 20.1)]
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        result = await start_options(hass, entry)
        # Zone 1 loses its sensor, zone 2 keeps it.
        result = await submit_rooms(
            hass, result,
            {"correction": "continuous_shift"},
            {"room_temperature": HALL_SENSOR, "correction": "none"},
        )
        assert result["type"] is FlowResultType.CREATE_ENTRY
        assert entry.options == {"windows": {}, "room_temperatures": {"2": HALL_SENSOR}}
        assert client.room_temp_clears == [1]

        client.room_temps.clear()
        set_room_sensor(hass, "22.0")
        await tick(hass, 16)
        assert client.room_temps == [(2, 20.1)]

        # No sensor left: the zone is cleared and nothing is pushed.
        result = await start_options(hass, entry)
        await submit_rooms(hass, result, {"correction": "continuous_shift"}, {"correction": "none"})
    assert entry.options == {"windows": {}}
    assert client.room_temp_clears == [1, 2]
    client.room_temps.clear()
    set_room_sensor(hass, "22.5", entity=HALL_SENSOR)
    await tick(hass, 16)
    assert client.room_temps == []


async def test_a_refused_clear_does_not_block_the_options(hass, client, caplog):
    set_room_sensor(hass, "21.4")
    entry = await setup_entry(hass, client, options={"room_temperatures": {"1": ROOM_SENSOR}})
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        result = await start_options(hass, entry)
        client.room_temp_errors = [FreedoConnectionError("daemon down")]
        result = await submit_rooms(
            hass, result, {"correction": "continuous_shift"}, {"correction": "none"}
        )
    assert result["type"] is FlowResultType.CREATE_ENTRY
    assert entry.options == {"windows": {}}
    assert "Room temperature release for zone 1 failed" in caplog.text


async def test_options_keep_the_sensors_while_the_entry_is_not_loaded(hass, client):
    entry = await setup_entry(hass, client, options={"room_temperatures": {"1": ROOM_SENSOR}})
    await hass.config_entries.async_unload(entry.entry_id)
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        result = await start_options(hass, entry, {"outdoor_entity": "sensor.outdoor"})
        await hass.async_block_till_done()
    assert result["type"] is FlowResultType.CREATE_ENTRY
    assert entry.options == {
        "windows": {},
        "outdoor_entity": "sensor.outdoor",
        "room_temperatures": {"1": ROOM_SENSOR},
    }
    assert client.room_temp_clears == []


async def test_options_of_the_old_temperature_step_carry_over(hass, client):
    # The options as the step "Temperature sensor of each zone" saved them: the
    # step Rooms keeps the same keys, so they need no migration.
    set_room_sensor(hass, "21.4")
    old = {
        "windows": {},
        "outdoor_entity": "sensor.outdoor",
        "room_temperatures": {"1": ROOM_SENSOR},
        CONF_ROOM_TEMP_PAUSED: [1],
    }
    entry = await setup_entry(hass, client, options=old)
    # Zone 1 is paused: no push at start.
    assert client.room_temps == []
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        result = await start_options(hass, entry, {"outdoor_entity": "sensor.outdoor"})
        # The mapped sensor has no device class, and stays the first choice.
        assert suggested(result, "room_temperature") == ROOM_SENSOR
        assert sensor_choices(result)[0] == ROOM_SENSOR
        result = await submit_rooms(
            hass, result,
            {"room_temperature": ROOM_SENSOR, "correction": "continuous_shift"},
            {"correction": "none"},
        )
    assert result["type"] is FlowResultType.CREATE_ENTRY
    assert entry.options == old
    assert client.room_temps == []


# --- preset select ---

PRESET = "select.freedo_profile"


async def test_preset_options_and_current(hass, client):
    await setup_entry(hass, client)
    state = hass.states.get(PRESET)
    assert state.attributes["options"] == ["Winter", "Summer"]
    assert state.state == "Winter"


async def test_preset_choice_activates_and_polls(hass, client):
    await setup_entry(hass, client)
    reads = client.preset_reads
    await select_option(hass, PRESET, "Summer")
    # The daemon takes the id of the chosen name and the poll shows it active.
    assert client.calls == [("activate_preset", 2)]
    assert client.preset_reads == reads + 1
    assert hass.states.get(PRESET).state == "Summer"


async def test_preset_follows_the_list_without_a_reload(hass, client):
    entry = await setup_entry(hass, client)
    client.presets_json["presets"][1]["name"] = "Summer 2026"
    await refresh(hass, entry)
    assert hass.states.get(PRESET).attributes["options"] == ["Winter", "Summer 2026"]

    client.presets_json["presets"].append({"id": 3, "name": "Autumn", "active": False})
    await refresh(hass, entry)
    assert hass.states.get(PRESET).attributes["options"] == ["Winter", "Summer 2026", "Autumn"]

    client.presets_json["presets"] = [
        p for p in client.presets_json["presets"] if p["id"] != 2
    ]
    await refresh(hass, entry)
    assert hass.states.get(PRESET).attributes["options"] == ["Winter", "Autumn"]


async def test_no_preset_select_on_a_daemon_without_the_route(hass, client):
    client.presets_json = None
    entry = await setup_entry(hass, client)
    assert not has_entity(hass, entry, "select", "preset")


# --- external room sensor ---


def external_room_sensor(hass, entry, zone):
    return hass.states.get(entity_id(hass, entry, "sensor", f"zone{zone}_external_room_sensor"))


async def test_external_room_sensor_shows_the_daemon_view(hass, client):
    entry = await setup_entry(hass, client)
    registry = er.async_get(hass)

    active = external_room_sensor(hass, entry, 1)
    assert active.state == "active"
    assert active.attributes["offset_mode"] == "setpoint"
    assert active.attributes["temperature_c"] == 21.4
    assert active.attributes["age_sec"] == 120
    assert active.attributes["device_temperature_c"] == 20.9
    assert active.attributes["shift_c"] == 0.2
    assert active.attributes["wanted_shift_c"] == 0.4
    assert registry.async_get(active.entity_id).entity_category == "diagnostic"

    # Zone 2 is in mode config: the daemon takes no reading, and the sensor says why.
    wrong_mode = external_room_sensor(hass, entry, 2)
    assert wrong_mode.state == "wrong_offset_mode"
    assert wrong_mode.attributes["offset_mode"] == "config"
    assert wrong_mode.attributes["temperature_c"] is None

    # The hot water zone has none.
    assert not has_entity(hass, entry, "sensor", "zone9_external_room_sensor")


@pytest.mark.parametrize(
    ("view", "state"),
    [
        ({"tempC": None, "at": None, "ageSec": None, "inUse": False}, "no_reading"),
        ({"inUse": False, "stale": True, "ageSec": 1900}, "stale"),
        ({"inUse": True, "stale": False}, "active"),
    ],
)
async def test_external_room_sensor_shows_the_cause(hass, client, view, state):
    entry = await setup_entry(hass, client)
    client.zones_json["declared"][0]["externalTemp"].update(view)
    await refresh(hass, entry)
    assert external_room_sensor(hass, entry, 1).state == state


async def test_external_room_sensor_follows_the_offset_mode(hass, client):
    entry = await setup_entry(hass, client)
    # Zone 1 leaves mode setpoint: the daemon forgets the reading.
    client.install_json["zones"][0]["offset_mode"] = "dbg2"
    client.zones_json["declared"][0]["externalTemp"].update(
        {"tempC": None, "at": None, "ageSec": None, "inUse": False, "wantedC": None}
    )
    await refresh(hass, entry)
    assert external_room_sensor(hass, entry, 1).state == "wrong_offset_mode"


async def test_no_external_room_sensor_on_a_daemon_without_the_view(hass, client):
    for zone in client.zones_json["declared"]:
        zone.pop("externalTemp", None)
    entry = await setup_entry(hass, client)
    for zone in (1, 2):
        assert not has_entity(hass, entry, "sensor", f"zone{zone}_external_room_sensor")


# --- offline schedule switch ---


async def test_offline_schedule_switch(hass, client):
    await setup_entry(hass, client)
    assert hass.states.get(OFFLINE_SCHEDULE).state == "on"

    await hass.services.async_call(
        "switch", "turn_off", {ATTR_ENTITY_ID: OFFLINE_SCHEDULE}, blocking=True
    )
    await hass.async_block_till_done()
    assert client.calls == [("offline_schedule", False)]
    assert hass.states.get(OFFLINE_SCHEDULE).state == "off"

    await hass.services.async_call(
        "switch", "turn_on", {ATTR_ENTITY_ID: OFFLINE_SCHEDULE}, blocking=True
    )
    await hass.async_block_till_done()
    assert client.calls[-1] == ("offline_schedule", True)
    assert hass.states.get(OFFLINE_SCHEDULE).state == "on"


async def test_offline_schedule_unknown_without_install(hass, client):
    # 503: the daemon holds no installation.
    client.settings_error = FreedoApiError(503, "no installation")
    await setup_entry(hass, client)
    assert hass.states.get(OFFLINE_SCHEDULE).state == "unknown"


# --- overlay end rule ---


async def call_freedo(hass, service, data):
    await hass.services.async_call(DOMAIN, service, data, blocking=True)
    await hass.async_block_till_done()


@pytest.mark.parametrize(
    ("data", "expected"),
    [
        ({"temperature": 21}, ("setpoint", 2, "manual", None)),
        (
            {"temperature": 21, "end_rule": "timer", "duration": {"hours": 1, "minutes": 30}},
            ("setpoint", 2, "timer", 5400),
        ),
        ({"temperature": 21, "end_rule": "until_next"}, ("setpoint", 2, "until_next", None)),
        ({"hvac_mode": "off"}, ("off", 2, "manual", None)),
        (
            {"hvac_mode": "off", "end_rule": "timer", "duration": {"minutes": 45}},
            ("off", 2, "timer", 2700),
        ),
        ({"hvac_mode": "off", "end_rule": "until_next"}, ("off", 2, "until_next", None)),
        (
            {"temperature": 21, "end_rule": "until_auto_change"},
            ("setpoint", 2, "until_auto_change", None),
        ),
        (
            {"hvac_mode": "off", "end_rule": "until_auto_change"},
            ("off", 2, "until_auto_change", None),
        ),
    ],
)
async def test_set_overlay_end_rule(hass, client, data, expected):
    await setup_entry(hass, client)
    await call_freedo(hass, "set_overlay", {ATTR_ENTITY_ID: BEDROOM, **data})
    assert client.end_rules == [expected]
    state = hass.states.get(BEDROOM)
    if expected[0] == "off":
        assert state.state == HVACMode.OFF
    else:
        assert state.state == HVACMode.HEAT
        assert state.attributes[ATTR_TEMPERATURE] == 21


@pytest.mark.parametrize(
    "data",
    [
        {},
        {"hvac_mode": "off", "temperature": 21},
        {"temperature": 21, "end_rule": "timer"},
        {"temperature": 21, "duration": {"hours": 1}},
        {"temperature": 21, "end_rule": "timer", "duration": {"hours": 0}},
        {"temperature": 21, "end_rule": "timer", "duration": {"days": 8}},
        {"temperature": 21, "end_rule": "until_auto_change", "duration": {"hours": 1}},
        {"hvac_mode": "off", "end_rule": "until_auto_change", "duration": {"hours": 1}},
        {"temperature": 30},
        {"temperature": 4},
        {"temperature": 21, "end_rule": "tomorrow"},
        {"hvac_mode": "auto"},
    ],
)
async def test_set_overlay_rejects_bad_input(hass, client, data):
    await setup_entry(hass, client)
    with pytest.raises((ServiceValidationError, vol.Invalid)):
        await call_freedo(hass, "set_overlay", {ATTR_ENTITY_ID: BEDROOM, **data})
    assert client.calls == []


async def test_overlay_attributes_show_the_end_rule(hass, client):
    await setup_entry(hass, client)
    # Zone 1 runs a boost that ends at a set instant.
    attrs = hass.states.get(LIVING).attributes
    assert attrs["overlay"] == "boost"
    assert attrs["overlay_ends_at"] == "2026-01-01T00:30:00Z"
    assert hass.states.get(BEDROOM).attributes["overlay"] == "auto"


async def test_overlay_rule_attribute(hass, client):
    await setup_entry(hass, client)
    # The fixture is the view of a daemon that sends no rule: the kind stands
    # in when it names a rule, and a boost has none.
    assert hass.states.get(LIVING).attributes["overlay_rule"] is None
    assert hass.states.get(BEDROOM).attributes["overlay_rule"] is None
    assert hass.states.get(HOT_WATER).attributes["overlay_rule"] == "manual"


async def test_overlay_rule_follows_the_daemon(hass, client):
    entry = await setup_entry(hass, client)
    living, bedroom, hot_water = (z["target"] for z in client.zones_json["declared"])
    living["overlay"]["rule"] = "timer"
    bedroom["overlay"] = {
        "kind": "until_auto_change", "rule": "until_auto_change", "on": True,
        "setpointC": 21.0, "endsAt": None,
    }
    hot_water["overlay"]["rule"] = "until_next"
    await refresh(hass, entry)

    assert hass.states.get(LIVING).attributes["overlay_rule"] == "timer"
    # kind keeps its meaning, endsAt is null without a deadline.
    bedroom_attrs = hass.states.get(BEDROOM).attributes
    assert bedroom_attrs["overlay"] == "until_auto_change"
    assert bedroom_attrs["overlay_rule"] == "until_auto_change"
    assert bedroom_attrs["overlay_ends_at"] is None
    assert "overlay_remaining" not in bedroom_attrs
    water_attrs = hass.states.get(HOT_WATER).attributes
    assert water_attrs["overlay"] == "manual"
    assert water_attrs["overlay_rule"] == "until_next"

    # The overlay ends: the rule goes with it.
    bedroom["overlay"] = None
    await refresh(hass, entry)
    assert hass.states.get(BEDROOM).attributes["overlay_rule"] is None


# --- a zone with no target: the temperature control ---

# The target of a zone that runs without a program and holds no 6200.
NO_PROGRAM = {
    "targetC": None, "source": "program", "programC": None, "programOff": False,
    "hasProgram": False, "boost": False, "known": True, "on": True, "overlay": None,
}
# The declared zones of the fixture: living, bedroom, hot water.
BEDROOM_ZONE, HOT_WATER_ZONE = 1, 2


async def water_call(hass, service, data=None):
    await hass.services.async_call(
        "water_heater", service, {ATTR_ENTITY_ID: HOT_WATER, **(data or {})}, blocking=True
    )
    await hass.async_block_till_done()


async def test_hot_water_with_no_target_offers_the_control(hass, client):
    client.zones_json["declared"][HOT_WATER_ZONE]["target"] = dict(NO_PROGRAM)
    await setup_entry(hass, client)

    state = hass.states.get(HOT_WATER)
    assert state.state == "auto"
    # The frontend draws the control only for a target that is not null: the
    # entity shows the lowest temperature and says that it is no target.
    assert state.attributes[ATTR_TEMPERATURE] == 30.0
    assert state.attributes["has_program"] is False
    assert state.attributes["program_temperature"] is None
    assert state.attributes["reported_setpoint"] is None
    assert "No target is set" in state.attributes["target_note"]

    # heat does not send the placeholder.
    for service, data in (("set_operation_mode", {"operation_mode": "heat"}), ("turn_on", {})):
        with pytest.raises(ServiceValidationError) as err:
            await water_call(hass, service, data)
        assert err.value.translation_domain == DOMAIN
        assert err.value.translation_key == "hot_water_temperature_required"
    assert client.calls == []

    # A temperature on the control is written once and selects heat.
    await water_call(hass, "set_temperature", {ATTR_TEMPERATURE: 45})
    assert client.calls == [("setpoint", 9, 45.0)]
    state = hass.states.get(HOT_WATER)
    assert state.state == "heat"
    assert state.attributes[ATTR_TEMPERATURE] == 45.0
    assert state.attributes["target_note"] is None


@pytest.mark.parametrize(
    ("target", "reported", "want", "note"),
    [
        # The daemon target outranks the program temperature and the boiler.
        (dict(NO_PROGRAM, targetC=48.0, programC=55.0, hasProgram=True, source="overlay",
              overlay={"kind": "manual", "on": True, "setpointC": 48.0, "endsAt": None}),
         52.0, 48.0, None),
        # The program temperature outranks the boiler.
        (dict(NO_PROGRAM, programC=55.0, hasProgram=True), 52.0, 55.0, None),
        (dict(NO_PROGRAM), 52.0, 52.0, "boiler reports"),
        (dict(NO_PROGRAM), None, 30.0, "No target is set"),
        # A value outside the range of a setpoint, and a zone that is off, count
        # as no value.
        (dict(NO_PROGRAM), 70.0, 30.0, "No target is set"),
        (dict(NO_PROGRAM, on=False), 52.0, 30.0, "No target is set"),
    ],
)
async def test_hot_water_target_order(hass, client, target, reported, want, note):
    client.zones_json["declared"][HOT_WATER_ZONE]["target"] = target
    if reported is not None:
        client.boiler_json["DHWSetpointC"] = reported
    await setup_entry(hass, client)

    attrs = hass.states.get(HOT_WATER).attributes
    assert attrs[ATTR_TEMPERATURE] == want
    if note is None:
        assert attrs["target_note"] is None
    else:
        assert note in attrs["target_note"]


async def test_hot_water_heat_keeps_the_setpoint_the_boiler_reports(hass, client):
    client.zones_json["declared"][HOT_WATER_ZONE]["target"] = dict(NO_PROGRAM)
    client.boiler_json["DHWSetpointC"] = 52.0
    await setup_entry(hass, client)

    attrs = hass.states.get(HOT_WATER).attributes
    assert attrs["reported_setpoint"] == 52.0
    await water_call(hass, "set_operation_mode", {"operation_mode": "heat"})
    assert client.calls == [("setpoint", 9, 52.0)]


async def test_room_with_no_target_offers_the_control(hass, client):
    client.zones_json["declared"][BEDROOM_ZONE]["target"] = dict(NO_PROGRAM)
    await setup_entry(hass, client)

    state = hass.states.get(BEDROOM)
    assert state.attributes[ATTR_TEMPERATURE] == 5.0
    assert state.attributes["has_program"] is False
    assert "No target is set" in state.attributes["target_note"]

    with pytest.raises(ServiceValidationError) as err:
        await call(hass, SERVICE_SET_HVAC_MODE, {ATTR_ENTITY_ID: BEDROOM, ATTR_HVAC_MODE: HVACMode.HEAT})
    assert err.value.translation_domain == DOMAIN
    assert err.value.translation_key == "zone_temperature_required"
    assert client.calls == []

    await call(hass, SERVICE_SET_TEMPERATURE, {ATTR_ENTITY_ID: BEDROOM, ATTR_TEMPERATURE: 21})
    assert client.calls == [("setpoint", 2, 21.0)]
    state = hass.states.get(BEDROOM)
    assert state.state == "heat"
    assert state.attributes[ATTR_TEMPERATURE] == 21.0
    assert state.attributes["target_note"] is None


async def test_room_with_a_program_shows_no_placeholder(hass, client):
    # The zone is off for now. Its program holds 17 C, so heat has a value to
    # write and the entity keeps the control hidden as before.
    client.zones_json["declared"][BEDROOM_ZONE]["target"] = dict(
        NO_PROGRAM, programC=17.0, hasProgram=True, source="overlay", on=False,
        overlay={"kind": "until_next", "on": False, "setpointC": None, "endsAt": None},
    )
    await setup_entry(hass, client)

    attrs = hass.states.get(BEDROOM).attributes
    assert attrs.get(ATTR_TEMPERATURE) is None
    assert attrs["target_note"] is None
    assert attrs["has_program"] is True
    await call(hass, SERVICE_SET_HVAC_MODE, {ATTR_ENTITY_ID: BEDROOM, ATTR_HVAC_MODE: HVACMode.HEAT})
    assert client.calls == [("setpoint", 2, 17.0)]


# --- device controls of a valve ---


async def test_child_lock_switch(hass, client):
    entry = await setup_entry(hass, client)
    lock = entity_id(hass, entry, "switch", "aaaa_child_lock")
    assert hass.states.get(lock).state == "off"

    await hass.services.async_call("switch", "turn_on", {ATTR_ENTITY_ID: lock}, blocking=True)
    await hass.async_block_till_done()
    assert client.calls == [("child_lock", "aaaa", True)]
    # The valve has not confirmed: the entity keeps the requested state.
    await refresh(hass, entry)
    assert hass.states.get(lock).state == "on"

    client.nodes_json[0]["ChildLock"] = True
    await refresh(hass, entry)
    assert hass.states.get(lock).state == "on"

    # Once the daemon reports the lock, it follows the daemon again.
    client.nodes_json[0]["ChildLock"] = False
    await refresh(hass, entry)
    assert hass.states.get(lock).state == "off"

    await hass.services.async_call("switch", "turn_off", {ATTR_ENTITY_ID: lock}, blocking=True)
    assert client.calls[-1] == ("child_lock", "aaaa", False)


async def test_child_lock_only_where_the_node_serves_it(hass, client):
    # The node view serves ChildLock as null on a device that is not a valve.
    client.nodes_json[0]["ChildLock"] = None
    entry = await setup_entry(hass, client)
    assert not has_entity(hass, entry, "switch", "aaaa_child_lock")
    # The thermostat has none either.
    assert not has_entity(hass, entry, "switch", "cccc_child_lock")


async def test_valve_buttons(hass, client):
    entry = await setup_entry(hass, client)
    for key, call_name in (("identify", "identify"), ("calibrate", "calibrate")):
        button = entity_id(hass, entry, "button", f"aaaa_{key}")
        await hass.services.async_call(
            "button", "press", {ATTR_ENTITY_ID: button}, blocking=True
        )
        assert client.calls[-1] == (call_name, "aaaa")
    # A wall thermostat has neither.
    assert not has_entity(hass, entry, "button", "cccc_identify")
    assert not has_entity(hass, entry, "button", "cccc_calibrate")


async def test_valve_outside_a_zone_gets_no_controls_that_need_a_zone(hass, client):
    # The daemon answers 400 "id required" to identify, child lock and display
    # writes of a valve that belongs to no zone.
    client.health_json["devices"].append(
        {"device": "VA0000000009", "short": "ffff", "kind": "valve", "label": "",
         "batteryMV": 2800, "lowBattery": False, "fault": False, "faultWord": 0,
         "offline": False, "lastSeen": None, "fallbackMode": 0, "commsWarnings": None}
    )
    client.nodes_json.append(
        {"Short": "ffff", "TempC": 0, "SetpointC": 0, "Mode": "", "LastSeenUnix": 0,
         "Telemetry": {}, "Display": {"orientation": "vertical", "notification": False},
         "ChildLock": False}
    )
    entry = await setup_entry(hass, client)
    for platform, key in (
        ("button", "identify"),
        ("switch", "child_lock"),
        ("switch", "notification"),
        ("select", "orientation"),
    ):
        assert has_entity(hass, entry, platform, f"aaaa_{key}"), key
        assert not has_entity(hass, entry, platform, f"ffff_{key}"), key
    # The calibration route needs no id.
    assert has_entity(hass, entry, "button", "ffff_calibrate")


async def test_button_error_is_a_home_assistant_error(hass, client):
    entry = await setup_entry(hass, client)
    button = entity_id(hass, entry, "button", "aaaa_calibrate")

    async def refuse(node):
        raise FreedoApiError(400, "aaaa is a thermostat: only a valve calibrates")

    client.calibrate = refuse
    with pytest.raises(HomeAssistantError, match="only a valve calibrates"):
        await hass.services.async_call("button", "press", {ATTR_ENTITY_ID: button}, blocking=True)


async def test_calibration_sensor(hass, client):
    entry = await setup_entry(hass, client)
    sensor = entity_id(hass, entry, "sensor", "aaaa_calibration")
    state = hass.states.get(sensor)
    assert state.state == "done"
    assert state.attributes["error_code"] == 0

    client.nodes_json[0]["Telemetry"].update(calib_state="3", calib_error="4")
    await refresh(hass, entry)
    state = hass.states.get(sensor)
    assert (state.state, state.attributes["error_code"]) == ("failed", 4)

    # The valve has not reported a calibration yet.
    client.nodes_json[0]["Telemetry"] = {"temp_C": "20.90C"}
    await refresh(hass, entry)
    state = hass.states.get(sensor)
    assert state.state == "unknown"
    assert state.attributes["error_code"] is None


async def test_offset_number_setpoint_mode(hass, client):
    entry = await setup_entry(hass, client)
    offset = entity_id(hass, entry, "number", "zone1_offset")
    # Zone 1 corrects its temperature by +0.5 C itself.
    assert float(hass.states.get(offset).state) == 0.5

    await hass.services.async_call(
        "number", "set_value", {ATTR_ENTITY_ID: offset, "value": -0.7}, blocking=True
    )
    assert client.calls == [("offset", 1, -0.7)]


async def test_offset_number_in_config_mode(hass, client):
    # Zone 2 states no measuring device: the route has nothing to write.
    entry = await setup_entry(hass, client)
    assert not has_entity(hass, entry, "number", "zone2_offset")
    await hass.config_entries.async_unload(entry.entry_id)

    # With one, the correction goes to the device and the daemon reports none.
    client.zones_json["declared"][1]["measuringDevice"] = "dddd"
    entry = await setup_entry(hass, client)
    offset = entity_id(hass, entry, "number", "zone2_offset")
    assert hass.states.get(offset).state == "unknown"
    await hass.services.async_call(
        "number", "set_value", {ATTR_ENTITY_ID: offset, "value": 0.3}, blocking=True
    )
    assert client.calls == [("offset", 2, 0.3)]
    assert float(hass.states.get(offset).state) == 0.3


async def test_offset_number_in_config_mode_survives_a_reload(hass, client):
    # The daemon reports no correction in mode config: the entity restores the
    # last value set from Home Assistant.
    client.zones_json["declared"][1]["measuringDevice"] = "dddd"
    entry = await setup_entry(hass, client)
    offset = entity_id(hass, entry, "number", "zone2_offset")
    await hass.services.async_call(
        "number", "set_value", {ATTR_ENTITY_ID: offset, "value": 0.3}, blocking=True
    )
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        await hass.config_entries.async_reload(entry.entry_id)
        await hass.async_block_till_done()
    assert float(hass.states.get(offset).state) == 0.3


async def test_offset_number_refuses_beyond_the_daemon_limit(hass, client):
    entry = await setup_entry(hass, client)
    offset = entity_id(hass, entry, "number", "zone1_offset")
    with pytest.raises(ServiceValidationError):
        await hass.services.async_call(
            "number", "set_value", {ATTR_ENTITY_ID: offset, "value": 10.5}, blocking=True
        )
    assert client.calls == []


# --- zone settings ---


async def turn(hass, service, target):
    await hass.services.async_call("switch", service, {ATTR_ENTITY_ID: target}, blocking=True)
    await hass.async_block_till_done()


async def test_early_start_and_open_window_switches(hass, client):
    entry = await setup_entry(hass, client)
    early = entity_id(hass, entry, "switch", "zone1_early_start")
    window = entity_id(hass, entry, "switch", "zone1_open_window_detection")
    assert hass.states.get(early).state == "off"
    assert hass.states.get(window).state == "on"

    await turn(hass, "turn_on", early)
    await turn(hass, "turn_off", window)
    assert client.calls == [
        ("patch_zone", 1, {"early_start": True}),
        ("patch_zone", 1, {"open_window_detection": False}),
    ]
    # The entities show the row the daemon answered.
    assert hass.states.get(early).state == "on"
    assert hass.states.get(window).state == "off"
    # The other zone keeps its own values.
    assert hass.states.get(entity_id(hass, entry, "switch", "zone2_early_start")).state == "on"


async def test_zone_settings_only_for_heating_zones_with_a_row(hass, client):
    entry = await setup_entry(hass, client)
    for key in ("early_start", "open_window_detection"):
        assert not has_entity(hass, entry, "switch", f"zone9_{key}")
    assert not has_entity(hass, entry, "number", "zone9_open_window_duration_sec")
    assert not has_entity(hass, entry, "select", "zone9_device_overlay_end")
    assert not has_entity(hass, entry, "select", "zone9_offset_mode")
    await hass.config_entries.async_unload(entry.entry_id)

    # The daemon holds no installation (503 on GET /api/install/zones).
    client.install_json = {}
    entry = await setup_entry(hass, client)
    assert not has_entity(hass, entry, "switch", "zone1_early_start")
    assert not has_entity(hass, entry, "select", "zone1_device_overlay_end")
    assert not has_entity(hass, entry, "select", "zone1_offset_mode")


async def test_open_window_duration_number(hass, client):
    entry = await setup_entry(hass, client)
    duration = entity_id(hass, entry, "number", "zone1_open_window_duration_sec")
    assert float(hass.states.get(duration).state) == 15
    await hass.services.async_call(
        "number", "set_value", {ATTR_ENTITY_ID: duration, "value": 20}, blocking=True
    )
    assert client.calls == [("patch_zone", 1, {"open_window_duration_sec": 1200})]
    assert float(hass.states.get(duration).state) == 20
    with pytest.raises(ServiceValidationError):
        await hass.services.async_call(
            "number", "set_value", {ATTR_ENTITY_ID: duration, "value": 1093}, blocking=True
        )
    assert len(client.calls) == 1


async def test_away_temperature_number(hass, client):
    entry = await setup_entry(hass, client)
    away = entity_id(hass, entry, "number", "zone1_away_temperature_c")
    state = hass.states.get(away)
    assert float(state.state) == 21.5
    assert state.attributes["device_class"] == "temperature"
    assert state.attributes["unit_of_measurement"] == "°C"
    assert (state.attributes["min"], state.attributes["max"], state.attributes["step"]) == (5, 25, 0.1)
    assert er.async_get(hass).async_get(away).entity_category == "config"
    # Zone 2 holds no value: it is off in away, which the minimum shows.
    unset = entity_id(hass, entry, "number", "zone2_away_temperature_c")
    assert float(hass.states.get(unset).state) == 5

    await hass.services.async_call(
        "number", "set_value", {ATTR_ENTITY_ID: unset, "value": 18.5}, blocking=True
    )
    await hass.services.async_call(
        "number", "set_value", {ATTR_ENTITY_ID: away, "value": 5}, blocking=True
    )
    assert client.calls == [
        ("patch_zone", 2, {"away_temperature_c": 18.5}),
        ("patch_zone", 1, {"away_temperature_c": 5.0}),
    ]
    assert float(hass.states.get(unset).state) == 18.5
    assert float(hass.states.get(away).state) == 5
    for value in (4.9, 25.1):
        with pytest.raises(ServiceValidationError):
            await hass.services.async_call(
                "number", "set_value", {ATTR_ENTITY_ID: away, "value": value}, blocking=True
            )
    assert len(client.calls) == 2


async def test_away_temperature_only_where_the_daemon_serves_it(hass, client):
    entry = await setup_entry(hass, client)
    # The hot water zone has no away temperature.
    assert not has_entity(hass, entry, "number", "zone9_away_temperature_c")
    await hass.config_entries.async_unload(entry.entry_id)

    # A daemon without the field sends no key in the zone row.
    for row in client.install_json["zones"]:
        del row["away_temperature_c"]
    entry = await setup_entry(hass, client)
    assert not has_entity(hass, entry, "number", "zone1_away_temperature_c")
    assert has_entity(hass, entry, "number", "zone1_open_window_duration_sec")


async def test_away_temperature_error_is_a_home_assistant_error(hass, client):
    entry = await setup_entry(hass, client)
    away = entity_id(hass, entry, "number", "zone1_away_temperature_c")

    async def refuse(zone, **fields):
        raise FreedoApiError(400, "away_temperature_c must be within 5..25 C")

    client.patch_install_zone = refuse
    with pytest.raises(HomeAssistantError, match="within 5..25"):
        await hass.services.async_call(
            "number", "set_value", {ATTR_ENTITY_ID: away, "value": 20}, blocking=True
        )
    assert float(hass.states.get(away).state) == 21.5


async def test_device_overlay_end_rule(hass, client):
    entry = await setup_entry(hass, client)
    rule = entity_id(hass, entry, "select", "zone1_device_overlay_end")
    timer = entity_id(hass, entry, "number", "zone1_device_overlay_end_sec")
    assert hass.states.get(rule).state == "until_next"
    # The timer length applies to the rule timer alone.
    assert hass.states.get(timer).state == "unavailable"

    async def select(option):
        await hass.services.async_call(
            "select", "select_option", {ATTR_ENTITY_ID: rule, "option": option}, blocking=True
        )
        await hass.async_block_till_done()

    await select("timer")
    # The daemon refuses a timer without a length: the first one is one hour.
    assert client.calls == [
        ("patch_zone", 1, {"device_overlay_end": "timer", "device_overlay_end_sec": 3600})
    ]
    assert hass.states.get(rule).state == "timer"
    assert float(hass.states.get(timer).state) == 60

    await hass.services.async_call(
        "number", "set_value", {ATTR_ENTITY_ID: timer, "value": 90}, blocking=True
    )
    assert client.calls[-1] == ("patch_zone", 1, {"device_overlay_end_sec": 5400})
    assert float(hass.states.get(timer).state) == 90

    await select("manual")
    assert client.calls[-1] == ("patch_zone", 1, {"device_overlay_end": "manual"})
    assert hass.states.get(rule).state == "manual"
    assert hass.states.get(timer).state == "unavailable"


async def test_zone_already_on_timer_keeps_its_length(hass, client):
    entry = await setup_entry(hass, client)
    # Zone 2 runs a 30 minute timer rule.
    timer = entity_id(hass, entry, "number", "zone2_device_overlay_end_sec")
    assert float(hass.states.get(timer).state) == 30


async def test_zone_setting_error_is_a_home_assistant_error(hass, client):
    entry = await setup_entry(hass, client)
    early = entity_id(hass, entry, "switch", "zone1_early_start")

    async def refuse(zone, **fields):
        raise FreedoApiError(400, "zone 1 is not a heating zone")

    client.patch_install_zone = refuse
    with pytest.raises(HomeAssistantError, match="not a heating zone"):
        await turn(hass, "turn_on", early)
    assert hass.states.get(early).state == "off"


OFFSET_MODE_ENABLED = [("select", "zone1_offset_mode"), ("select", "zone2_offset_mode")]
AUTO_OFFSET_ENABLED = [("switch", "zone1_auto_offset"), ("switch", "zone2_auto_offset")]


async def test_old_correction_entities_are_disabled_diagnostics(hass, client):
    # A new install gets the select Temperature offset mode and the switch
    # Automatic correction disabled; select Room temperature correction
    # replaces them.
    entry = await setup_entry(hass, client)
    registry = er.async_get(hass)
    for platform, key in [*OFFSET_MODE_ENABLED, *AUTO_OFFSET_ENABLED]:
        found = entity_id(hass, entry, platform, key)
        assert registry.async_get(found).disabled_by is er.RegistryEntryDisabler.INTEGRATION
        assert registry.async_get(found).entity_category == "diagnostic"
        assert hass.states.get(found) is None


async def test_old_correction_entities_stay_enabled_after_an_upgrade(hass, client):
    # An install that had them keeps them enabled, so its automations work.
    entry = await setup_entry(hass, client, enabled=OFFSET_MODE_ENABLED + AUTO_OFFSET_ENABLED)
    assert hass.states.get(entity_id(hass, entry, "select", "zone1_offset_mode")).state == "setpoint"
    assert hass.states.get(entity_id(hass, entry, "switch", "zone1_auto_offset")).state == "off"


async def test_offset_mode_select(hass, client):
    entry = await setup_entry(hass, client, enabled=OFFSET_MODE_ENABLED)
    living = entity_id(hass, entry, "select", "zone1_offset_mode")
    bedroom = entity_id(hass, entry, "select", "zone2_offset_mode")
    assert hass.states.get(living).state == "setpoint"
    assert hass.states.get(bedroom).state == "config"
    assert hass.states.get(living).attributes["options"] == list(OFFSET_MODES)
    # The hot water zone has no correction.
    assert not has_entity(hass, entry, "select", "zone9_offset_mode")

    async def select(entity, option):
        await hass.services.async_call(
            "select", "select_option", {ATTR_ENTITY_ID: entity, "option": option}, blocking=True
        )
        await hass.async_block_till_done()

    await select(living, "dbg2")
    assert client.calls == [("patch_zone", 1, {"offset_mode": "dbg2"})]
    assert hass.states.get(living).state == "dbg2"
    await select(living, "config")
    assert client.calls[-1] == ("patch_zone", 1, {"offset_mode": "config"})
    assert hass.states.get(living).state == "config"
    # The other zone keeps its mode.
    assert hass.states.get(bedroom).state == "config"


async def test_offset_mode_select_updates_the_room_sensor_state(hass, client):
    entry = await setup_entry(hass, client, enabled=OFFSET_MODE_ENABLED)
    bedroom = entity_id(hass, entry, "select", "zone2_offset_mode")
    assert external_room_sensor(hass, entry, 2).state == "wrong_offset_mode"

    await hass.services.async_call(
        "select", "select_option", {ATTR_ENTITY_ID: bedroom, "option": "setpoint"}, blocking=True
    )
    await hass.async_block_till_done()
    assert client.calls == [("patch_zone", 2, {"offset_mode": "setpoint"})]
    # The mode now accepts a reading and none came yet.
    assert external_room_sensor(hass, entry, 2).state == "no_reading"


async def test_offset_mode_select_error_is_a_home_assistant_error(hass, client):
    entry = await setup_entry(hass, client, enabled=OFFSET_MODE_ENABLED)
    living = entity_id(hass, entry, "select", "zone1_offset_mode")

    async def refuse(zone, **fields):
        raise FreedoApiError(400, 'offset_mode: bad mode "cloud": want config, dbg2 or setpoint')

    client.patch_install_zone = refuse
    with pytest.raises(HomeAssistantError, match="bad mode"):
        await hass.services.async_call(
            "select", "select_option", {ATTR_ENTITY_ID: living, "option": "dbg2"}, blocking=True
        )
    assert hass.states.get(living).state == "setpoint"


# --- room temperature correction ---

LIVING_CORRECTION = "zone1_room_temperature_correction"
BEDROOM_CORRECTION = "zone2_room_temperature_correction"


async def test_correction_select_options_and_current(hass, client):
    client.install_json["zones"][1]["auto_offset"] = True
    entry = await setup_entry(hass, client)
    living = entity_id(hass, entry, "select", LIVING_CORRECTION)
    bedroom = entity_id(hass, entry, "select", BEDROOM_CORRECTION)
    assert hass.states.get(living).attributes["options"] == [
        "none", "continuous_shift", "valve_calibration",
    ]
    assert er.async_get(hass).async_get(living).entity_category == "config"
    # Zone 1 is in mode setpoint, zone 2 in mode config with auto_offset on.
    assert hass.states.get(living).state == "continuous_shift"
    assert hass.states.get(bedroom).state == "valve_calibration"
    client.install_json["zones"][1]["auto_offset"] = False
    await refresh(hass, entry)
    assert hass.states.get(bedroom).state == "none"
    assert not has_entity(hass, entry, "select", "zone9_room_temperature_correction")


@pytest.mark.parametrize(
    ("mode", "auto", "option", "fields"),
    [
        ("setpoint", False, "valve_calibration", {"offset_mode": "config", "auto_offset": True}),
        ("config", True, "continuous_shift", {"auto_offset": False, "offset_mode": "setpoint"}),
        ("dbg2", True, "continuous_shift", {"auto_offset": False, "offset_mode": "setpoint"}),
        ("setpoint", False, "none", {"offset_mode": "config"}),
        ("dbg2", True, "none", {"auto_offset": False}),
        ("config", False, "continuous_shift", {"offset_mode": "setpoint"}),
        ("dbg2", False, "valve_calibration", {"auto_offset": True}),
        ("config", True, "valve_calibration", None),
    ],
)
async def test_correction_select_writes_what_the_daemon_accepts(hass, client, mode, auto, option, fields):
    # The fake daemon refuses offset_mode setpoint while auto_offset is on and
    # auto_offset in mode setpoint, against the values a request leaves.
    client.install_json["zones"][1].update(offset_mode=mode, auto_offset=auto)
    entry = await setup_entry(hass, client)
    bedroom = entity_id(hass, entry, "select", BEDROOM_CORRECTION)
    await select_option(hass, bedroom, option)
    assert client.calls == ([("patch_zone", 2, fields)] if fields else [])
    assert hass.states.get(bedroom).state == option


async def test_interval_needs_the_valve_calibration(hass, client):
    entry = await setup_entry(hass, client)
    interval = entity_id(hass, entry, "number", "zone1_auto_offset_interval_sec")
    assert hass.states.get(interval).state == "unavailable"
    await select_option(hass, entity_id(hass, entry, "select", LIVING_CORRECTION), "valve_calibration")
    assert float(hass.states.get(interval).state) == 24
    await select_option(hass, entity_id(hass, entry, "select", LIVING_CORRECTION), "none")
    assert hass.states.get(interval).state == "unavailable"


# --- automatic correction ---


async def test_auto_offset_switch_writes_and_shows_the_view(hass, client):
    client.install_json["zones"][0]["offset_mode"] = "config"
    entry = await setup_entry(hass, client, enabled=AUTO_OFFSET_ENABLED)
    auto = entity_id(hass, entry, "switch", "zone1_auto_offset")
    assert hass.states.get(auto).state == "off"
    # The attributes are the autoOffset view of GET /api/zones.
    assert hass.states.get(auto).attributes["sample_span_sec"] == 0
    assert hass.states.get(auto).attributes["filtered_diff_c"] == 0.0
    assert hass.states.get(auto).attributes["last_write_at"] is None
    assert hass.states.get(auto).attributes["stop_reason"] is None
    stopped = entity_id(hass, entry, "switch", "zone2_auto_offset")
    assert hass.states.get(stopped).attributes["stop_reason"].startswith("the filtered difference")

    await turn(hass, "turn_on", auto)
    await turn(hass, "turn_off", auto)
    assert client.calls == [
        ("patch_zone", 1, {"auto_offset": True}),
        ("patch_zone", 1, {"auto_offset": False}),
    ]
    # The entities show the row the daemon answered.
    assert hass.states.get(auto).state == "off"
    # The other zone keeps its own value, and the hot water zone has none.
    assert hass.states.get(entity_id(hass, entry, "switch", "zone2_auto_offset")).state == "off"
    assert not has_entity(hass, entry, "switch", "zone9_auto_offset")


async def test_auto_offset_error_is_a_home_assistant_error(hass, client):
    entry = await setup_entry(hass, client, enabled=AUTO_OFFSET_ENABLED)
    # Zone 1 is in offset mode setpoint: the daemon refuses the automatic
    # correction there.
    auto = entity_id(hass, entry, "switch", "zone1_auto_offset")
    with pytest.raises(HomeAssistantError, match="offset mode setpoint"):
        await turn(hass, "turn_on", auto)
    assert hass.states.get(auto).state == "off"


async def test_auto_offset_interval_number_reads_and_writes_seconds(hass, client):
    client.install_json["zones"][0].update(offset_mode="config", auto_offset=True)
    entry = await setup_entry(hass, client)
    interval = entity_id(hass, entry, "number", "zone1_auto_offset_interval_sec")
    state = hass.states.get(interval)
    # The daemon holds seconds, the number shows hours.
    assert float(state.state) == 24
    assert state.attributes["unit_of_measurement"] == "h"
    assert (state.attributes["min"], state.attributes["max"], state.attributes["step"]) == (1, 168, 1)
    assert er.async_get(hass).async_get(interval).entity_category == "config"

    await hass.services.async_call(
        "number", "set_value", {ATTR_ENTITY_ID: interval, "value": 48}, blocking=True
    )
    assert client.calls == [("patch_zone", 1, {"auto_offset_interval_sec": 172800})]
    assert float(hass.states.get(interval).state) == 48


async def test_temperature_source_sensor_with_auto_offset(hass, client):
    set_room_sensor(hass, "21.4", entity=HALL_SENSOR)
    # Zone 2 is in offset mode config and runs the automatic correction: the
    # daemon takes the external reading there too.
    client.install_json["zones"][1]["auto_offset"] = True
    entry = await setup_entry(hass, client, options={"room_temperatures": {"2": HALL_SENSOR}})
    bedroom = entity_id(hass, entry, "select", BEDROOM_SOURCE)
    assert hass.states.get(bedroom).state == "hall temperature (21.4 °C)"


async def test_temperature_source_sensor_keeps_the_valve_calibration(hass, client):
    set_room_sensor(hass, "21.8", entity=HALL_SENSOR)
    client.zones_json["declared"][1]["measuringDevice"] = "dddd"
    client.install_json["zones"][1]["auto_offset"] = True
    entry = await setup_entry(
        hass,
        client,
        options={"room_temperatures": {"2": HALL_SENSOR}, CONF_ROOM_TEMP_PAUSED: [2]},
    )
    bedroom = entity_id(hass, entry, "select", BEDROOM_SOURCE)
    assert hass.states.get(bedroom).state == "Bedroom (VA0000000004)"

    await select_option(hass, bedroom, "hall temperature (21.8 °C)")
    # The zone already has a correction, so the choice leaves it alone.
    assert not any(call[0] == "patch_zone" for call in client.calls)
    assert 2 not in entry.options.get(CONF_ROOM_TEMP_PAUSED, [])
    assert client.room_temps == [(2, 21.8)]
    assert hass.states.get(bedroom).state == "hall temperature (21.8 °C)"


async def test_room_temp_state_with_auto_offset_is_not_wrong_mode(hass, client):
    client.install_json["zones"][1]["auto_offset"] = True
    entry = await setup_entry(hass, client)
    # Mode config with the automatic correction accepts the reading: no reading
    # came yet, and the cause is not the offset mode.
    assert external_room_sensor(hass, entry, 2).state == "no_reading"


# --- weekly schedule ---


async def test_get_schedule(hass, client):
    await setup_entry(hass, client)
    response = await hass.services.async_call(
        DOMAIN, "get_schedule", {ATTR_ENTITY_ID: [LIVING, BEDROOM]},
        blocking=True, return_response=True,
    )
    living = response[LIVING]
    assert living["zone"] == 1
    assert living["programs"][0]["days"] == ["mon", "tue", "wed", "thu", "fri"]
    assert living["programs"][0]["entries"][1] == {"time": "06:30", "temperature": 21.0, "enabled": True}
    # A zone with no schedule has no programs.
    assert response[BEDROOM] == {"zone": 2, "programs": []}


async def test_set_schedule(hass, client):
    await setup_entry(hass, client)
    await call_freedo(hass, "set_schedule", {ATTR_ENTITY_ID: BEDROOM, "programs": WEEK})
    [(name, zone, body)] = client.calls
    assert (name, zone) == ("set_schedule", 2)
    assert body[0]["entries"][1] == {"minute": 390, "setpoint_c": 21.5, "enabled": True}
    assert body[1]["days"] == ["sat", "sun"]


@pytest.mark.parametrize(
    "programs",
    [
        WEEK[:1],
        [{"days": ["mon"], "entries": [{"time": "06:00", "temperature": 21}]}],
        [dict(WEEK[0], entries=[{"time": "00:00", "temperature": 40}]), WEEK[1]],
        # 29 entries encode to 263 bytes, above the 255 the device stores per day.
        [
            dict(
                WEEK[0],
                entries=[{"time": f"00:{m:02d}", "temperature": 17} for m in range(29)],
            ),
            WEEK[1],
        ],
    ],
)
async def test_set_schedule_refuses_before_the_daemon(hass, client, programs):
    await setup_entry(hass, client)
    with pytest.raises(ServiceValidationError, match="not valid"):
        await call_freedo(hass, "set_schedule", {ATTR_ENTITY_ID: BEDROOM, "programs": programs})
    assert client.calls == []


async def test_copy_schedule(hass, client):
    await setup_entry(hass, client)
    await call_freedo(hass, "copy_schedule", {ATTR_ENTITY_ID: LIVING, "to": [BEDROOM, BEDROOM]})
    assert [c[:2] for c in client.calls] == [("get_schedule", 1), ("set_schedule", 2)]
    body = client.calls[1][2]
    assert body[0]["entries"][1] == {"minute": 390, "setpoint_c": 21.0, "enabled": True}
    assert body[0]["entries"][2]["enabled"] is False
    assert [p["days"] for p in body] == [p["days"] for p in SCHEDULE["programs"]]


async def test_copy_schedule_goes_on_after_a_refused_zone(hass, client):
    office = copy.deepcopy(client.zones_json["declared"][1])
    office.update(number=3, name="Office", members=["eeee"])
    client.zones_json["declared"].append(office)
    await setup_entry(hass, client)
    real = client.set_zone_schedule

    async def refuse_zone_two(zone, programs):
        if zone == 2:
            raise FreedoApiError(400, "zone 2 declares no member")
        return await real(zone, programs)

    client.set_zone_schedule = refuse_zone_two
    with pytest.raises(HomeAssistantError, match=r"climate\.bedroom: .*zone 2 declares no member"):
        await call_freedo(
            hass, "copy_schedule", {ATTR_ENTITY_ID: LIVING, "to": [BEDROOM, "climate.office"]}
        )
    # The zone after the refused one still got the schedule.
    assert [c[:2] for c in client.calls if c[0] == "set_schedule"] == [("set_schedule", 3)]


@pytest.mark.parametrize(
    ("source", "targets", "message"),
    [
        (LIVING, [LIVING], "itself"),
        (LIVING, ["climate.nowhere"], "not a freedo zone"),
        (LIVING, ["switch.freedo_offline_schedule"], "not a freedo zone"),
        (BEDROOM, [LIVING], "no schedule"),
    ],
)
async def test_copy_schedule_refuses(hass, client, source, targets, message):
    await setup_entry(hass, client)
    with pytest.raises(ServiceValidationError, match=message):
        await call_freedo(hass, "copy_schedule", {ATTR_ENTITY_ID: source, "to": targets})
    assert not [c for c in client.calls if c[0] == "set_schedule"]


# --- hot water schedule ---

HOT_WATER_SCHEDULE = {
    "zone": 9,
    "members": ["cccc"],
    "programs": [
        {"days": ["mon", "tue", "wed", "thu", "fri", "sat", "sun"], "mask": 127,
         "entries": [{"minute": 0, "setpoint_c": 40.0, "enabled": False},
                     {"minute": 360, "setpoint_c": 55.0, "enabled": True}]},
    ],
}


async def test_get_hot_water_schedule(hass, client):
    client.schedules[9] = copy.deepcopy(HOT_WATER_SCHEDULE)
    await setup_entry(hass, client)
    response = await hass.services.async_call(
        DOMAIN, "get_schedule", {ATTR_ENTITY_ID: [HOT_WATER, LIVING]},
        blocking=True, return_response=True,
    )
    water = response[HOT_WATER]
    assert water["zone"] == 9
    assert water["programs"][0]["entries"] == [
        {"time": "00:00", "temperature": 40.0, "enabled": False},
        {"time": "06:00", "temperature": 55.0, "enabled": True},
    ]
    # The services serve a heating zone in the same call.
    assert response[LIVING]["zone"] == 1
    # The daemon keeps no hot water program until one is set: 404, no programs.
    del client.schedules[9]
    response = await hass.services.async_call(
        DOMAIN, "get_schedule", {ATTR_ENTITY_ID: HOT_WATER}, blocking=True, return_response=True
    )
    assert response == {HOT_WATER: {"zone": 9, "programs": []}}


async def test_set_hot_water_schedule(hass, client):
    await setup_entry(hass, client)
    await call_freedo(hass, "set_schedule", {ATTR_ENTITY_ID: HOT_WATER, "programs": HOT_WATER_WEEK})
    [(name, zone, body)] = client.calls
    assert (name, zone) == ("set_schedule", 9)
    assert body[0]["entries"][1] == {"minute": 360, "setpoint_c": 55.5, "enabled": True}
    assert body[0]["entries"][2]["enabled"] is False


@pytest.mark.parametrize(
    ("target", "programs", "range_"),
    [
        (HOT_WATER, WEEK, "30..65"),
        (HOT_WATER, [dict(HOT_WATER_WEEK[0], entries=[{"time": "00:00", "temperature": 29}])], "30..65"),
        (BEDROOM, HOT_WATER_WEEK, "5..25"),
    ],
)
async def test_set_schedule_keeps_to_the_range_of_the_zone(hass, client, target, programs, range_):
    await setup_entry(hass, client)
    with pytest.raises(ServiceValidationError, match=f"within {range_}"):
        await call_freedo(hass, "set_schedule", {ATTR_ENTITY_ID: target, "programs": programs})
    assert client.calls == []


async def test_set_schedule_checks_every_target_before_it_writes(hass, client):
    await setup_entry(hass, client)
    # The room week is not valid for the hot water zone, valid for the bedroom.
    with pytest.raises(ServiceValidationError, match="within 30..65"):
        await call_freedo(
            hass, "set_schedule", {ATTR_ENTITY_ID: [BEDROOM, HOT_WATER], "programs": WEEK}
        )
    assert client.calls == []


async def test_hot_water_schedule_error_is_a_home_assistant_error(hass, client):
    await setup_entry(hass, client)

    async def refuse(zone, programs):
        raise FreedoApiError(400, "programs[0].entries[0].setpoint_c must be within 30..65 C for hot water")

    client.set_zone_schedule = refuse
    with pytest.raises(HomeAssistantError, match="for hot water"):
        await call_freedo(
            hass, "set_schedule", {ATTR_ENTITY_ID: HOT_WATER, "programs": HOT_WATER_WEEK}
        )


@pytest.mark.parametrize(
    ("source", "targets", "message"),
    [
        (HOT_WATER, [LIVING], "different kind of zone"),
        (LIVING, [HOT_WATER], "different kind of zone"),
        (LIVING, [BEDROOM, HOT_WATER], "different kind of zone"),
        (HOT_WATER, [HOT_WATER], "itself"),
    ],
)
async def test_copy_schedule_refuses_a_room_hot_water_pair(hass, client, source, targets, message):
    client.schedules[9] = copy.deepcopy(HOT_WATER_SCHEDULE)
    await setup_entry(hass, client)
    with pytest.raises(ServiceValidationError, match=message):
        await call_freedo(hass, "copy_schedule", {ATTR_ENTITY_ID: source, "to": targets})
    # Nothing is read or written before the refusal.
    assert client.calls == []


# --- open window by hand ---


async def test_set_window_activates_and_resets(hass, client):
    entry = await setup_entry(hass, client)
    zone_window = entity_id(hass, entry, "binary_sensor", "zone2_window")
    assert hass.states.get(zone_window).state == "off"

    async def daemon_window(zone, is_open):
        # The daemon answers with the window view of the zone and reports it.
        client.windows.append((zone, is_open))
        view = {"open": is_open, "source": "external" if is_open else None, "since": None,
                "deadline": None, "undelivered": []}
        client.zones_json["declared"][zone - 1]["window"] = view
        return copy.deepcopy(view)

    client.push_window = daemon_window
    # The default of the field is to activate.
    await call_freedo(hass, "set_window", {ATTR_ENTITY_ID: BEDROOM})
    assert client.windows == [(2, True)]
    assert hass.states.get(BEDROOM).attributes["window_open"] is True
    assert hass.states.get(BEDROOM).attributes["window_source"] == "external"
    assert hass.states.get(zone_window).state == "on"

    await call_freedo(hass, "set_window", {ATTR_ENTITY_ID: BEDROOM, "open": False})
    assert client.windows == [(2, True), (2, False)]
    assert hass.states.get(BEDROOM).attributes["window_open"] is False
    assert hass.states.get(zone_window).state == "off"


async def test_set_window_targets_heating_zones_only(hass, client):
    await setup_entry(hass, client)
    # The service is an entity service of the climate entities: the hot water
    # zone has no window and the daemon would answer 400.
    await call_freedo(hass, "set_window", {ATTR_ENTITY_ID: [LIVING, HOT_WATER], "open": True})
    assert client.windows == [(1, True)]


async def test_set_window_error_is_a_home_assistant_error(hass, client):
    await setup_entry(hass, client)
    client.window_errors.append(FreedoApiError(400, "zone 2: the zone has no open window"))
    with pytest.raises(HomeAssistantError, match="no open window"):
        await call_freedo(hass, "set_window", {ATTR_ENTITY_ID: BEDROOM})
    assert client.windows == []


# --- pairing mode ---


async def test_pairing_mode_switch(hass, client):
    entry = await setup_entry(hass, client)
    pairing = entity_id(hass, entry, "switch", "pairing_mode")
    assert hass.states.get(pairing).state == "off"
    entity = er.async_get(hass).async_get(pairing)
    assert entity.entity_category == "config"
    # The switch belongs to the hub device.
    hub = dr.async_get(hass).async_get_device_by_identifier((DOMAIN, entry.entry_id), entry.entry_id)
    assert entity.device_id == hub.id

    await turn(hass, "turn_on", pairing)
    # The longest window the daemon allows, 10 minutes.
    assert client.calls == [("pair_window", 600)]
    state = hass.states.get(pairing)
    assert state.state == "on"
    assert state.attributes["until"] == "2026-01-01T00:10:00Z"

    await turn(hass, "turn_off", pairing)
    assert client.calls[-1] == ("pair_window", 0)
    state = hass.states.get(pairing)
    assert state.state == "off"
    assert state.attributes["until"] is None


async def test_pairing_mode_follows_the_daemon(hass, client):
    entry = await setup_entry(hass, client)
    pairing = entity_id(hass, entry, "switch", "pairing_mode")
    # A window opened outside Home Assistant shows at the next poll, and so does
    # its end. The joiners the window learned are not exposed.
    client.pair_window_json = {
        "open": True, "opened": "2026-01-01T00:00:00Z", "until": "2026-01-01T00:02:00Z",
        "responder": True,
        "joiners": [{"eui": "0000000000000001", "short": "ffff", "label": "", "frames": 3}],
    }
    await refresh(hass, entry)
    state = hass.states.get(pairing)
    assert state.state == "on"
    assert set(state.attributes) - {"friendly_name"} == {"until"}
    client.pair_window_json = {"open": False, "responder": False, "joiners": []}
    await refresh(hass, entry)
    assert hass.states.get(pairing).state == "off"
    assert client.calls == []


async def test_no_pairing_mode_on_a_daemon_without_the_route(hass, client):
    client.pair_window_json = None
    entry = await setup_entry(hass, client)
    assert not has_entity(hass, entry, "switch", "pairing_mode")
    # The other entities load.
    assert has_entity(hass, entry, "switch", "away")


async def test_pairing_mode_error_is_a_home_assistant_error(hass, client):
    entry = await setup_entry(hass, client)
    pairing = entity_id(hass, entry, "switch", "pairing_mode")

    async def refuse(seconds):
        raise FreedoApiError(400, "seconds must be between 0 and 600")

    client.set_pair_window = refuse
    with pytest.raises(HomeAssistantError, match="between 0 and 600"):
        await turn(hass, "turn_on", pairing)
    assert hass.states.get(pairing).state == "off"


# --- diagnostics ---


async def test_device_diagnostic_sensors(hass, client):
    entry = await setup_entry(hass, client)
    registry = er.async_get(hass)
    firmware = entity_id(hass, entry, "sensor", "aaaa_firmware")
    assert hass.states.get(firmware).state == "2.5"
    assert registry.async_get(firmware).entity_category == "diagnostic"
    # A device that has not reported a firmware has none.
    del client.nodes_json[0]["Telemetry"]["fw_run_version"]
    await refresh(hass, entry)
    assert hass.states.get(firmware).state == "unknown"

    # The device readings are diagnostic and off by default, as the climate
    # entity shows the zone values.
    temperature = registry.async_get(entity_id(hass, entry, "sensor", "aaaa_temperature"))
    assert temperature.disabled_by is er.RegistryEntryDisabler.INTEGRATION
    assert temperature.entity_category == "diagnostic"
    assert temperature.original_device_class == "temperature"


async def test_device_temperature_sensor_state(hass, client):
    entry = await setup_entry(hass, client)
    registry = er.async_get(hass)
    sensor = entity_id(hass, entry, "sensor", "aaaa_temperature")
    registry.async_update_entity(sensor, disabled_by=None)
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        await hass.config_entries.async_reload(entry.entry_id)
        await hass.async_block_till_done()
    # Zone 1 corrects its temperature by +0.5 C: the device reading is the wire one.
    assert float(hass.states.get(sensor).state) == 20.9


async def test_humidity_sensor_appears_with_the_first_report(hass, client):
    # Only bbbb reports a humidity, and it is no declared device: none is created.
    entry = await setup_entry(hass, client)
    assert not has_entity(hass, entry, "sensor", "aaaa_humidity")
    assert not has_entity(hass, entry, "sensor", "bbbb_humidity")

    # The valve reports a humidity later: its sensor is added on the next poll.
    client.nodes_json[0]["HumidityPct"] = 41.5
    client.nodes_json[0]["Telemetry"]["humidity_pct"] = "41.50%"
    await refresh(hass, entry)
    humidity = er.async_get(hass).async_get(entity_id(hass, entry, "sensor", "aaaa_humidity"))
    assert humidity.original_device_class == "humidity"
    assert humidity.disabled_by is er.RegistryEntryDisabler.INTEGRATION
    # A later poll does not add it again.
    await refresh(hass, entry)
    assert len([e for e in er.async_entries_for_config_entry(er.async_get(hass), entry.entry_id)
                if e.unique_id.endswith("aaaa_humidity")]) == 1


async def test_zone_demand_sensor(hass, client):
    entry = await setup_entry(hass, client)
    assert float(hass.states.get(entity_id(hass, entry, "sensor", "zone1_demand")).state) == 40
    # Zone 2 has not reported a zone report.
    assert hass.states.get(entity_id(hass, entry, "sensor", "zone2_demand")).state == "unknown"
    assert not has_entity(hass, entry, "sensor", "zone9_demand")


async def test_zone_heating_binary_sensor(hass, client):
    entry = await setup_entry(hass, client)
    living = entity_id(hass, entry, "binary_sensor", "zone1_heating")
    state = hass.states.get(living)
    assert (state.state, state.attributes["device_class"]) == ("on", "running")
    assert er.async_get(hass).async_get(living).entity_category is None
    assert hass.states.get(LIVING).attributes["hvac_action"] == "heating"
    # Zone 2 has not reported a zone report.
    bedroom = entity_id(hass, entry, "binary_sensor", "zone2_heating")
    assert hass.states.get(bedroom).state == "unknown"
    assert "hvac_action" not in hass.states.get(BEDROOM).attributes
    # The hot water zone has no heating sensor.
    assert not has_entity(hass, entry, "binary_sensor", "zone9_heating")

    for view in client.zones_json["demand"]:
        view.update(DemandPct=0, UpdatedUnix=1700000060)
    await refresh(hass, entry)
    for sensor, climate in ((living, LIVING), (bedroom, BEDROOM)):
        assert hass.states.get(sensor).state == "off"
        assert hass.states.get(climate).attributes["hvac_action"] == "idle"


async def test_comms_warning_binary_sensor(hass, client):
    entry = await setup_entry(hass, client)
    warning = entity_id(hass, entry, "binary_sensor", "aaaa_commsWarnings")
    state = hass.states.get(warning)
    assert state.state == "on"
    assert state.attributes["warnings"] == ["noConfigAnswer"]
    assert state.attributes["device_class"] == "problem"
    assert er.async_get(hass).async_get(warning).entity_category == "diagnostic"
    # The communication warnings are no fault.
    assert hass.states.get(entity_id(hass, entry, "binary_sensor", "aaaa_fault")).state == "off"

    client.health_json["devices"][0]["commsWarnings"] = None
    await refresh(hass, entry)
    state = hass.states.get(warning)
    assert (state.state, state.attributes["warnings"]) == ("off", [])


async def test_no_comms_warning_sensor_without_the_field(hass, client):
    # A daemon older than the link bits of the fault word sends no list.
    for dev in client.health_json["devices"]:
        del dev["commsWarnings"]
    entry = await setup_entry(hass, client)
    assert not has_entity(hass, entry, "binary_sensor", "aaaa_commsWarnings")
    assert has_entity(hass, entry, "binary_sensor", "aaaa_fault")


# --- boiler flow limit ---


def with_flow_circuit(client, **change):
    client.boiler_json["flowLimit"]["circuits"] = [dict(FLOW_CIRCUIT, **change)]


async def test_flow_limit_number(hass, client):
    with_flow_circuit(client)
    entry = await setup_entry(hass, client)
    flow = entity_id(hass, entry, "number", "circuit1_flow_limit")
    state = hass.states.get(flow)
    assert float(state.state) == 45
    assert state.attributes["device_class"] == "temperature"

    await hass.services.async_call(
        "number", "set_value", {ATTR_ENTITY_ID: flow, "value": 52}, blocking=True
    )
    # The write names the thermostat that drives the circuit.
    assert client.calls == [("flow", 1, 52.0, "0000000000000001")]

    for value in (29, 76):
        with pytest.raises(ServiceValidationError):
            await hass.services.async_call(
                "number", "set_value", {ATTR_ENTITY_ID: flow, "value": value}, blocking=True
            )
    assert len(client.calls) == 1


async def test_flow_limit_number_range_follows_the_daemon(hass, client):
    # The daemon cuts the top to the boiler's highest CH setpoint.
    with_flow_circuit(client, minC=32, maxC=60)
    entry = await setup_entry(hass, client)
    flow = entity_id(hass, entry, "number", "circuit1_flow_limit")
    state = hass.states.get(flow)
    assert (state.attributes["min"], state.attributes["max"]) == (32, 60)

    await hass.services.async_call(
        "number", "set_value", {ATTR_ENTITY_ID: flow, "value": 60}, blocking=True
    )
    for value in (31, 61):
        with pytest.raises(ServiceValidationError):
            await hass.services.async_call(
                "number", "set_value", {ATTR_ENTITY_ID: flow, "value": value}, blocking=True
            )
    assert client.calls == [("flow", 1, 60.0, "0000000000000001")]


async def test_flow_limit_number_range_falls_back_without_the_fields(hass, client):
    with_flow_circuit(client)
    entry = await setup_entry(hass, client)
    state = hass.states.get(entity_id(hass, entry, "number", "circuit1_flow_limit"))
    assert (state.attributes["min"], state.attributes["max"]) == (30, 75)


@pytest.mark.parametrize("driver", ["curve", "defaultCurve", "adapt"])
async def test_flow_limit_number_edits_the_manual_limit_while_managed(hass, client, driver):
    # The daemon refuses POST /api/boiler/flow on a circuit that a curve, the
    # default curve or the adaptation drives (409). The number then shows and
    # edits flow_limit_c, which they start from.
    client.settings_json["flow_limit_c"] = 56
    with_flow_circuit(client, computedC=41, **{driver: True})
    entry = await setup_entry(hass, client)
    flow = entity_id(hass, entry, "number", "circuit1_flow_limit")
    assert float(hass.states.get(flow).state) == 56

    await hass.services.async_call(
        "number", "set_value", {ATTR_ENTITY_ID: flow, "value": 58}, blocking=True
    )
    assert client.calls == [("patch_install", {"flow_limit_c": 58})]
    assert float(hass.states.get(flow).state) == 58

    # The circuit is free again: the number shows what is served and writes it.
    client.boiler_json["flowLimit"]["circuits"][0][driver] = False
    await refresh(hass, entry)
    assert float(hass.states.get(flow).state) == 45
    await hass.services.async_call(
        "number", "set_value", {ATTR_ENTITY_ID: flow, "value": 52}, blocking=True
    )
    assert client.calls[-1] == ("flow", 1, 52.0, "0000000000000001")


async def test_flow_limit_number_unknown_while_managed_without_a_manual_limit(hass, client):
    with_flow_circuit(client, curve=True)
    entry = await setup_entry(hass, client)
    flow = entity_id(hass, entry, "number", "circuit1_flow_limit")
    assert hass.states.get(flow).state == "unknown"

    await hass.services.async_call(
        "number", "set_value", {ATTR_ENTITY_ID: flow, "value": 50}, blocking=True
    )
    assert client.calls == [("patch_install", {"flow_limit_c": 50})]
    assert float(hass.states.get(flow).state) == 50


async def test_flow_limit_sensor_attributes_show_the_default_curve(hass, client):
    with_flow_circuit(client, defaultCurve=True, reductionC=7, computedC=49, servedC=49)
    entry = await setup_entry(hass, client)
    for field in ("servedC", "computedC"):
        state = hass.states.get(entity_id(hass, entry, "sensor", f"circuit1_{field}"))
        assert float(state.state) == 49
        assert state.attributes["default_curve"] is True
        assert state.attributes["reduction_c"] == 7


async def test_flow_default_curve_switch(hass, client):
    client.settings_json["flow_limit_c"] = 56
    entry = await setup_entry(hass, client)
    switch = entity_id(hass, entry, "switch", "flow_default_curve")
    assert hass.states.get(switch).state == "off"
    assert er.async_get(hass).async_get(switch).entity_category == "config"

    await hass.services.async_call("switch", "turn_on", {ATTR_ENTITY_ID: switch}, blocking=True)
    assert client.calls == [("patch_install", {"flow_default_curve": True})]
    assert hass.states.get(switch).state == "on"

    await hass.services.async_call("switch", "turn_off", {ATTR_ENTITY_ID: switch}, blocking=True)
    assert client.calls[-1] == ("patch_install", {"flow_default_curve": False})
    assert hass.states.get(switch).state == "off"


async def test_flow_default_curve_switch_needs_the_manual_limit(hass, client):
    entry = await setup_entry(hass, client)
    switch = entity_id(hass, entry, "switch", "flow_default_curve")
    # The daemon refuses the curve while flow_limit_c is unset.
    with pytest.raises(HomeAssistantError, match="flow_limit_c"):
        await hass.services.async_call("switch", "turn_on", {ATTR_ENTITY_ID: switch}, blocking=True)
    assert hass.states.get(switch).state == "off"


async def test_flow_auto_adapt_switch(hass, client):
    entry = await setup_entry(hass, client)
    switch = entity_id(hass, entry, "switch", "flow_auto_adapt")
    assert hass.states.get(switch).state == "off"

    await hass.services.async_call("switch", "turn_on", {ATTR_ENTITY_ID: switch}, blocking=True)
    assert client.calls == [("patch_install", {"flow_auto_adapt": True})]
    assert hass.states.get(switch).state == "on"

    # The state follows the daemon at the next poll.
    client.settings_json["flow_auto_adapt"] = False
    await refresh(hass, entry)
    assert hass.states.get(switch).state == "off"


async def test_flow_switches_need_a_daemon_that_serves_the_flags(hass, client):
    # An older daemon sends neither flag, and a daemon without an installation
    # sends no options at all.
    for key in ("flow_default_curve", "flow_auto_adapt"):
        del client.settings_json[key]
    entry = await setup_entry(hass, client)
    assert not has_entity(hass, entry, "switch", "flow_default_curve")
    assert not has_entity(hass, entry, "switch", "flow_auto_adapt")


async def test_flow_limit_shows_the_default_until_a_value_is_set(hass, client):
    # The daemon serves defaultC (48) to a circuit with no value of its own.
    with_flow_circuit(client, servedC=48)
    entry = await setup_entry(hass, client)
    assert float(hass.states.get(entity_id(hass, entry, "number", "circuit1_flow_limit")).state) == 48
    assert float(hass.states.get(entity_id(hass, entry, "sensor", "circuit1_servedC")).state) == 48


async def test_flow_limit_needs_a_heat_source(hass, client):
    with_flow_circuit(client)
    # The daemon holds no installation (503 on GET /api/install/circuits).
    client.circuits_json = {}
    entry = await setup_entry(hass, client)
    assert not has_entity(hass, entry, "number", "circuit1_flow_limit")


async def test_flow_limit_error_is_a_home_assistant_error(hass, client):
    with_flow_circuit(client)
    entry = await setup_entry(hass, client)
    flow = entity_id(hass, entry, "number", "circuit1_flow_limit")

    async def refuse(circuit, temp_c, target):
        raise FreedoApiError(409, "circuit 1 follows its weather curve")

    client.set_flow_limit = refuse
    with pytest.raises(HomeAssistantError, match="weather curve"):
        await hass.services.async_call(
            "number", "set_value", {ATTR_ENTITY_ID: flow, "value": 50}, blocking=True
        )


# --- boiler energy ---


def with_energy(client, **change):
    client.boiler_json["energy"] = dict(ENERGY, **change)


async def test_energy_sensors(hass, client):
    with_energy(client)
    entry = await setup_entry(hass, client)
    registry = er.async_get(hass)

    energy = hass.states.get(entity_id(hass, entry, "sensor", "boiler_heat_energy"))
    assert float(energy.state) == 1234.567
    assert energy.attributes["device_class"] == "energy"
    assert energy.attributes["state_class"] == "total_increasing"
    assert energy.attributes["unit_of_measurement"] == "kWh"

    power = hass.states.get(entity_id(hass, entry, "sensor", "boiler_heat_power"))
    assert float(power.state) == 12.4
    assert power.attributes["device_class"] == "power"
    assert power.attributes["state_class"] == "measurement"
    assert power.attributes["unit_of_measurement"] == "kW"

    status_id = entity_id(hass, entry, "sensor", "boiler_energy_estimate")
    assert hass.states.get(status_id).state == "running"
    assert registry.async_get(status_id).entity_category == "diagnostic"

    # The counter and the output follow the daemon.
    with_energy(client, totalKWh=1240.0, powerKW=0.0)
    await refresh(hass, entry)
    assert float(hass.states.get(energy.entity_id).state) == 1240.0
    assert float(hass.states.get(power.entity_id).state) == 0.0


async def test_energy_sensors_wait_for_the_boiler_power(hass, client):
    with_energy(
        client, running=False, reason="noMaxPower", maxPowerKW=None, minPowerKW=0.0, powerKW=0.0
    )
    entry = await setup_entry(hass, client)
    # The estimate cannot run: the sensors that need it do not exist, and the
    # status says why.
    assert not has_entity(hass, entry, "sensor", "boiler_heat_energy")
    assert not has_entity(hass, entry, "sensor", "boiler_heat_power")
    status_id = entity_id(hass, entry, "sensor", "boiler_energy_estimate")
    assert hass.states.get(status_id).state == "no_max_power"

    with_energy(client, running=False, reason="noReport", powerKW=0.0)
    await refresh(hass, entry)
    assert hass.states.get(status_id).state == "no_report"
    energy = hass.states.get(entity_id(hass, entry, "sensor", "boiler_heat_energy"))
    assert float(energy.state) == 1234.567
    # The output is unknown while the estimate does not run.
    assert hass.states.get(entity_id(hass, entry, "sensor", "boiler_heat_power")).state == "unknown"

    # A later poll does not add them again.
    await refresh(hass, entry)
    unique_ids = [e.unique_id for e in er.async_entries_for_config_entry(er.async_get(hass), entry.entry_id)]
    assert len([u for u in unique_ids if u.endswith("boiler_heat_energy")]) == 1


@pytest.mark.parametrize(
    ("reason", "state"),
    [
        ("noMaxPower", "no_max_power"),
        ("noReport", "no_report"),
        ("stale", "stale"),
        ("noBurnerFlag", "no_burner_flag"),
        ("noModulation", "no_modulation"),
        ("someNewReason", "unknown"),
    ],
)
async def test_energy_status_shows_the_reason(hass, client, reason, state):
    with_energy(client, running=False, reason=reason, powerKW=0.0)
    entry = await setup_entry(hass, client)
    assert hass.states.get(entity_id(hass, entry, "sensor", "boiler_energy_estimate")).state == state


async def test_no_energy_sensors_without_the_estimate(hass, client):
    # A daemon older than the estimate sends no energy object.
    entry = await setup_entry(hass, client)
    for key in ("boiler_heat_energy", "boiler_heat_power", "boiler_energy_estimate"):
        assert not has_entity(hass, entry, "sensor", key)


async def test_boiler_power_numbers(hass, client):
    entry = await setup_entry(hass, client)
    registry = er.async_get(hass)
    high = entity_id(hass, entry, "number", "boiler_max_power")
    low = entity_id(hass, entry, "number", "boiler_min_power")
    # The maximum is unset; the minimum is 0.
    assert hass.states.get(high).state == "unknown"
    assert float(hass.states.get(low).state) == 0
    for number in (high, low):
        assert registry.async_get(number).entity_category == "config"
        assert hass.states.get(number).attributes["unit_of_measurement"] == "kW"
        assert hass.states.get(number).attributes["device_class"] == "power"

    async def write(number, value):
        await hass.services.async_call(
            "number", "set_value", {ATTR_ENTITY_ID: number, "value": value}, blocking=True
        )
        await hass.async_block_till_done()

    await write(high, 24)
    await write(low, 3.5)
    assert client.calls == [
        ("patch_install", {"boiler_max_power_kw": 24.0}),
        ("patch_install", {"boiler_min_power_kw": 3.5}),
    ]
    assert float(hass.states.get(high).state) == 24
    assert float(hass.states.get(low).state) == 3.5

    # The daemon refuses a minimum that is not below the maximum.
    with pytest.raises(HomeAssistantError, match="not below boiler_max_power_kw"):
        await write(low, 24)
    assert float(hass.states.get(low).state) == 3.5
    # The range is the daemon range: 0 < max <= 500, 0 <= min.
    for number, value in ((high, 0), (high, 501), (low, -1)):
        with pytest.raises(ServiceValidationError):
            await write(number, value)
    assert len(client.calls) == 3


async def test_boiler_power_numbers_need_an_installation(hass, client):
    # 503 on GET /api/install: the daemon holds no installation.
    client.settings_error = FreedoApiError(503, "no installation")
    entry = await setup_entry(hass, client)
    for key in ("boiler_max_power", "boiler_min_power", "boiler_max_gas", "boiler_min_gas"):
        assert not has_entity(hass, entry, "number", key)


# --- boiler gas ---


def with_gas(client, **change):
    client.boiler_json["energy"] = dict(ENERGY, gas=dict(GAS, **change))


async def test_gas_sensors(hass, client):
    with_gas(client)
    entry = await setup_entry(hass, client)
    registry = er.async_get(hass)

    volume = hass.states.get(entity_id(hass, entry, "sensor", "boiler_gas_volume"))
    assert float(volume.state) == 321.654
    assert volume.attributes["device_class"] == "gas"
    assert volume.attributes["state_class"] == "total_increasing"
    assert volume.attributes["unit_of_measurement"] == "m³"

    flow = hass.states.get(entity_id(hass, entry, "sensor", "boiler_gas_flow"))
    assert float(flow.state) == 1.465
    assert flow.attributes["device_class"] == "volume_flow_rate"
    assert flow.attributes["state_class"] == "measurement"
    assert flow.attributes["unit_of_measurement"] == "m³/h"

    status_id = entity_id(hass, entry, "sensor", "boiler_gas_estimate")
    assert hass.states.get(status_id).state == "running"
    assert hass.states.get(status_id).attributes["device_class"] == "enum"
    assert registry.async_get(status_id).entity_category == "diagnostic"

    # The counter follows the daemon, and the flow is unknown while the
    # estimate does not run.
    with_gas(client, totalM3=330.0, flowM3H=0.0, running=False, reason="stale")
    await refresh(hass, entry)
    assert float(hass.states.get(volume.entity_id).state) == 330.0
    assert hass.states.get(flow.entity_id).state == "unknown"
    assert hass.states.get(status_id).state == "stale"


@pytest.mark.parametrize(
    ("unit", "volume_class", "volume_unit", "flow_class", "flow_unit"),
    [
        ("m3", "gas", "m³", "volume_flow_rate", "m³/h"),
        ("l", "volume", "L", "volume_flow_rate", "L/h"),
        ("kg", "weight", "kg", None, "kg/h"),
    ],
)
async def test_fuel_unit_sets_the_sensor_classes_and_units(
    hass, client, unit, volume_class, volume_unit, flow_class, flow_unit
):
    client.settings_json["fuel_unit"] = unit
    with_gas(client, unit=unit)
    entry = await setup_entry(hass, client)

    volume = hass.states.get(entity_id(hass, entry, "sensor", "boiler_gas_volume"))
    assert volume.attributes["device_class"] == volume_class
    assert volume.attributes["unit_of_measurement"] == volume_unit
    assert volume.attributes["state_class"] == "total_increasing"

    flow = hass.states.get(entity_id(hass, entry, "sensor", "boiler_gas_flow"))
    assert flow.attributes.get("device_class") == flow_class
    assert flow.attributes["unit_of_measurement"] == flow_unit
    assert flow.attributes["state_class"] == "measurement"

    for key in ("boiler_max_gas", "boiler_min_gas"):
        number = hass.states.get(entity_id(hass, entry, "number", key))
        assert number.attributes.get("device_class") == flow_class
        assert number.attributes["unit_of_measurement"] == flow_unit


async def test_fuel_unit_defaults_to_m3_on_a_daemon_without_it(hass, client):
    # Neither the install settings nor the gas view carry a unit.
    with_gas(client)
    entry = await setup_entry(hass, client)
    assert hass.states.get(entity_id(hass, entry, "sensor", "boiler_gas_volume")).attributes["unit_of_measurement"] == "m³"
    number = hass.states.get(entity_id(hass, entry, "number", "boiler_max_gas"))
    assert number.attributes["unit_of_measurement"] == "m³/h"


async def test_gas_sensors_wait_for_both_flows(hass, client):
    unset = {"running": False, "reason": "noGasFlow", "flowM3H": 0.0, "maxFlowM3H": None}
    with_gas(client, minFlowM3H=None, **unset)
    entry = await setup_entry(hass, client)
    # One flow is not enough: the volume and the flow do not exist, and the
    # status says why.
    assert not has_entity(hass, entry, "sensor", "boiler_gas_volume")
    assert not has_entity(hass, entry, "sensor", "boiler_gas_flow")
    status_id = entity_id(hass, entry, "sensor", "boiler_gas_estimate")
    assert hass.states.get(status_id).state == "no_gas_flow"

    with_gas(client, maxFlowM3H=2.63, minFlowM3H=None, **{k: v for k, v in unset.items() if k != "maxFlowM3H"})
    await refresh(hass, entry)
    assert not has_entity(hass, entry, "sensor", "boiler_gas_volume")

    with_gas(client, running=False, reason="noReport", flowM3H=0.0)
    await refresh(hass, entry)
    assert hass.states.get(status_id).state == "no_report"
    volume = hass.states.get(entity_id(hass, entry, "sensor", "boiler_gas_volume"))
    assert float(volume.state) == 321.654
    assert hass.states.get(entity_id(hass, entry, "sensor", "boiler_gas_flow")).state == "unknown"

    # A later poll does not add them again.
    await refresh(hass, entry)
    unique_ids = [e.unique_id for e in er.async_entries_for_config_entry(er.async_get(hass), entry.entry_id)]
    assert len([u for u in unique_ids if u.endswith("boiler_gas_volume")]) == 1


@pytest.mark.parametrize(
    ("reason", "state"),
    [
        ("noGasFlow", "no_gas_flow"),
        ("noReport", "no_report"),
        ("stale", "stale"),
        ("noBurnerFlag", "no_burner_flag"),
        ("noModulation", "no_modulation"),
        ("someNewReason", "unknown"),
    ],
)
async def test_gas_status_shows_the_reason(hass, client, reason, state):
    with_gas(client, running=False, reason=reason, flowM3H=0.0)
    entry = await setup_entry(hass, client)
    assert hass.states.get(entity_id(hass, entry, "sensor", "boiler_gas_estimate")).state == state


async def test_no_gas_sensors_without_the_gas_estimate(hass, client):
    # A daemon that serves the heat estimate but not the gas one.
    with_energy(client)
    entry = await setup_entry(hass, client)
    assert has_entity(hass, entry, "sensor", "boiler_heat_energy")
    for key in ("boiler_gas_volume", "boiler_gas_flow", "boiler_gas_estimate", "boiler_gas_meter"):
        assert not has_entity(hass, entry, "sensor", key)


# --- gas meter ---

# GET /api/boiler energy.gas.meter: the meter value the daemon estimates from
# the readings the user records.
METER = {"value": 8785.957, "lastReading": 8785.957, "lastReadingAt": "2026-01-01T00:00:00Z",
         "readings": 3, "factor": 0.95, "factorReadings": 2}


def with_meter(client, meter):
    client.boiler_json["energy"] = dict(ENERGY, gas=dict(GAS, meter=meter))


async def test_gas_meter_sensor(hass, client):
    with_meter(client, dict(METER))
    entry = await setup_entry(hass, client)
    meter = hass.states.get(entity_id(hass, entry, "sensor", "boiler_gas_meter"))
    assert float(meter.state) == 8785.957
    assert meter.attributes["device_class"] == "gas"
    assert meter.attributes["unit_of_measurement"] == "m³"
    # Total increasing: a reading below 0.9 of the previous value is a meter
    # reset, as after a replacement, and a small dip only gives a negative delta.
    assert meter.attributes["state_class"] == "total_increasing"
    assert meter.attributes["last_reading"] == 8785.957
    assert meter.attributes["last_reading_at"] == "2026-01-01T00:00:00Z"
    assert meter.attributes["readings"] == 3
    assert meter.attributes["factor"] == 0.95
    assert meter.attributes["factor_readings"] == 2


async def test_gas_meter_unavailable_without_a_reading(hass, client):
    with_meter(client, None)
    entry = await setup_entry(hass, client)
    assert hass.states.get(entity_id(hass, entry, "sensor", "boiler_gas_meter")).state == "unavailable"


@pytest.mark.parametrize(
    ("unit", "device_class", "unit_name"),
    [("m3", "gas", "m³"), ("l", "volume", "L"), ("kg", "weight", "kg")],
)
async def test_gas_meter_follows_the_fuel_unit(hass, client, unit, device_class, unit_name):
    client.settings_json["fuel_unit"] = unit
    with_meter(client, dict(METER))
    entry = await setup_entry(hass, client)
    meter = hass.states.get(entity_id(hass, entry, "sensor", "boiler_gas_meter"))
    assert meter.attributes["device_class"] == device_class
    assert meter.attributes["unit_of_measurement"] == unit_name


async def test_calibrate_gas_meter_records_the_reading(hass, client):
    with_meter(client, None)
    entry = await setup_entry(hass, client)
    meter = entity_id(hass, entry, "sensor", "boiler_gas_meter")
    assert hass.states.get(meter).state == "unavailable"

    await call_freedo(hass, "calibrate_gas_meter", {"value": 8786.5})
    assert client.calls == [("gas_meter", 8786.5, None, None, False)]
    # The service refreshes the coordinator: the sensor shows the new reading.
    assert float(hass.states.get(meter).state) == 8786.5


async def test_calibrate_gas_meter_sends_the_estimate_and_time(hass, client):
    with_meter(client, dict(METER))
    await setup_entry(hass, client)
    at = dt_util.parse_datetime("2026-01-01T00:00:00+00:00")
    await call_freedo(
        hass, "calibrate_gas_meter", {"value": 8786.5, "estimate": 300.0, "at": at}
    )
    assert client.calls == [("gas_meter", 8786.5, 300.0, at, False)]


async def test_calibrate_gas_meter_replaced_starts_a_new_meter(hass, client):
    # A replaced meter drops the stored readings and starts from this one.
    with_meter(client, dict(METER))
    await setup_entry(hass, client)
    await call_freedo(hass, "calibrate_gas_meter", {"value": 12.0, "replaced": True})
    assert client.calls == [("gas_meter", 12.0, None, None, True)]


async def test_calibrate_gas_meter_localizes_a_naive_time(hass, client):
    # The datetime selector sends a naive local time, which the daemon refuses:
    # the sent value must carry an offset.
    with_meter(client, dict(METER))
    await setup_entry(hass, client)
    await call_freedo(
        hass, "calibrate_gas_meter", {"value": 8786.5, "at": "2026-01-01T00:00:00"}
    )
    sent = client.calls[0][3]
    assert sent == dt_util.as_utc(dt_util.as_local(dt_util.parse_datetime("2026-01-01T00:00:00")))
    assert sent.isoformat().endswith("+00:00")


async def test_calibrate_gas_meter_error_is_a_home_assistant_error(hass, client):
    with_meter(client, dict(METER))
    await setup_entry(hass, client)

    async def refuse(reading, estimate=None, at=None, replaced=False):
        raise FreedoApiError(400, "reading 8000 is below the previous reading 8785.957")

    client.record_gas_meter = refuse
    with pytest.raises(HomeAssistantError, match="below the previous reading"):
        await call_freedo(hass, "calibrate_gas_meter", {"value": 8000})


async def test_undo_gas_meter_reading(hass, client):
    with_meter(client, dict(METER))
    entry = await setup_entry(hass, client)
    meter = entity_id(hass, entry, "sensor", "boiler_gas_meter")
    assert hass.states.get(meter).state != "unavailable"

    await call_freedo(hass, "undo_gas_meter_reading", {})
    assert client.calls == [("undo_gas_meter",)]
    # The service refreshes the coordinator: the sensor is unavailable again.
    assert hass.states.get(meter).state == "unavailable"


async def add_second_hub(hass, client):
    """A second loaded freedo entry, on the same fake daemon client."""
    entry = MockConfigEntry(
        domain=DOMAIN, data={CONF_HOST: "127.0.0.2", CONF_PORT: 8080}, options={}
    )
    entry.add_to_hass(hass)
    with patch("custom_components.freedo.FreedoClient", return_value=client):
        assert await hass.config_entries.async_setup(entry.entry_id)
        await hass.async_block_till_done()
    return entry


async def test_calibrate_gas_meter_names_the_hub_with_two_hubs(hass, client):
    with_meter(client, dict(METER))
    await setup_entry(hass, client)
    second = await add_second_hub(hass, client)

    # Two loaded hubs without an id are ambiguous.
    with pytest.raises(ServiceValidationError) as err:
        await call_freedo(hass, "calibrate_gas_meter", {"value": 8786.5})
    assert err.value.translation_key == "several_hubs"

    # The id targets one hub: the shared client records one reading only.
    client.calls.clear()
    await call_freedo(
        hass, "calibrate_gas_meter", {"value": 8786.5, "config_entry_id": second.entry_id}
    )
    assert client.calls == [("gas_meter", 8786.5, None, None, False)]

    # An id that names no loaded hub is refused.
    with pytest.raises(ServiceValidationError) as err:
        await call_freedo(
            hass, "calibrate_gas_meter", {"value": 8786.5, "config_entry_id": "missing"}
        )
    assert err.value.translation_key == "unknown_hub"


async def test_undo_gas_meter_reading_names_the_hub(hass, client):
    with_meter(client, dict(METER))
    await setup_entry(hass, client)
    second = await add_second_hub(hass, client)

    client.calls.clear()
    await call_freedo(hass, "undo_gas_meter_reading", {"config_entry_id": second.entry_id})
    assert client.calls == [("undo_gas_meter",)]

    with pytest.raises(ServiceValidationError) as err:
        await call_freedo(hass, "undo_gas_meter_reading", {})
    assert err.value.translation_key == "several_hubs"


async def test_boiler_gas_numbers(hass, client):
    entry = await setup_entry(hass, client)
    registry = er.async_get(hass)
    high = entity_id(hass, entry, "number", "boiler_max_gas")
    low = entity_id(hass, entry, "number", "boiler_min_gas")
    # Both flows are unset.
    for number in (high, low):
        assert hass.states.get(number).state == "unknown"
        assert registry.async_get(number).entity_category == "config"
        assert hass.states.get(number).attributes["unit_of_measurement"] == "m³/h"
        assert hass.states.get(number).attributes["device_class"] == "volume_flow_rate"

    async def write(number, value):
        await hass.services.async_call(
            "number", "set_value", {ATTR_ENTITY_ID: number, "value": value}, blocking=True
        )
        await hass.async_block_till_done()

    await write(high, 2.63)
    await write(low, 0.3)
    assert client.calls == [
        ("patch_install", {"boiler_max_gas_m3h": 2.63}),
        ("patch_install", {"boiler_min_gas_m3h": 0.3}),
    ]
    assert float(hass.states.get(high).state) == 2.63
    assert float(hass.states.get(low).state) == 0.3

    # The daemon refuses a minimum that is not below the maximum.
    with pytest.raises(HomeAssistantError, match="not below boiler_max_gas_m3h"):
        await write(low, 2.63)
    assert float(hass.states.get(low).state) == 0.3
    # The range is the daemon range: 0 < max <= 60, 0 <= min <= 60.
    for number, value in ((high, 0), (high, 61), (low, -1), (low, 61)):
        with pytest.raises(ServiceValidationError):
            await write(number, value)
    assert len(client.calls) == 3


async def test_no_gas_numbers_on_a_daemon_without_the_gas_options(hass, client):
    del client.settings_json["boiler_max_gas_m3h"], client.settings_json["boiler_min_gas_m3h"]
    entry = await setup_entry(hass, client)
    assert has_entity(hass, entry, "number", "boiler_max_power")
    assert not has_entity(hass, entry, "number", "boiler_max_gas")
    assert not has_entity(hass, entry, "number", "boiler_min_gas")


# --- home level services ---


async def test_boost_all_zones(hass, client):
    await setup_entry(hass, client)
    await call_freedo(hass, "boost_all", {})
    # Every heating zone, not the hot water.
    assert sorted(client.calls) == [("boost", 1, True), ("boost", 2, True)]
    client.calls.clear()
    await call_freedo(hass, "boost_all", {"on": False})
    assert sorted(client.calls) == [("boost", 1, False), ("boost", 2, False)]


async def test_resume_all_zones(hass, client):
    await setup_entry(hass, client)
    # A target requested on the entity does not outlive the return to schedule.
    await call(hass, SERVICE_SET_TEMPERATURE, {ATTR_ENTITY_ID: BEDROOM, ATTR_TEMPERATURE: 22})
    client.calls.clear()
    await call_freedo(hass, "resume_all", {})
    assert sorted(client.calls) == [("auto", 1), ("auto", 2), ("auto", 9)]
    state = hass.states.get(BEDROOM)
    assert state.state == HVACMode.AUTO
    assert state.attributes[ATTR_TEMPERATURE] == 17


async def test_home_services_go_on_after_a_refused_zone(hass, client):
    await setup_entry(hass, client)
    real = client.zone_boost

    async def refuse_zone_one(zone, on):
        if zone == 1:
            raise FreedoApiError(400, "zone 1 elects no demand reporter")
        return await real(zone, on)

    client.zone_boost = refuse_zone_one
    with pytest.raises(HomeAssistantError, match="zone 1 elects no demand reporter"):
        await call_freedo(hass, "boost_all", {})
    # Zone 2 still got its boost.
    assert client.calls == [("boost", 2, True)]


async def test_home_services_without_a_loaded_zone(hass, client):
    entry = await setup_entry(hass, client)
    await hass.config_entries.async_unload(entry.entry_id)
    await hass.async_block_till_done()
    # The services belong to the integration, not to an entry.
    with pytest.raises(ServiceValidationError, match="No freedo zone"):
        await call_freedo(hass, "boost_all", {})
    with pytest.raises(ServiceValidationError, match="No freedo zone"):
        await call_freedo(hass, "resume_all", {})


def test_service_selectors_are_valid():
    """Home Assistant refuses a service field whose selector it cannot build."""
    from homeassistant.helpers import selector
    from homeassistant.util.yaml import load_yaml_dict

    services = load_yaml_dict(
        str(pathlib.Path(custom_components.freedo.__file__).parent / "services.yaml")
    )
    assert set(services) == {
        "boost", "boost_all", "calibrate_gas_meter", "copy_schedule", "get_schedule",
        "resume_all", "set_overlay", "set_schedule", "set_window", "undo_gas_meter_reading",
    }
    for spec in services.values():
        for field in ((spec or {}).get("fields") or {}).values():
            selector.validate_selector(field["selector"])
    # The schedule services take a heating zone and the hot water zone.
    for name in ("get_schedule", "set_schedule", "copy_schedule"):
        assert services[name]["target"]["entity"]["domain"] == ["climate", "water_heater"]
    # set_overlay takes a timer of up to 7 days.
    duration = services["set_overlay"]["fields"]["duration"]["selector"]["duration"]
    assert duration["enable_day"] is True
    # The end rule options are the zone overlay rules, and each has a label.
    options = services["set_overlay"]["fields"]["end_rule"]["selector"]["select"]["options"]
    assert set(options) == set(ZONE_OVERLAY_RULES)
    assert "until_auto_change" in options


def test_translations_match_strings():
    folder = pathlib.Path(custom_components.freedo.__file__).parent
    strings = json.loads((folder / "strings.json").read_text())
    assert json.loads((folder / "translations" / "en.json").read_text()) == strings
    assert set(strings["selector"]["overlay_end_rule"]["options"]) == set(ZONE_OVERLAY_RULES)
    assert set(strings["entity"]["select"]["offset_mode"]["state"]) == set(OFFSET_MODES)
