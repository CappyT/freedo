"""Unit tests of the API model and the environment payload mapping.

The fixtures are synthetic and follow the JSON the daemon writes.
"""

import copy

import pytest

from model import (
    CORRECTION_CALIBRATION,
    CORRECTION_NONE,
    CORRECTION_SHIFT,
    boiler_energy,
    correction_patch,
    device_zones,
    boiler_gas,
    boiler_unsupported,
    device_option_label,
    energy_state,
    flow_circuit,
    flow_managed,
    fuel_unit,
    gas_state,
    node_calibration,
    node_child_lock,
    node_display,
    node_firmware,
    node_humidity,
    node_temperature,
    outdoor_payload,
    parse,
    room_temp_paused,
    room_temp_payload,
    room_temp_state,
    room_temp_zones,
    schedule_payload,
    schedule_programs,
    window_payload,
    window_zones,
    zone_boost,
    zone_correction,
    zone_current_temperature,
    zone_demand_pct,
    zone_has_program,
    zone_heating,
    zone_measuring_candidates,
    zone_mode,
    zone_offset_mode,
    zone_operation,
    zone_overlay_end,
    zone_overlay_rule,
    zone_program_temperature,
    zone_reported_setpoint,
    zone_target_source,
    zone_target_temperature,
)

HEALTH = {
    "ok": True,
    "devices": [
        {"device": "VA0000000001", "short": "aaaa", "kind": "valve", "label": "Living",
         "batteryMV": 2100, "lowBattery": True, "fault": False, "faultWord": 0,
         "offline": False, "lastSeen": "2026-01-01T00:00:00Z", "fallbackMode": 0,
         "commsWarnings": ["noConfigAnswer"]},
        {"device": "RU0000000001", "short": "cccc", "kind": "thermostat", "label": "",
         "batteryMV": 0, "lowBattery": False, "fault": True, "faultWord": 4,
         "offline": True, "lastSeen": None, "fallbackMode": 0, "commsWarnings": None},
    ],
    "zoneFallback": [{"zone": 2, "mode": 3, "name": "frost", "source": "cccc",
                      "since": "2026-01-01T00:00:00Z"}],
}
ZONES = {
    "zones": {"1": ["aaaa", "bbbb"]},
    "declared": [
        {"number": 1, "name": "Living", "kind": "heating", "members": ["aaaa", "bbbb"],
         "circuit": 1, "reporter": "aaaa", "measuringDevice": "bbbb", "writer": "aaaa",
         "window": {"open": True, "source": "external", "since": "2026-01-01T00:00:00Z",
                    "deadline": "2026-01-01T02:00:00Z", "undelivered": []},
         "target": {"targetC": 25.0, "source": "boost", "programC": 20.0, "programOff": False,
                    "hasProgram": True, "boost": True, "known": True,
                    "overlay": {"kind": "boost", "on": True, "setpointC": 25.0,
                                "endsAt": "2026-01-01T00:30:00Z"}},
         "externalTemp": {"tempC": 21.4, "at": "2026-01-01T00:00:00Z", "ageSec": 120,
                          "source": "Living thermometer", "stale": False, "inUse": True, "deviceC": 20.9, "wantedC": 0.4,
                          "shiftC": 0.2, "offsetC": 0.0},
         "autoOffset": {"enabled": False, "intervalSec": 86400, "lastWriteAt": None,
                        "nextAt": None, "filteredDiffC": 0.0, "sampleSpanSec": 0,
                        "stopReason": None}},
        {"number": 2, "name": "Bedroom", "kind": "heating", "members": ["dddd"],
         "window": {"open": False, "source": None, "since": None, "deadline": None,
                    "undelivered": []},
         "target": {"targetC": 17.0, "source": "program", "programC": 17.0,
                    "programOff": False, "hasProgram": True, "boost": False, "known": True,
                    "overlay": None},
         "externalTemp": {"tempC": None, "at": None, "ageSec": None, "source": None,
                          "stale": False,
                          "inUse": False, "deviceC": None, "wantedC": None, "shiftC": 0.0,
                          "offsetC": 0.0},
         "autoOffset": {"enabled": False, "intervalSec": 43200, "lastWriteAt": None,
                        "nextAt": None, "filteredDiffC": 0.0, "sampleSpanSec": 0,
                        "stopReason": "the filtered difference did not shrink after the last write"}},
        {"number": 9, "name": "DHW", "kind": "hotwater", "members": ["cccc"],
         "target": {"targetC": None, "source": "overlay", "programC": 55.0,
                    "programOff": False, "hasProgram": True, "boost": False, "known": True,
                    "on": False,
                    "overlay": {"kind": "manual", "on": False, "setpointC": None,
                                "endsAt": None}}},
    ],
    "demand": [
        {"Number": 1, "Name": "Living", "Kind": "heating", "Circuit": 1, "DemandPct": 40,
         "HeatPct": 40, "TempC": 20.5, "Reporter": "aaaa", "Source": "aaaa",
         "UpdatedUnix": 1700000000, "FallbackMode": 0, "Fallback": "none",
         "FallbackSinceUnix": 0},
        {"Number": 2, "Name": "Bedroom", "Kind": "heating", "Circuit": 1, "DemandPct": 0,
         "HeatPct": 0, "TempC": 0, "Reporter": "", "Source": "", "UpdatedUnix": 0,
         "FallbackMode": 3, "Fallback": "frost", "FallbackSinceUnix": 1700000000},
    ],
}
NODES = [
    {"Short": "aaaa", "TempC": 20.9, "SetpointC": 21, "Mode": "manual",
     "HumidityPct": 0, "BatteryMV": 2100, "BatteryPct": 20, "LastSeenUnix": 1700000000,
     "Telemetry": {"temp_C": "20.90C", "calib_state": "2", "calib_error": "0",
                   "fw_run_version": "2.5"},
     "Display": {"orientation": "vertical", "notification": False}, "ChildLock": False},
    {"Short": "bbbb", "TempC": 20.7, "SetpointC": 0, "Mode": "",
     "HumidityPct": 48.5, "BatteryMV": 2900, "BatteryPct": 90, "LastSeenUnix": 1700000000,
     "Telemetry": {"temp_C": "20.70C", "humidity_pct": "48.50%"}},
    {"Short": "dddd", "TempC": 18.2, "SetpointC": 17, "Mode": "auto",
     "HumidityPct": 0, "BatteryMV": 0, "BatteryPct": 0, "LastSeenUnix": 1700000000,
     "Telemetry": {"temp_C": "18.20C"}},
    {"Short": "eeee", "TempC": 0, "SetpointC": 0, "Mode": "", "LastSeenUnix": 0,
     "Telemetry": {}},
]
BOILER = {"Present": True, "FlowC": 45.0, "DHWC": 48.0,
          "flowLimit": {"outdoor": None, "circuits": [], "defaultC": 48}}
