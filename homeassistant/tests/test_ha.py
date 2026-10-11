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
from homeassistant.exceptions import HomeAssistantError, ServiceValidationError  # noqa: E402
from homeassistant.helpers import device_registry as dr, entity_registry as er  # noqa: E402
from homeassistant.util import dt as dt_util  # noqa: E402
from pytest_homeassistant_custom_component.common import (  # noqa: E402
    MockConfigEntry,
    async_fire_time_changed,
)

import custom_components.freedo  # noqa: E402,F401  # the target of the client patch
from custom_components.freedo.api import FreedoApiError, FreedoConnectionError  # noqa: E402
from custom_components.freedo.model import ZONE_OVERLAY_RULES  # noqa: E402
from test_model import (  # noqa: E402
    BOILER,
    CIRCUITS,
    ENERGY,
    FLOW_CIRCUIT,
    GAS,
    HEALTH,
    HOME,
    INSTALL,
    INSTALL_ZONES,
    NODES,
    HOT_WATER_WEEK,
    SCHEDULE,
    WEEK,
    ZONES,
)

DOMAIN = "freedo"
LIVING = "climate.living"
BEDROOM = "climate.bedroom"
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
        # GET /api/pair/window, None for a daemon without the route. The times are
        # left out while the window never opened.
        self.pair_window_json: dict | None = {"open": False, "responder": False, "joiners": []}

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

    async def pair_window(self):
        return copy.deepcopy(self.pair_window_json)

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
        mode = fields.get("device_overlay_end")
        # The daemon drops the duration of a rule other than timer and refuses
        # a timer without one.
        if mode is not None and mode != "timer":
            row["device_overlay_end_sec"] = None
        if mode == "timer" and not (fields.get("device_overlay_end_sec") or row["device_overlay_end_sec"]):
            raise FreedoApiError(400, "device_overlay_end: mode timer needs a duration_sec")
        row.update(fields)
        return {"rev": 9, "warnings": [], "zone": copy.deepcopy(row)}

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

    async def set_away(self, away):
        return {"away": away}

    async def set_display(self, node, orientation=None, notification=None):
        return {}


@pytest.fixture(autouse=True)
def auto_enable_custom_integrations(enable_custom_integrations):
    yield


@pytest.fixture
def client() -> FakeClient:
    return FakeClient()


async def setup_entry(hass, client, options=None) -> MockConfigEntry:
    entry = MockConfigEntry(
        domain=DOMAIN,
        data={CONF_HOST: "127.0.0.1", CONF_PORT: 8080},
        options=options or {},
    )
    entry.add_to_hass(hass)
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
    assert {"Living", "Bedroom", "Hot water"} <= names
    assert any(d.serial_number == "VA0000000001" for d in children)
    assert all(d.via_device_id == hub.id for d in children), [
        (d.name, d.via_device_id) for d in children
    ]
    assert "via_device" not in caplog.text


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
    await hass.config_entries.async_unload(entry.entry_id)

    # The daemon holds no installation (503 on GET /api/install/zones).
    client.install_json = {}
    entry = await setup_entry(hass, client)
    assert not has_entity(hass, entry, "switch", "zone1_early_start")
    assert not has_entity(hass, entry, "select", "zone1_device_overlay_end")


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


async def test_flow_limit_number_follows_the_curve(hass, client):
    with_flow_circuit(client, curve=True, computedC=41)
    entry = await setup_entry(hass, client)
    flow = entity_id(hass, entry, "number", "circuit1_flow_limit")
    # The daemon refuses a value on a circuit with a curve (409).
    assert hass.states.get(flow).state == "unavailable"

    client.boiler_json["flowLimit"]["circuits"][0]["curve"] = False
    await refresh(hass, entry)
    assert float(hass.states.get(flow).state) == 45


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
    for key in ("boiler_gas_volume", "boiler_gas_flow", "boiler_gas_estimate"):
        assert not has_entity(hass, entry, "sensor", key)


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
        "boost", "boost_all", "copy_schedule", "get_schedule", "resume_all",
        "set_overlay", "set_schedule", "set_window",
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
