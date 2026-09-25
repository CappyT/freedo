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
from typing import Any

KIND_HEATING = "heating"
KIND_HOT_WATER = "hotwater"
KIND_BRIDGE = "bridge"
KIND_VALVE = "valve"

# The values of GET /api/nodes Display.orientation that a write accepts.
ORIENTATIONS = ("horizontal", "vertical")

# The daemon rejects an outdoor temperature outside -60..60 C with 400.
OUTDOOR_BOUND_C = 60.0

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
    boiler: dict[str, Any] = field(default_factory=dict)
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
) -> FreedoData:
    """Build the model from the read endpoints. install is the body of
    GET /api/install/zones, settings the body of GET /api/install."""
    data = FreedoData(boiler=dict(boiler or {}), away=bool((home or {}).get("away")))
    offline = (settings or {}).get("offline_schedule")
    if isinstance(offline, bool):
        data.offline_schedule = offline
    shifts = {
        int(z["number"]): float(z.get("setpoint_offset_c") or 0)
        for z in (install or {}).get("zones") or []
    }
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


def node_display(data: FreedoData, short: str) -> Mapping[str, Any] | None:
    """GET /api/nodes Display: the display settings the daemon serves a valve,
    None on any other device."""
    display = (data.nodes.get(short) or {}).get("Display")
    return display if isinstance(display, Mapping) else None


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