HOME = {"away": True, "scheduled": {"1": 20}}
# GET /api/install/zones: zone 1 corrects its temperature by +0.5 C (mode setpoint).
INSTALL_ZONES = {
    "rev": 7,
    "zones": [
        {"number": 1, "name": "Living", "kind": "heating", "members": ["VA0000000001"],
         "circuit": 1, "reporter": "VA0000000001", "measuring_device": None,
         "offset_mode": "setpoint", "setpoint_offset_c": 0.5,
         "auto_offset": False, "auto_offset_interval_sec": 86400,
         "open_window_detection": True, "open_window_duration_sec": 900,
         "early_start": False, "device_overlay_end": "until_next",
         "device_overlay_end_sec": None, "away_temperature_c": 21.5,
         "ha_area": None},
        {"number": 2, "name": "Bedroom", "kind": "heating", "members": ["VA0000000002"],
         "circuit": 1, "reporter": None, "measuring_device": None,
         "offset_mode": "config", "setpoint_offset_c": None,
         "auto_offset": False, "auto_offset_interval_sec": 43200,
         "open_window_detection": False, "open_window_duration_sec": 600,
         "early_start": True, "device_overlay_end": "timer",
         "device_overlay_end_sec": 1800, "away_temperature_c": None,
         "ha_area": None},
        {"number": 9, "name": "DHW", "kind": "hotwater", "members": ["RU0000000001"],
         "circuit": None, "reporter": None, "measuring_device": None,
         "offset_mode": None, "setpoint_offset_c": None,
         "open_window_detection": True, "open_window_duration_sec": 900,
         "early_start": False, "device_overlay_end": None,
         "device_overlay_end_sec": None, "away_temperature_c": None,
         "ha_area": None},
    ],
}

# GET /api/install/devices: the devices placed in each room. The wall thermostat
# cccc sits in the hot water room, not in a heating room.
INSTALL_DEVICES = {
    "rev": 7,
    "devices": [
        {"eui": "0000000000000001", "short": "aaaa", "kind": "valve",
         "serial": "VA0000000001", "label": "Living", "notes": "", "zone": 1},
        {"eui": "0000000000000002", "short": "bbbb", "kind": "valve",
         "serial": "VA0000000002", "label": "", "notes": "", "zone": 1},
        {"eui": "0000000000000003", "short": "dddd", "kind": "valve",
         "serial": "VA0000000004", "label": "Bedroom", "notes": "", "zone": 2},
        {"eui": "0000000000000004", "short": "cccc", "kind": "thermostat",
         "serial": "RU0000000001", "label": "", "notes": "", "zone": 9},
    ],
}

# GET /api/install/circuits and the flow limit view of GET /api/boiler for circuit 1.
CIRCUITS = {"rev": 7, "circuits": [{"id": 1, "name": "Heating", "heat_source": "0000000000000001"}]}

# GET /api/presets: the program presets, the active one flagged.
PRESETS = {
    "presets": [
        {"id": 1, "name": "Winter", "active": True},
        {"id": 2, "name": "Summer", "active": False},
    ]
}
FLOW_CIRCUIT = {"id": 1, "curve": False, "computedC": None, "servedC": 45, "changedAt": None,
                "fetched": True, "adapt": False, "defaultCurve": False, "reductionC": None}

# GET /api/zones/{n}/schedule of zone 1.
SCHEDULE = {
    "zone": 1,
    "members": ["aaaa", "bbbb"],
    "programs": [
        {"days": ["mon", "tue", "wed", "thu", "fri"], "mask": 31,
         "entries": [{"minute": 0, "setpoint_c": 17.0, "enabled": True},
                     {"minute": 390, "setpoint_c": 21.0, "enabled": True},
                     {"minute": 1320, "setpoint_c": 17.0, "enabled": False}]},
        {"days": ["sat", "sun"], "mask": 96,
         "entries": [{"minute": 0, "setpoint_c": 18.5, "enabled": True}]},
    ],
}

# GET /api/install: the installation options the integration reads. The boiler
# power is unset: the maximum is null and the minimum 0. The gas flows are unset,
# and so is the flow limit. The flow limit curves and the adaptation are off.
INSTALL = {"rev": 7, "home_id": 1234567, "offline_schedule": True,
           "utc_offset_min": None, "timezone": "",
           "flow_limit_c": None, "flow_auto_adapt": False, "flow_default_curve": False,
           "boiler_max_power_kw": None, "boiler_min_power_kw": 0.0,
           "boiler_max_gas_m3h": None, "boiler_min_gas_m3h": None}

