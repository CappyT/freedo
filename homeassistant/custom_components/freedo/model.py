"""Data model of the freedo API and the payload mapping of the environment
inputs.

This module imports nothing from Home Assistant, so the unit tests run
without it. The field names follow the daemon JSON: the keys of the view
structs of the tado package are the Go field names ("Short", "TempC", ...),
the keys the daemon builds itself are camelCase ("targetC", "endsAt", ...).
"""

from __future__ import annotations

from collections.abc import Iterable, Mapping
from dataclasses import dataclass, field
from datetime import time
from typing import Any

KIND_HEATING = "heating"
KIND_HOT_WATER = "hotwater"
KIND_BRIDGE = "bridge"
KIND_VALVE = "valve"

# The values of GET /api/nodes Display.orientation that a write accepts.
ORIENTATIONS = ("horizontal", "vertical")

# The daemon rejects a flow limit outside this range with 400 (tag 2040).
FLOW_LIMIT_MIN_C = 30.0
FLOW_LIMIT_MAX_C = 75.0

# The daemon rejects a room setpoint outside this range with 400.
ZONE_MIN_C = 5.0
ZONE_MAX_C = 25.0

DAYS = ("mon", "tue", "wed", "thu", "fri", "sat", "sun")

# The daemon rejects a zone temperature correction beyond +-10 C (tag 0140).
OFFSET_LIMIT_C = 10.0
OFFSET_MODE_SETPOINT = "setpoint"

# The end rules of a change made on a device (GET /api/install/zones
# device_overlay_end), and the timer length the daemon accepts in seconds.
OVERLAY_END_RULES = ("manual", "until_next", "timer")
OVERLAY_END_MAX_SEC = 7 * 24 * 3600
# The timer a zone gets when its rule changes to timer.
OVERLAY_END_DEFAULT_SEC = 3600
# The daemon bounds the open window detection time at 16 bits of seconds.
OPEN_WINDOW_MAX_SEC = 0xFFFF

# The daemon rejects an outdoor temperature outside -60..60 C with 400.
OUTDOOR_BOUND_C = 60.0

# The daemon rejects a boiler power above this with 400 (a larger value is a
# typo in W).
BOILER_POWER_MAX_KW = 500.0

# GET /api/boiler energy.reason, the cause of an estimate that does not run, by
# the state of the energy estimate sensor.
ENERGY_REASONS = {
    "noMaxPower": "no_max_power",
    "noReport": "no_report",
    "stale": "stale",
    "noBurnerFlag": "no_burner_flag",
    "noModulation": "no_modulation",
}
ENERGY_RUNNING = "running"

# The device stores one day program in at most this many bytes of its protobuf
# encoding, and the daemon rejects a longer one with 400.
SCHEDULE_DAY_BYTES_MAX = 255

_KNOWN_BINARY = {"on": True, "off": False}


@dataclass(frozen=True)
class Zone:
    """A zone as the installation declares it (GET /api/zones "declared")."""

    number: int
    name: str
    kind: str
    members: tuple[str, ...]
    measuring: str | None = None
    reporter: str | None = None
    # GET /api/install/zones setpoint_offset_c: the correction of a zone in
    # offset mode setpoint. The daemon reports the zone temperature and target
    # on the corrected scale, corrected = wire + shift_c, and GET /api/nodes on
    # the wire scale.
    shift_c: float = 0.0


@dataclass
class FreedoData:
    """One poll of the daemon."""

    zones: dict[int, Zone] = field(default_factory=dict)
    # GET /api/zones "demand": ZoneView by zone number.
    demand: dict[int, dict[str, Any]] = field(default_factory=dict)
    # GET /api/zones "declared"[].window, heating zones only.
    windows: dict[int, dict[str, Any]] = field(default_factory=dict)
    # GET /api/zones "declared"[].target: what the zone targets now.
    targets: dict[int, dict[str, Any]] = field(default_factory=dict)
    # GET /api/nodes: ValveView by short address.
    nodes: dict[str, dict[str, Any]] = field(default_factory=dict)
    # GET /api/health "devices": by short address.
    devices: dict[str, dict[str, Any]] = field(default_factory=dict)
    # GET /api/health "zoneFallback": active fallbacks by zone number.
    fallback: dict[int, dict[str, Any]] = field(default_factory=dict)
    # GET /api/install/circuits circuits[] heat_source: the wall thermostat that
    # drives each circuit, by circuit id.
    circuit_sources: dict[int, str] = field(default_factory=dict)
    # GET /api/install/zones zones[]: the installation row by zone number.
    zone_settings: dict[int, dict[str, Any]] = field(default_factory=dict)
    boiler: dict[str, Any] = field(default_factory=dict)
    # GET /api/install: the installation options. Empty when the daemon holds
    # no installation.
    settings: dict[str, Any] = field(default_factory=dict)
    away: bool = False
    # GET /api/install offline_schedule: d/config tag 02b3 of every device
    # without a per-device value. None when the daemon holds no installation.
    offline_schedule: bool | None = None

    @property
    def hot_water_zone(self) -> Zone | None:
        for zone in self.zones.values():
            if zone.kind == KIND_HOT_WATER:
                return zone
        return None

    def heating_zones(self) -> list[Zone]:
        return sorted(
            (z for z in self.zones.values() if z.kind == KIND_HEATING),
            key=lambda z: z.number,
        )


