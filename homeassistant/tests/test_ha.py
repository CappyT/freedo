"""Home Assistant tests of the entities, the device registry and the
environment inputs, against a fake daemon client.

They run only with pytest-homeassistant-custom-component installed.
"""

import asyncio
import copy
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
from test_model import (  # noqa: E402
    BOILER,
    CIRCUITS,
    FLOW_CIRCUIT,
    HEALTH,
    HOME,
    INSTALL,
    INSTALL_ZONES,
    NODES,
    SCHEDULE,
    WEEK,
    ZONES,
)

DOMAIN = "freedo"
LIVING = "climate.living"
BEDROOM = "climate.bedroom"
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

    async def install(self):
        if self.settings_error is not None:
            raise self.settings_error
        return copy.deepcopy(self.settings_json)

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


@pytest.mark.parametrize(
    ("source", "targets", "message"),
    [
        (LIVING, [LIVING], "itself"),
        (LIVING, ["climate.nowhere"], "not a freedo heating zone"),
        (LIVING, ["switch.freedo_offline_schedule"], "not a freedo heating zone"),
        (BEDROOM, [LIVING], "no schedule"),
    ],
)
async def test_copy_schedule_refuses(hass, client, source, targets, message):
    await setup_entry(hass, client)
    with pytest.raises(ServiceValidationError, match=message):
        await call_freedo(hass, "copy_schedule", {ATTR_ENTITY_ID: source, "to": targets})
    assert not [c for c in client.calls if c[0] == "set_schedule"]


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


async def test_flow_limit_unknown_until_served(hass, client):
    with_flow_circuit(client, servedC=0)
    entry = await setup_entry(hass, client)
    assert hass.states.get(entity_id(hass, entry, "number", "circuit1_flow_limit")).state == "unknown"


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


# --- home level services ---

HOT_WATER = "water_heater.hot_water"


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
        "set_overlay", "set_schedule",
    }
    for spec in services.values():
        for field in ((spec or {}).get("fields") or {}).values():
            selector.validate_selector(field["selector"])