# The energy view of GET /api/boiler, with the boiler power set.
ENERGY = {"running": True, "reason": None, "powerKW": 12.4, "totalKWh": 1234.567,
          "maxPowerKW": 24.0, "minPowerKW": 3.0, "holdSec": 300,
          "reportedAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"}

# The gas view of GET /api/boiler energy.gas, with both flows set.
GAS = {"running": True, "reason": None, "flowM3H": 1.465, "totalM3": 321.654,
       "maxFlowM3H": 2.63, "minFlowM3H": 0.3}


def data():
    return parse(HEALTH, ZONES, NODES, BOILER, HOME)


def _zones_with(number, **fields):
    """ZONES with the demand entry of one zone replaced by fields."""
    zones = dict(ZONES)
    zones["demand"] = [dict(v) for v in ZONES["demand"]]
    for view in zones["demand"]:
        if view["Number"] == number:
            view.update(fields)
    return zones


def test_parse():
    d = data()
    assert sorted(d.zones) == [1, 2, 9]
    assert d.zones[1].members == ("aaaa", "bbbb")
    assert d.zones[1].measuring == "bbbb"
    assert d.hot_water_zone.number == 9
    assert [z.number for z in d.heating_zones()] == [1, 2]
    assert d.windows[1]["source"] == "external"
    assert 9 not in d.windows
    assert d.devices["aaaa"]["lowBattery"] is True
    assert d.nodes["dddd"]["Mode"] == "auto"
    assert list(d.fallback) == [2]
    assert d.away is True
    assert d.offline_schedule is None


def test_install_devices():
    d = parse(HEALTH, ZONES, NODES, BOILER, HOME, install_devices=INSTALL_DEVICES)
    assert d.install_devices["bbbb"]["serial"] == "VA0000000002"
    # The candidates of a room are its own devices, by label then serial.
    rows = zone_measuring_candidates(d, d.zones[1])
    assert [r["short"] for r in rows] == ["aaaa", "bbbb"]
    assert [device_option_label(r) for r in rows] == ["Living (VA0000000001)", "VA0000000002"]
    # A device placed in another room is not a candidate of this one.
    assert [r["short"] for r in zone_measuring_candidates(d, d.zones[2])] == ["dddd"]
    assert zone_measuring_candidates(d, d.zones[2])[0]["short"] != "cccc"


def test_device_option_label_needs_no_label():
    # A device without a label shows its serial, and its short address without one.
    assert device_option_label({"label": "Living", "serial": "VA1"}) == "Living (VA1)"
    assert device_option_label({"label": "", "serial": "VA2"}) == "VA2"
    assert device_option_label({"short": "bbbb"}) == "bbbb"


def test_room_temp_paused():
    assert room_temp_paused([1, "2", "x", None]) == {1, 2}
    assert room_temp_paused(None) == set()


def test_pair_window():
    window = {"open": True, "until": "2026-01-01T00:10:00Z", "responder": False, "joiners": []}
    assert parse(HEALTH, ZONES, NODES, BOILER, HOME, pair_window=window).pair_window == window
    # A daemon without the route sends none.
    assert data().pair_window is None


def test_offline_schedule():
    for value in (True, False):
        settings = dict(INSTALL, offline_schedule=value)
        d = parse(HEALTH, ZONES, NODES, BOILER, HOME, INSTALL_ZONES, settings)
        assert d.offline_schedule is value
    # No installation (503 read as {}) or a field of another type: unknown.
    assert parse(HEALTH, ZONES, NODES, BOILER, HOME, INSTALL_ZONES, {}).offline_schedule is None
    assert parse(HEALTH, ZONES, NODES, BOILER, HOME, None, {"offline_schedule": 1}).offline_schedule is None


def test_zone_overlay_rule():
    d = data()
    living, bedroom, hot_water = d.zones[1], d.zones[2], d.zones[9]
    # No overlay: no rule. A daemon that sends no rule: the kind stands in when
    # it names a rule, and a boost has none.
    assert zone_overlay_rule(d, bedroom) is None
    assert zone_overlay_rule(d, living) is None
    assert zone_overlay_rule(d, hot_water) == "manual"
    # The rule the daemon sends wins over the kind.
    for rule in ("manual", "timer", "until_next", "until_auto_change"):
        zones = copy.deepcopy(ZONES)
        zones["declared"][0]["target"]["overlay"]["rule"] = rule
        assert zone_overlay_rule(parse(HEALTH, zones, NODES, BOILER, HOME), living) == rule


def test_zone_values():
    d = data()
    living, bedroom = d.zones[1], d.zones[2]
    # The zone report (tag 4060) wins over the device readings.
    assert zone_current_temperature(d, living) == 20.5
    # No zone report: the first heard device of the zone.
    assert zone_current_temperature(d, bedroom) == 18.2
    # The target comes from the zone block the daemon holds, not from a device.
    assert zone_target_temperature(d, living) == 25.0
    assert zone_mode(d, living) == "boost"
    assert zone_boost(d, living) is True
    assert zone_overlay_end(d, living) == "2026-01-01T00:30:00Z"
    assert zone_target_source(d, living) == "boost"
    assert zone_target_temperature(d, bedroom) == 17.0
    assert zone_mode(d, bedroom) == "auto"
    assert zone_boost(d, bedroom) is False
    assert zone_overlay_end(d, bedroom) is None
    assert zone_program_temperature(d, bedroom) == 17.0
    assert zone_demand_pct(d, living) == 40
    assert zone_demand_pct(d, bedroom) is None