def parse(
    health: Mapping[str, Any],
    zones: Mapping[str, Any],
    nodes: Iterable[Mapping[str, Any]],
    boiler: Mapping[str, Any],
    home: Mapping[str, Any],
    install: Mapping[str, Any] | None = None,
    settings: Mapping[str, Any] | None = None,
    circuits: Mapping[str, Any] | None = None,
) -> FreedoData:
    """Build the model from the read endpoints. install is the body of
    GET /api/install/zones, settings the body of GET /api/install, circuits the
    body of GET /api/install/circuits."""
    data = FreedoData(
        boiler=dict(boiler or {}),
        settings=dict(settings or {}),
        away=bool((home or {}).get("away")),
    )
    offline = (settings or {}).get("offline_schedule")
    if isinstance(offline, bool):
        data.offline_schedule = offline
    rows = {int(z["number"]): dict(z) for z in (install or {}).get("zones") or []}
    data.zone_settings = rows
    data.circuit_sources = {
        int(c["id"]): c["heat_source"]
        for c in (circuits or {}).get("circuits") or []
        if c.get("heat_source")
    }
    shifts = {n: float(row.get("setpoint_offset_c") or 0) for n, row in rows.items()}
    for z in zones.get("declared") or []:
        number = int(z["number"])
        data.zones[number] = Zone(
            number=number,
            name=z.get("name") or f"Zone {number}",
            kind=z.get("kind") or "",
            members=tuple(z.get("members") or ()),
            measuring=z.get("measuringDevice"),
            reporter=z.get("reporter"),
            shift_c=shifts.get(number, 0.0),
        )
        if isinstance(z.get("window"), Mapping):
            data.windows[number] = dict(z["window"])
        if isinstance(z.get("target"), Mapping):
            data.targets[number] = dict(z["target"])
    for view in zones.get("demand") or []:
        data.demand[int(view["Number"])] = dict(view)
    for node in nodes or []:
        if node.get("Short"):
            data.nodes[node["Short"]] = dict(node)
    for dev in health.get("devices") or []:
        if dev.get("short"):
            data.devices[dev["short"]] = dict(dev)
    for fb in health.get("zoneFallback") or []:
        data.fallback[int(fb["zone"])] = dict(fb)
    return data


def _seen(node: Mapping[str, Any] | None) -> bool:
    return bool(node) and bool(node.get("LastSeenUnix"))


def _zone_nodes(data: FreedoData, zone: Zone) -> list[dict[str, Any]]:
    """The zone's heard devices: measuring device, reporter, then members."""
    order: list[str] = []
    for short in (zone.measuring, zone.reporter, *zone.members):
        if short and short not in order:
            order.append(short)
    return [data.nodes[s] for s in order if _seen(data.nodes.get(s))]


def _node_temperature(node: Mapping[str, Any]) -> float | None:
    """Tag 012d of a device on the wire scale. GET /api/nodes TempC is 0 until
    the device sends 012d; Telemetry holds temp_C from the first one."""
    if "temp_C" not in (node.get("Telemetry") or {}):
        return None
    return float(node["TempC"])


def node_temperature(data: FreedoData, short: str) -> float | None:
    """The 012d reading of a device on the wire scale, None until it sends one."""
    node = data.nodes.get(short)
    return _node_temperature(node) if node else None


def node_humidity(data: FreedoData, short: str) -> float | None:
    """The 0135 reading of a device, None on a device that never sent one."""
    node = data.nodes.get(short) or {}
    if "humidity_pct" not in (node.get("Telemetry") or {}):
        return None
    return float(node["HumidityPct"])


