"""Unit tests of the API model and the environment payload mapping.

The fixtures are synthetic and follow the JSON the daemon writes.
"""

import pytest

from model import (
    boiler_unsupported,
    flow_circuit,
    node_calibration,
    node_child_lock,
    node_display,
    node_firmware,
    node_humidity,
    node_temperature,
    outdoor_payload,
    parse,
    schedule_payload,
    schedule_programs,
    window_payload,
    window_zones,
    zone_boost,
    zone_current_temperature,
    zone_demand_pct,
    zone_mode,
    zone_offset_mode,
    zone_operation,
    zone_overlay_end,
    zone_program_temperature,
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
                    "boost": True, "known": True,
                    "overlay": {"kind": "boost", "on": True, "setpointC": 25.0,
                                "endsAt": "2026-01-01T00:30:00Z"}}},
        {"number": 2, "name": "Bedroom", "kind": "heating", "members": ["dddd"],
         "window": {"open": False, "source": None, "since": None, "deadline": None,
                    "undelivered": []},
         "target": {"targetC": 17.0, "source": "program", "programC": 17.0,
                    "programOff": False, "boost": False, "known": True, "overlay": None}},
        {"number": 9, "name": "Hot water", "kind": "hotwater", "members": ["cccc"],
         "target": {"targetC": None, "source": "overlay", "programC": 55.0,
                    "programOff": False, "boost": False, "known": True, "on": False,
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
         "open_window_detection": True, "open_window_duration_sec": 900,
         "early_start": False, "device_overlay_end": "until_next",
         "device_overlay_end_sec": None},
        {"number": 2, "name": "Bedroom", "kind": "heating", "members": ["VA0000000002"],
         "circuit": 1, "reporter": None, "measuring_device": None,
         "offset_mode": "config", "setpoint_offset_c": None,
         "open_window_detection": False, "open_window_duration_sec": 600,
         "early_start": True, "device_overlay_end": "timer",
         "device_overlay_end_sec": 1800},
        {"number": 9, "name": "Hot water", "kind": "hotwater", "members": ["RU0000000001"],
         "circuit": None, "reporter": None, "measuring_device": None,
         "offset_mode": None, "setpoint_offset_c": None,
         "open_window_detection": True, "open_window_duration_sec": 900,
         "early_start": False, "device_overlay_end": None,
         "device_overlay_end_sec": None},
    ],
}

# GET /api/install/circuits and the flow limit view of GET /api/boiler for circuit 1.
CIRCUITS = {"rev": 7, "circuits": [{"id": 1, "name": "Heating", "heat_source": "0000000000000001"}]}
FLOW_CIRCUIT = {"id": 1, "curve": False, "computedC": None, "servedC": 45, "changedAt": None,
                "fetched": True}

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

# GET /api/install: the installation options the integration reads.
INSTALL = {"rev": 7, "home_id": 1234567, "offline_schedule": True,
           "utc_offset_min": None, "timezone": ""}


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


def test_offline_schedule():
    for value in (True, False):
        settings = dict(INSTALL, offline_schedule=value)
        d = parse(HEALTH, ZONES, NODES, BOILER, HOME, INSTALL_ZONES, settings)
        assert d.offline_schedule is value
    # No installation (503 read as {}) or a field of another type: unknown.
    assert parse(HEALTH, ZONES, NODES, BOILER, HOME, INSTALL_ZONES, {}).offline_schedule is None
    assert parse(HEALTH, ZONES, NODES, BOILER, HOME, None, {"offline_schedule": 1}).offline_schedule is None


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