def test_zone_heating_follows_the_demand():
    d = parse({}, ZONES, NODES, {}, {})
    living, bedroom = d.zones[1], d.zones[2]
    assert zone_heating(d, living) is True
    # Zone 2 has not reported a zone report.
    assert zone_heating(d, bedroom) is None
    d.demand[2].update(UpdatedUnix=1700000000, DemandPct=0)
    assert zone_heating(d, bedroom) is False
    d.demand[1]["DemandPct"] = 1
    assert zone_heating(d, living) is True


def test_zone_reporter_order():
    # No measuring device: the demand reporter comes before the other members.
    zones = {"declared": [{"number": 1, "name": "Living", "kind": "heating",
                           "members": ["aaaa", "bbbb"], "reporter": "bbbb",
                           "writer": "aaaa"}]}
    d = parse({}, zones, NODES, {}, {})
    assert d.zones[1].reporter == "bbbb"
    assert zone_current_temperature(d, d.zones[1]) == 20.7


def test_zone_temperature_waits_for_4060():
    # A 40a0 or 4160 without 4060 sets UpdatedUnix and leaves TempC at 0.
    zones = _zones_with(1, TempC=0, UpdatedUnix=1700000000)
    d = parse(HEALTH, zones, NODES, BOILER, HOME)
    assert zone_current_temperature(d, d.zones[1]) == 20.7
    # On the corrected scale the daemon adds the shift to the missing 0.
    zones = _zones_with(1, TempC=0.5, UpdatedUnix=1700000000)
    d = parse(HEALTH, zones, NODES, BOILER, HOME, INSTALL_ZONES)
    assert zone_current_temperature(d, d.zones[1]) == 21.2
    # A reported 4060 comes corrected from the daemon.
    zones = _zones_with(1, TempC=21.0, UpdatedUnix=1700000000)
    d = parse(HEALTH, zones, NODES, BOILER, HOME, INSTALL_ZONES)
    assert zone_current_temperature(d, d.zones[1]) == 21.0


def test_member_temperature_corrected_scale():
    zones = _zones_with(1, TempC=0.5, UpdatedUnix=0)
    d = parse(HEALTH, zones, NODES, BOILER, HOME, INSTALL_ZONES)
    assert d.zones[1].shift_c == 0.5
    assert d.zones[2].shift_c == 0.0
    # bbbb reports 20.7 on the wire scale.
    assert zone_current_temperature(d, d.zones[1]) == 21.2
    assert zone_current_temperature(d, d.zones[2]) == 18.2


def test_member_without_012d():
    # dddd is heard but never sent 012d: its TempC 0 is no reading.
    nodes = [dict(n) for n in NODES]
    nodes[2]["TempC"] = 0
    nodes[2]["Telemetry"] = {"battery_mV": "3000"}
    d = parse(HEALTH, ZONES, nodes, BOILER, HOME)
    assert zone_current_temperature(d, d.zones[2]) is None
    # Zone 1: the measuring device has no reading, the reporter has one.
    nodes[1]["TempC"] = 0
    nodes[1]["Telemetry"] = None
    zones = _zones_with(1, TempC=0, UpdatedUnix=0)
    d = parse(HEALTH, zones, nodes, BOILER, HOME)
    assert zone_current_temperature(d, d.zones[1]) == 20.9


def test_zone_never_heard():
    d = parse({}, {"declared": [{"number": 4, "name": "Attic", "kind": "heating",
                                 "members": ["eeee"]}]}, NODES, {}, {})
    zone = d.zones[4]
    assert zone_current_temperature(d, zone) is None
    assert zone_target_temperature(d, zone) is None
    assert zone_mode(d, zone) is None
    assert zone_boost(d, zone) is False


def test_zone_block_unknown():
    zones = {"declared": [{"number": 4, "name": "Attic", "kind": "heating", "members": ["eeee"],
                           "target": {"targetC": None, "source": None, "programC": 18.0,
                                      "programOff": False, "boost": False, "known": False,
                                      "overlay": None}}]}
    d = parse({}, zones, NODES, {}, {})
    zone = d.zones[4]
    assert zone_mode(d, zone) is None
    assert zone_target_temperature(d, zone) is None
    assert zone_program_temperature(d, zone) == 18.0


def _with_hot_water_target(target):
    zones = dict(ZONES)
    zones["declared"] = [dict(z) for z in ZONES["declared"]]
    zones["declared"][2]["target"] = target
    return parse(HEALTH, zones, NODES, BOILER, HOME)


def test_zone_operation():
    d = data()
    hot_water = d.hot_water_zone
    assert zone_operation(d, hot_water) == "off"
    assert zone_target_temperature(d, hot_water) is None
    assert zone_program_temperature(d, hot_water) == 55.0
    d = _with_hot_water_target({
        "targetC": 50.0, "source": "overlay", "programC": 55.0, "programOff": False,
        "boost": False, "known": True, "on": True,
        "overlay": {"kind": "until_next", "on": True, "setpointC": 50.0,
                    "endsAt": "2026-01-01T04:00:00Z"}})
    assert zone_operation(d, d.hot_water_zone) == "heat"
    assert zone_target_temperature(d, d.hot_water_zone) == 50.0
    assert zone_overlay_end(d, d.hot_water_zone) == "2026-01-01T04:00:00Z"
    d = _with_hot_water_target({
        "targetC": 55.0, "source": "program", "programC": 55.0, "programOff": False,
        "boost": False, "known": True, "on": True, "overlay": None})
    assert zone_operation(d, d.hot_water_zone) == "auto"