def node_firmware(data: FreedoData, short: str) -> str | None:
    """The running firmware version, major.minor, from the fw/state the device
    reports (Telemetry fw_run_version)."""
    return ((data.nodes.get(short) or {}).get("Telemetry") or {}).get("fw_run_version")


def zone_current_temperature(data: FreedoData, zone: Zone) -> float | None:
    """Tag 4060 of the zone report, else the first zone device with a 012d,
    both on the corrected scale."""
    report = data.demand.get(zone.number)
    if report and report.get("UpdatedUnix"):
        temp = float(report["TempC"])
        # A 40a0 or 4160 alone also sets UpdatedUnix, and TempC then reads
        # 0 + shift: the demand view has no field that says 4060 is missing.
        if round(temp - zone.shift_c, 2) != 0:
            return temp
    for node in _zone_nodes(data, zone):
        temp = _node_temperature(node)
        if temp is not None:
            return round(temp + zone.shift_c, 2)
    return None


def zone_current_humidity(data: FreedoData, zone: Zone) -> float | None:
    for node in _zone_nodes(data, zone):
        if node.get("HumidityPct"):
            return float(node["HumidityPct"])
    return None


def _target(data: FreedoData, zone: Zone) -> Mapping[str, Any]:
    return data.targets.get(zone.number) or {}


def _overlay(data: FreedoData, zone: Zone) -> Mapping[str, Any] | None:
    overlay = _target(data, zone).get("overlay")
    return overlay if isinstance(overlay, Mapping) else None


def _float(value: Any) -> float | None:
    return float(value) if value is not None else None


def zone_target_temperature(data: FreedoData, zone: Zone) -> float | None:
    """The target the zone runs now. None when the device falls back to a
    value of its own: an open window, away, an off overlay or program block."""
    return _float(_target(data, zone).get("targetC"))


def zone_program_temperature(data: FreedoData, zone: Zone) -> float | None:
    return _float(_target(data, zone).get("programC"))


def zone_mode(data: FreedoData, zone: Zone) -> str | None:
    """"auto" when the zone follows its program, the overlay kind ("manual",
    "until_next" or "boost") on an overlay, None when the daemon holds no
    block of the zone."""
    if not _target(data, zone).get("known"):
        return None
    overlay = _overlay(data, zone)
    return str(overlay.get("kind")) if overlay else "auto"


def zone_boost(data: FreedoData, zone: Zone) -> bool:
    return bool(_target(data, zone).get("boost"))


def zone_overlay_end(data: FreedoData, zone: Zone) -> str | None:
    """The instant the daemon ends the overlay, RFC 3339, None without one."""
    overlay = _overlay(data, zone)
    return overlay.get("endsAt") if overlay else None


def zone_target_source(data: FreedoData, zone: Zone) -> str | None:
    """program, overlay, boost, window or away."""
    return _target(data, zone).get("source")


def zone_operation(data: FreedoData, zone: Zone) -> str | None:
    """"auto" on the program, "heat" or "off" on an overlay, None when the
    daemon holds no block of the zone."""
    mode = zone_mode(data, zone)
    if mode is None or mode == "auto":
        return mode
    return "off" if _target(data, zone).get("on") is False else "heat"


def zone_demand_pct(data: FreedoData, zone: Zone) -> int | None:
    report = data.demand.get(zone.number)
    if not report or not report.get("UpdatedUnix"):
        return None
    return int(report.get("DemandPct") or 0)


def in_zone(data: FreedoData, short: str) -> bool:
    """Whether a zone declares the device as a member. The daemon answers 400
    "id required" to an identify, child lock or display write of a device
    that belongs to no zone."""
    return any(short in zone.members for zone in data.zones.values())


def node_display(data: FreedoData, short: str) -> Mapping[str, Any] | None:
    """GET /api/nodes Display: the display settings the daemon serves a valve,
    None on any other device."""
    display = (data.nodes.get(short) or {}).get("Display")
    return display if isinstance(display, Mapping) else None


def node_child_lock(data: FreedoData, short: str) -> bool | None:
    """GET /api/nodes ChildLock: the state the valve confirmed, None on a
    device that is not a valve."""
    value = (data.nodes.get(short) or {}).get("ChildLock")
    return value if isinstance(value, bool) else None


# Tag 01fa of the d/<serial>/mnt report, which GET /api/nodes carries under
# Telemetry calib_state.
CALIBRATION_STATES = {"0": "none", "1": "started", "2": "done", "3": "failed"}