def test_zone_has_program():
    d = data()
    assert [zone_has_program(d, d.zones[n]) for n in (1, 2, 9)] == [True, True, True]
    # A zone with no program reads false, and a daemon that sends no field reads
    # unknown.
    no_program = {"targetC": None, "source": "program", "programC": None,
                  "programOff": False, "hasProgram": False, "boost": False, "known": True,
                  "on": True, "overlay": None}
    d = _with_hot_water_target(no_program)
    assert zone_has_program(d, d.hot_water_zone) is False
    assert zone_target_temperature(d, d.hot_water_zone) is None
    assert zone_program_temperature(d, d.hot_water_zone) is None
    del no_program["hasProgram"]
    d = _with_hot_water_target(no_program)
    assert zone_has_program(d, d.hot_water_zone) is None


def test_zone_reported_setpoint():
    def setpoint(value, on=True):
        target = {"targetC": None, "source": "program", "programC": None, "programOff": False,
                  "hasProgram": False, "boost": False, "known": True, "on": on, "overlay": None}
        zones = dict(ZONES)
        zones["declared"] = [dict(z) for z in ZONES["declared"]]
        zones["declared"][2]["target"] = target
        boiler = dict(BOILER) if value is None else dict(BOILER, DHWSetpointC=value)
        d = parse(HEALTH, zones, NODES, boiler, HOME)
        return zone_reported_setpoint(d, d.hot_water_zone)

    assert setpoint(52.0) == 52.0
    # Nothing reported, a zone that is off and a value outside the range the
    # daemon accepts as a hot water setpoint.
    assert setpoint(None) is None
    assert setpoint(52.0, on=False) is None
    assert setpoint(29.9) is None
    assert setpoint(65.1) is None
    assert setpoint(30.0) == 30.0
    assert setpoint(65.0) == 65.0


def test_room_off_operation():
    d = data()
    assert zone_operation(d, d.zones[1]) == "heat"
    assert zone_operation(d, d.zones[2]) == "auto"
    zones = dict(ZONES)
    zones["declared"] = [dict(z) for z in ZONES["declared"]]
    zones["declared"][1]["target"] = {
        "targetC": None, "source": "overlay", "programC": 17.0, "programOff": False,
        "boost": False, "known": True, "on": False,
        "overlay": {"kind": "until_next", "on": False, "setpointC": None,
                    "endsAt": "2026-01-01T06:00:00Z"}}
    d = parse(HEALTH, zones, NODES, BOILER, HOME)
    bedroom = d.zones[2]
    assert zone_operation(d, bedroom) == "off"
    assert zone_mode(d, bedroom) == "until_next"
    assert zone_target_temperature(d, bedroom) is None
    assert zone_overlay_end(d, bedroom) == "2026-01-01T06:00:00Z"


def test_outdoor_payload():
    assert outdoor_payload("4.25", "°C") == {"tempC": 4.25}
    assert outdoor_payload("-3", None) == {"tempC": -3.0}
    assert outdoor_payload("50", "°F") == {"tempC": 10.0}
    for state in ("unavailable", "unknown", "", None, "nan"):
        assert outdoor_payload(state, "°C") is None
    # Outside the -60..60 bound the daemon answers 400: never pushed.
    assert outdoor_payload("61", "°C") is None


def test_room_temp_payload():
    assert room_temp_payload("21.456", "°C") == {"tempC": 21.46}
    assert room_temp_payload("-3", None) == {"tempC": -3.0}
    assert room_temp_payload("68.9", "°F") == {"tempC": 20.5}
    assert room_temp_payload("294.15", "K") == {"tempC": 21.0}
    for state in ("unavailable", "unknown", "", None, "nan", "inf"):
        assert room_temp_payload(state, "°C") is None
    # The daemon answers 400 outside -10..50 C: never pushed.
    assert room_temp_payload("-10", "°C") == {"tempC": -10.0}
    assert room_temp_payload("50", "°C") == {"tempC": 50.0}
    assert room_temp_payload("-10.1", "°C") is None
    assert room_temp_payload("50.1", "°C") is None


def test_room_temp_zones():
    mapping = {"1": "sensor.a", 2: "sensor.b", "x": "sensor.bad", "3": "", "4": None}
    assert room_temp_zones(mapping) == {1: "sensor.a", 2: "sensor.b"}


def test_window_payload():
    assert window_payload(["off", "off"]) == {"open": False}
    assert window_payload(["off", "on"]) == {"open": True}
    assert window_payload(["unavailable", "off"]) == {"open": False}
    assert window_payload(["unavailable", "on"]) == {"open": True}
    assert window_payload(["unavailable", "unknown", None]) is None
    assert window_payload([]) is None


def test_window_zones():
    mapping = {"binary_sensor.b": 1, "binary_sensor.a": "1", "binary_sensor.c": 2,
               "binary_sensor.bad": "x"}
    assert window_zones(mapping) == {1: ["binary_sensor.a", "binary_sensor.b"],
                                     2: ["binary_sensor.c"]}


def test_boiler_unsupported():
    assert boiler_unsupported(data()) == set()
    boiler = {"Present": True, "FlowC": 45.0, "ReturnC": None, "OutsideC": None,
              "Unsupported": ["outside_C", "return_water_C"]}
    d = parse(HEALTH, ZONES, NODES, boiler, HOME)
    assert boiler_unsupported(d) == {"outside_C", "return_water_C"}


def test_node_display():
    d = data()
    assert node_display(d, "aaaa") == {"orientation": "vertical", "notification": False}
    assert node_display(d, "bbbb") is None
    assert node_display(d, "ffff") is None


def test_valve_device_values():
    d = data()
    # A valve serves the child lock, the other devices serve null or nothing.
    assert node_child_lock(d, "aaaa") is False
    assert node_child_lock(d, "bbbb") is None
    assert node_calibration(d, "aaaa") == ("done", 0)
    assert node_calibration(d, "bbbb") == (None, None)
    nodes = [dict(NODES[0], Telemetry={"calib_state": "3", "calib_error": "4"})]
    assert node_calibration(parse(HEALTH, ZONES, nodes, BOILER, HOME), "aaaa") == ("failed", 4)
    # A state the daemon does not name is unknown.
    nodes = [dict(NODES[0], Telemetry={"calib_state": "9"})]
    assert node_calibration(parse(HEALTH, ZONES, nodes, BOILER, HOME), "aaaa") == (None, None)


def test_zone_settings_rows():
    d = parse(HEALTH, ZONES, NODES, BOILER, HOME, INSTALL_ZONES)
    assert sorted(d.zone_settings) == [1, 2, 9]
    assert d.zone_settings[1]["offset_mode"] == "setpoint"
    assert zone_offset_mode(d, d.zones[1]) == "setpoint"
    assert zone_offset_mode(d, d.zones[2]) == "config"
    # No installation read (503): no row.
    assert zone_offset_mode(data(), d.zones[1]) is None


def _room_temp(**fields):
    """Zone 1 of the fixture with its external temperature view changed."""
    zones = copy.deepcopy(ZONES)
    zones["declared"][0]["externalTemp"].update(fields)
    return parse(HEALTH, zones, NODES, BOILER, HOME, INSTALL_ZONES)


def test_room_temp_state():
    d = parse(HEALTH, ZONES, NODES, BOILER, HOME, INSTALL_ZONES)
    assert sorted(d.room_temps) == [1, 2]
    assert room_temp_state(d, d.zones[1]) == "active"
    # Zone 2 is in mode config: the daemon takes no reading.
    assert room_temp_state(d, d.zones[2]) == "no_correction"
    # The hot water zone has no view.
    assert room_temp_state(d, d.zones[9]) is None

    no_reading = {"tempC": None, "at": None, "ageSec": None, "inUse": False}
    assert room_temp_state(_room_temp(**no_reading), d.zones[1]) == "no_reading"
    stale = _room_temp(inUse=False, stale=True, ageSec=1900)
    assert room_temp_state(stale, d.zones[1]) == "stale"
    assert room_temp_state(_room_temp(inUse=False), d.zones[1]) is None
    # No installation read: the mode is unknown, and so is the cause.
    assert room_temp_state(parse(HEALTH, ZONES, NODES, BOILER, HOME), d.zones[2]) is None
    # Mode config with auto_offset on takes the reading: the cause is not the
    # mode, and no reading came yet.
    auto = copy.deepcopy(INSTALL_ZONES)
    next(r for r in auto["zones"] if r["number"] == 2)["auto_offset"] = True
    d2 = parse(HEALTH, ZONES, NODES, BOILER, HOME, auto)
    assert room_temp_state(d2, d2.zones[2]) == "no_reading"


WEEK = [
    {"days": ["mon", "tue", "wed", "thu", "fri"],
     "entries": [{"time": "00:00", "temperature": 17},
                 {"time": "06:30", "temperature": 21.5, "enabled": True},
                 {"time": "22:00", "temperature": 17, "enabled": False}]},
    {"days": ["Sat", "sun"], "entries": [{"time": "00:00:00", "temperature": 18}]},
]


def test_schedule_payload():
    assert schedule_payload(WEEK) == [
        {"days": ["mon", "tue", "wed", "thu", "fri"],
         "entries": [{"minute": 0, "setpoint_c": 17, "enabled": True},
                     {"minute": 390, "setpoint_c": 21.5, "enabled": True},
                     {"minute": 1320, "setpoint_c": 17, "enabled": False}]},
        {"days": ["sat", "sun"], "entries": [{"minute": 0, "setpoint_c": 18, "enabled": True}]},
    ]


HOT_WATER_WEEK = [
    {"days": ["mon", "tue", "wed", "thu", "fri", "sat", "sun"],
     "entries": [{"time": "00:00", "temperature": 30},
                 {"time": "06:00", "temperature": 55.5},
                 {"time": "22:00", "temperature": 65, "enabled": False}]},
]


def test_schedule_payload_takes_the_range_of_the_zone():
    body = schedule_payload(HOT_WATER_WEEK, 30, 65)
    assert [e["setpoint_c"] for e in body[0]["entries"]] == [30, 55.5, 65]
    # The room range is the default: the hot water week is above it.
    with pytest.raises(ValueError, match="within 5..25"):
        schedule_payload(HOT_WATER_WEEK)
    # The room week is below the hot water range.
    with pytest.raises(ValueError, match=r"entries\[0\].temperature must be within 30..65"):
        schedule_payload(WEEK, 30, 65)
    for temp in (29.9, 65.1):
        week = _week(**{"0.entries.1.temperature": temp})
        with pytest.raises(ValueError, match="within 30..65"):
            schedule_payload(week, 30, 65)


def test_schedule_view_round_trips():
    programs = schedule_programs(SCHEDULE)
    assert programs[0]["entries"][1] == {"time": "06:30", "temperature": 21.0, "enabled": True}
    assert programs[0]["entries"][2]["enabled"] is False
    body = schedule_payload(programs)
    assert [p["days"] for p in body] == [p["days"] for p in SCHEDULE["programs"]]
    assert body[0]["entries"][1] == {"minute": 390, "setpoint_c": 21.0, "enabled": True}
    assert schedule_programs({}) == []


def _week(**change):
    """WEEK with the fields of one entry or program replaced."""
    import copy

    week = copy.deepcopy(WEEK)
    for path, value in change.items():
        target = week
        *head, last = path.split(".")
        for key in head:
            target = target[int(key)] if key.isdigit() else target[key]
        if last.isdigit():
            target[int(last)] = value
        else:
            target[last] = value
    return week