def node_calibration(data: FreedoData, short: str) -> tuple[str | None, int | None]:
    """The motor calibration state and its error code (Telemetry calib_state
    and calib_error), None until the valve reports them."""
    telemetry = (data.nodes.get(short) or {}).get("Telemetry") or {}
    state = CALIBRATION_STATES.get(telemetry.get("calib_state", ""))
    error = telemetry.get("calib_error", "")
    return state, int(error) if error.isdigit() else None


def zone_setting(data: FreedoData, zone: Zone, key: str) -> Any:
    """A field of the zone row of GET /api/install/zones, None without one."""
    return (data.zone_settings.get(zone.number) or {}).get(key)


def zone_offset_mode(data: FreedoData, zone: Zone) -> str | None:
    """GET /api/install/zones offset_mode: config, dbg2 or setpoint."""
    return (data.zone_settings.get(zone.number) or {}).get("offset_mode")


def flow_circuit(data: FreedoData, circuit: int) -> Mapping[str, Any] | None:
    """GET /api/boiler flowLimit.circuits[] of one circuit."""
    for view in (data.boiler.get("flowLimit") or {}).get("circuits") or []:
        if view.get("id") == circuit:
            return view
    return None


def boiler_energy(data: FreedoData) -> Mapping[str, Any] | None:
    """GET /api/boiler energy: the heat output estimate, None on a daemon that
    does not serve it."""
    energy = data.boiler.get("energy")
    return energy if isinstance(energy, Mapping) else None


def energy_state(data: FreedoData) -> str | None:
    """"running", or the cause the estimate does not run, None when the daemon
    names a cause this version does not know."""
    energy = boiler_energy(data)
    if energy is None:
        return None
    if energy.get("running"):
        return ENERGY_RUNNING
    return ENERGY_REASONS.get(energy.get("reason"))


def boiler_unsupported(data: FreedoData) -> set[str]:
    """The catalogue names the boiler lists as unsupported (tag 0481), whose
    values the daemon reports as null."""
    return set(data.boiler.get("Unsupported") or ())


def outdoor_payload(state: str | None, unit: str | None) -> dict[str, float] | None:
    """Map an outdoor temperature state to the POST /api/outdoor body.

    An unavailable, unknown, non-numeric or out-of-range state gives None:
    the caller does not push it.
    """
    if state is None:
        return None
    try:
        value = float(state)
    except (TypeError, ValueError):
        return None
    if value != value:  # NaN
        return None
    if unit in ("°F", "F"):
        value = (value - 32.0) * 5.0 / 9.0
    elif unit == "K":
        value -= 273.15
    if abs(value) > OUTDOOR_BOUND_C:
        return None
    return {"tempC": round(value, 2)}


def window_zones(mapping: Mapping[str, Any]) -> dict[int, list[str]]:
    """Invert the options mapping entity_id -> zone into zone -> entity ids."""
    out: dict[int, list[str]] = {}
    for entity_id, zone in mapping.items():
        try:
            number = int(zone)
        except (TypeError, ValueError):
            continue
        out.setdefault(number, []).append(entity_id)
    for entities in out.values():
        entities.sort()
    return out


def window_payload(states: Iterable[str | None]) -> dict[str, bool] | None:
    """Map the states of the window sensors of one zone to the body of
    POST /api/zones/{n}/window.

    The zone is open when any known sensor is on. A sensor that is
    unavailable or unknown does not count. With no known sensor the result is
    None: the caller does not push it.
    """
    known = [_KNOWN_BINARY[s] for s in states if s in _KNOWN_BINARY]
    if not known:
        return None
    return {"open": any(known)}


def _entry_body(entry: Any, where: str) -> tuple[int, dict[str, Any]]:
    """One entry of a schedule: the minute of the day and the daemon body."""
    if not isinstance(entry, Mapping):
        raise ValueError(f"{where} must be a mapping with time and temperature")
    unknown = set(entry) - {"time", "temperature", "enabled"}
    if unknown:
        raise ValueError(f"{where} has unknown fields: {', '.join(sorted(unknown))}")
    value = entry.get("time")
    if isinstance(value, str):
        try:
            value = time.fromisoformat(value.strip())
        except ValueError:
            raise ValueError(f"{where}.time {value!r} is not a time of day, use HH:MM") from None
    if not isinstance(value, time):
        raise ValueError(f"{where}.time is required, as HH:MM")
    if value.second or value.microsecond:
        raise ValueError(f"{where}.time must be a whole minute")
    temp = entry.get("temperature")
    if isinstance(temp, bool) or not isinstance(temp, (int, float)):
        raise ValueError(f"{where}.temperature is required, as a number")
    if not ZONE_MIN_C <= temp <= ZONE_MAX_C:
        raise ValueError(f"{where}.temperature must be within {ZONE_MIN_C:g}..{ZONE_MAX_C:g} C")
    if abs(temp * 100 - round(temp * 100)) > 1e-9:
        raise ValueError(f"{where}.temperature must have at most two decimal places")
    enabled = entry.get("enabled", True)
    if not isinstance(enabled, bool):
        raise ValueError(f"{where}.enabled must be true or false")
    minute = value.hour * 60 + value.minute
    return minute, {"minute": minute, "setpoint_c": temp, "enabled": enabled}