@pytest.mark.parametrize(
    ("programs", "message"),
    [
        ([], "at least one program"),
        ("mon", "list"),
        (_week(**{"0.days": []}), "list of days"),
        (_week(**{"0.days": ["mon", "funday"]}), "not a day"),
        (_week(**{"1.days": ["sat"]}), "all seven days"),
        (_week(**{"1.days": ["fri", "sat", "sun"]}), "more than one program"),
        (_week(**{"0.entries": []}), "at least one entry"),
        (_week(**{"0.entries.0.time": "00:05"}), "must be 00:00"),
        (_week(**{"0.entries.1.time": "00:00"}), "later than the entry before"),
        (_week(**{"0.entries.2.time": "06:30"}), "later than the entry before"),
        (_week(**{"0.entries.1.time": "25:00"}), "not a time of day"),
        (_week(**{"0.entries.1.time": "06:30:15"}), "whole minute"),
        (_week(**{"0.entries.1.time": 390}), "as HH:MM"),
        (_week(**{"0.entries.1.temperature": 26}), "within 5..25"),
        (_week(**{"0.entries.1.temperature": 4.9}), "within 5..25"),
        (_week(**{"0.entries.1.temperature": 21.123}), "two decimal places"),
        (_week(**{"0.entries.1.temperature": "21"}), "as a number"),
        (_week(**{"0.entries.1.temperature": True}), "as a number"),
        (_week(**{"0.entries.1.enabled": "yes"}), "true or false"),
        (_week(**{"0.entries.1.mode": "x"}), "unknown fields: mode"),
        (_week(**{"0.zone": 1}), "unknown fields: zone"),
        ([5], "must be a mapping"),
    ],
)
def test_schedule_payload_refuses(programs, message):
    with pytest.raises(ValueError, match=message):
        schedule_payload(programs)


def _day_at(*minutes):
    """One program for the whole week with an entry at each minute of the day."""
    entries = [
        {"time": f"{minute // 60:02d}:{minute % 60:02d}", "temperature": 17} for minute in minutes
    ]
    return [{"days": ["mon", "tue", "wed", "thu", "fri", "sat", "sun"], "entries": entries}]


def test_schedule_day_size_limit():
    # The daemon encodes a day as protobuf and refuses more than 255 bytes. An
    # entry takes 9 bytes while its minute fits one varint byte (minute < 128)
    # and 10 bytes after that, and the day mask field takes 2.
    early_ok = _day_at(*range(28))
    assert 2 + 9 * 28 == 254
    assert schedule_payload(early_ok)[0]["entries"][-1]["minute"] == 27
    # 7 early and 19 late entries are 255 bytes: the limit itself passes.
    exact = _day_at(*range(7), *range(128, 147))
    assert 2 + 7 * 9 + 19 * 10 == 255
    assert len(schedule_payload(exact)[0]["entries"]) == 26


@pytest.mark.parametrize(
    ("minutes", "size"),
    [
        (range(29), 263),
        ((*range(7), *range(128, 148)), 265),
        ((0, *range(128, 153)), 261),
    ],
)
def test_schedule_day_size_refused(minutes, size):
    with pytest.raises(ValueError, match=rf"programs\[0\] encodes to {size} bytes.*255"):
        schedule_payload(_day_at(*minutes))


def test_device_readings():
    d = data()
    assert node_firmware(d, "aaaa") == "2.5"
    assert node_firmware(d, "bbbb") is None
    # A report without a running version falls back to the build hash.
    build = [dict(NODES[0])]
    build[0]["Telemetry"] = {"fw_build_hash": "73b9b52"}
    assert node_firmware(parse(HEALTH, ZONES, build, BOILER, HOME), "aaaa") == "73b9b52"
    # The wire scale: no zone correction.
    assert node_temperature(d, "aaaa") == 20.9
    assert node_temperature(d, "eeee") is None
    assert node_temperature(d, "zzzz") is None
    assert node_humidity(d, "bbbb") == 48.5
    # A device that never sent 0135 has no humidity, not 0 %.
    assert node_humidity(d, "aaaa") is None


def test_circuit_sources_and_flow_view():
    boiler = {**BOILER, "flowLimit": {"circuits": [FLOW_CIRCUIT], "defaultC": 48}}
    d = parse(HEALTH, ZONES, NODES, boiler, HOME, INSTALL_ZONES, INSTALL, CIRCUITS)
    assert d.circuit_sources == {1: "0000000000000001"}
    assert flow_circuit(d, 1)["servedC"] == 45
    assert flow_circuit(d, 2) is None
    # No installation (503 read as {}): no source.
    assert parse(HEALTH, ZONES, NODES, boiler, HOME, None, None, {}).circuit_sources == {}
    assert flow_circuit(data(), 1) is None


def test_flow_managed():
    # The direct route is open on a circuit that nothing manages, and on a daemon
    # that sends none of the fields.
    assert not flow_managed(FLOW_CIRCUIT)
    assert not flow_managed({"id": 1, "curve": False})
    for field in ("curve", "defaultCurve", "adapt"):
        assert flow_managed(dict(FLOW_CIRCUIT, **{field: True}))


def test_boiler_energy_state():
    boiler = {**BOILER, "energy": ENERGY}
    d = parse(HEALTH, ZONES, NODES, boiler, HOME)
    assert boiler_energy(d)["totalKWh"] == 1234.567
    assert energy_state(d) == "running"
    for reason, state in (
        ("noMaxPower", "no_max_power"),
        ("noReport", "no_report"),
        ("stale", "stale"),
        ("noBurnerFlag", "no_burner_flag"),
        ("noModulation", "no_modulation"),
        # A reason of a newer daemon is unknown, not an error.
        ("noSuchReason", None),
    ):
        stopped = {**BOILER, "energy": {**ENERGY, "running": False, "reason": reason}}
        assert energy_state(parse(HEALTH, ZONES, NODES, stopped, HOME)) == state
    # A daemon that does not serve the estimate.
    assert boiler_energy(data()) is None
    assert energy_state(data()) is None


def test_boiler_gas_state():
    energy = {**ENERGY, "gas": GAS}
    d = parse(HEALTH, ZONES, NODES, {**BOILER, "energy": energy}, HOME)
    assert boiler_gas(d)["totalM3"] == 321.654
    assert gas_state(d) == "running"
    for reason, state in (
        ("noGasFlow", "no_gas_flow"),
        ("noReport", "no_report"),
        ("stale", "stale"),
        ("noBurnerFlag", "no_burner_flag"),
        ("noModulation", "no_modulation"),
        # A reason of a newer daemon is unknown, not an error.
        ("noSuchReason", None),
    ):
        stopped = {**BOILER, "energy": {**ENERGY, "gas": {**GAS, "running": False, "reason": reason}}}
        assert gas_state(parse(HEALTH, ZONES, NODES, stopped, HOME)) == state
    # A daemon that serves the heat estimate but not the gas one, and one that
    # serves neither.
    old = parse(HEALTH, ZONES, NODES, {**BOILER, "energy": ENERGY}, HOME)
    assert boiler_gas(old) is None
    assert gas_state(old) is None
    assert boiler_gas(data()) is None
    assert gas_state(data()) is None


def test_fuel_unit():
    # A daemon that serves no unit counts natural gas in m3.
    assert fuel_unit(data()) == "m3"
    energy = {**ENERGY, "gas": {**GAS, "unit": "l"}}
    from_view = parse(HEALTH, ZONES, NODES, {**BOILER, "energy": energy}, HOME)
    assert fuel_unit(from_view) == "l"
    # The install settings come first, and an unknown unit is no unit.
    both = parse(HEALTH, ZONES, NODES, {**BOILER, "energy": energy}, HOME, settings={"fuel_unit": "kg"})
    assert fuel_unit(both) == "kg"
    unknown = parse(HEALTH, ZONES, NODES, BOILER, HOME, settings={"fuel_unit": "gallon"})
    assert fuel_unit(unknown) == "m3"


def test_install_settings():
    d = parse(HEALTH, ZONES, NODES, BOILER, HOME, None, INSTALL)
    assert d.settings["boiler_max_power_kw"] is None
    assert d.settings["boiler_min_power_kw"] == 0.0
    # No installation (503 read as {}): no settings.
    assert parse(HEALTH, ZONES, NODES, BOILER, HOME, None, {}).settings == {}
    assert data().settings == {}


@pytest.mark.parametrize(
    ("mode", "auto", "want"),
    [
        ("setpoint", False, CORRECTION_SHIFT),
        ("config", False, CORRECTION_NONE),
        ("dbg2", False, CORRECTION_NONE),
        ("config", True, CORRECTION_CALIBRATION),
        ("dbg2", True, CORRECTION_CALIBRATION),
        (None, False, None),
    ],
)
def test_zone_correction(mode, auto, want):
    install = copy.deepcopy(INSTALL_ZONES)
    install["zones"][1].update(offset_mode=mode, auto_offset=auto)
    d = parse(HEALTH, ZONES, NODES, BOILER, HOME, install)
    assert zone_correction(d, d.zones[2]) == want


def test_correction_patch_leaves_no_state_the_daemon_refuses():
    # The daemon refuses offset_mode setpoint with auto_offset on, against the
    # values a request leaves. Every move ends on the method and passes.
    methods = (CORRECTION_NONE, CORRECTION_SHIFT, CORRECTION_CALIBRATION)
    for mode in ("config", "dbg2", "setpoint"):
        for auto in (False, True):
            if mode == "setpoint" and auto:
                continue
            for method in methods:
                fields = correction_patch(mode, auto, method)
                new_mode = fields.get("offset_mode", mode)
                new_auto = fields.get("auto_offset", auto)
                assert not (new_mode == "setpoint" and new_auto), (mode, auto, method)
                if new_auto:
                    reached = CORRECTION_CALIBRATION
                else:
                    reached = CORRECTION_SHIFT if new_mode == "setpoint" else CORRECTION_NONE
                assert reached == method, (mode, auto, method)
    assert correction_patch("setpoint", False, CORRECTION_SHIFT) == {}
    with pytest.raises(ValueError):
        correction_patch("config", False, "cloud")


def test_device_zones():
    zones = copy.deepcopy(ZONES)
    devices = copy.deepcopy(INSTALL_DEVICES)
    # cccc serves the heating zone 1 and the hot water zone 9, and its row names
    # zone 9: it follows the heating zone. eeee is listed by zone 2 and its row
    # names zone 1, a heating zone: it follows its row.
    zones["declared"][0]["members"] = ["aaaa", "bbbb", "cccc"]
    zones["declared"][1]["members"] = ["dddd", "eeee"]
    devices["devices"].append({"short": "eeee", "zone": 1})
    d = parse(HEALTH, zones, NODES, BOILER, HOME, install_devices=devices)
    assert device_zones(d) == {"aaaa": 1, "bbbb": 1, "cccc": 1, "dddd": 2, "eeee": 1}
    # A device of the hot water zone alone follows it.
    d = parse(HEALTH, ZONES, NODES, BOILER, HOME, install_devices=INSTALL_DEVICES)
    assert device_zones(d)["cccc"] == 9