def _uvarint_size(value: int) -> int:
    size = 1
    while value >= 0x80:
        value >>= 7
        size += 1
    return size


def _day_program_size(entries: list[dict[str, Any]]) -> int:
    """The bytes of a day program in the daemon encoding: the day mask field
    (tag and a one-byte varint), then per entry its tag, length and body (minute,
    setpoint in centi-degrees, enabled)."""
    size = 2
    for entry in entries:
        body = (
            1 + _uvarint_size(entry["minute"])
            + 1 + _uvarint_size(round(entry["setpoint_c"] * 100))
            + 2
        )
        size += 1 + _uvarint_size(body) + body
    return size


def schedule_payload(programs: Any) -> list[dict[str, Any]]:
    """Check a weekly schedule against the rules the daemon enforces and map it
    to the body of PUT /api/zones/{n}/schedule. Raises ValueError with the
    first problem.

    A schedule is a list of programs, each with the days it runs on and its
    entries: {"days": ["mon", "tue"], "entries": [{"time": "00:00",
    "temperature": 17, "enabled": true}]}. The programs cover each of the seven
    days once, the entries of a program start at 00:00 and increase, and a
    program encodes to at most SCHEDULE_DAY_BYTES_MAX bytes.
    """
    if not isinstance(programs, list) or not programs:
        raise ValueError("programs must be a list with at least one program")
    seen: set[str] = set()
    body: list[dict[str, Any]] = []
    for i, program in enumerate(programs):
        where = f"programs[{i}]"
        if not isinstance(program, Mapping):
            raise ValueError(f"{where} must be a mapping with days and entries")
        unknown = set(program) - {"days", "entries"}
        if unknown:
            raise ValueError(f"{where} has unknown fields: {', '.join(sorted(unknown))}")
        days = program.get("days")
        if not isinstance(days, list) or not days:
            raise ValueError(f"{where}.days must be a list of days")
        names: list[str] = []
        for day in days:
            name = day.strip().lower() if isinstance(day, str) else day
            if name not in DAYS:
                raise ValueError(f"{where}.days: {day!r} is not a day, use {', '.join(DAYS)}")
            if name in seen:
                raise ValueError(f"{where}.days: {name} is in more than one program")
            seen.add(name)
            names.append(name)
        entries = program.get("entries")
        if not isinstance(entries, list) or not entries:
            raise ValueError(f"{where}.entries must be a list with at least one entry")
        out: list[dict[str, Any]] = []
        previous = -1
        for j, entry in enumerate(entries):
            minute, item = _entry_body(entry, f"{where}.entries[{j}]")
            if not out and minute != 0:
                raise ValueError(f"{where}.entries[0].time must be 00:00")
            if minute <= previous:
                raise ValueError(f"{where}.entries[{j}].time must be later than the entry before")
            previous = minute
            out.append(item)
        size = _day_program_size(out)
        if size > SCHEDULE_DAY_BYTES_MAX:
            raise ValueError(
                f"{where} encodes to {size} bytes, the device stores at most "
                f"{SCHEDULE_DAY_BYTES_MAX} per day"
            )
        body.append({"days": names, "entries": out})
    if len(seen) != len(DAYS):
        missing = ", ".join(d for d in DAYS if d not in seen)
        raise ValueError(f"programs must cover all seven days, missing: {missing}")
    return body


def schedule_programs(view: Mapping[str, Any]) -> list[dict[str, Any]]:
    """The programs of GET /api/zones/{n}/schedule in the form of
    schedule_payload."""
    return [
        {
            "days": list(program["days"]),
            "entries": [
                {
                    "time": f"{entry['minute'] // 60:02d}:{entry['minute'] % 60:02d}",
                    "temperature": entry["setpoint_c"],
                    "enabled": entry["enabled"],
                }
                for entry in program["entries"]
            ],
        }
        for program in view.get("programs") or []
    ]
