"""Rooms: each tado zone is a room, and the area of its zone device in the
device registry maps it to a Home Assistant area. The options step Rooms and the
device page both set that area; the integration reads it at each poll.

- The member devices of a zone take its area when the area changes, if they
  have no area or still carry the previous area of the zone. A device placed
  elsewhere keeps its area. A device that appears later (a new pairing) with no
  area takes the area of its zone once. At a steady state nothing moves, so a
  device the user clears to no area stays so. device_zones says which zone a
  device follows.
- The zone device of a heating zone follows the zone name of the daemon, unless
  the user named the device in Home Assistant. The hot water zone device keeps
  its translated name.
- The heating zones without an area raise one repair issue per entry, which
  lists them and which the user can ignore.
- The heating zones that map a room sensor, with its pushes running, and show
  a refused push raise a second repair issue per entry: a device owns the
  zone, so the daemon refuses the readings and the sensor changes nothing. The
  issue lists the rooms and follows each poll and each change of the sensor
  mapping.
- The area goes to the daemon as the display field ha_area of the zone row, on a
  daemon that serves it. The stored value is also the previous area of the zone
  after a restart.
- The temperature source of a heating zone is a device of the room (owner
  device) or a Home Assistant sensor (owner coordinator). The options step of
  a room and the select Temperature source both read it with current_source,
  list it with source_choices and write it with async_set_source.
"""

from __future__ import annotations

from collections.abc import Callable, Mapping
from datetime import datetime
import logging
from typing import Any

from homeassistant.components.binary_sensor import BinarySensorDeviceClass
from homeassistant.components.sensor import SensorDeviceClass
from homeassistant.const import ATTR_DEVICE_CLASS, ATTR_UNIT_OF_MEASUREMENT
from homeassistant.core import HomeAssistant, State, callback
from homeassistant.helpers import (
    area_registry as ar,
    device_registry as dr,
    entity_registry as er,
    issue_registry as ir,
)
from homeassistant.util import dt as dt_util

from .api import FreedoError
from .const import CONF_ROOM_TEMP_PAUSED, CONF_ROOM_TEMPS, DOMAIN
from .coordinator import FreedoConfigEntry, FreedoCoordinator, FreedoRuntime
from .model import (
    KIND_HEATING,
    OWNER_COORDINATOR,
    OWNER_DEVICE,
    FreedoData,
    Zone,
    device_option_label,
    device_zones,
    room_temp_paused,
    room_temp_zones,
    zone_measuring_candidates,
    zone_owned,
)

_LOGGER = logging.getLogger(__name__)

ISSUE_NO_AREA = "rooms_no_area"
ISSUE_SENSOR_IGNORED = "room_sensor_ignored"
# The kinds of a temperature source.
SOURCE_DEVICE = "device"
SOURCE_SENSOR = "sensor"
# The binary sensors that tell that a room is open.
OPENING_CLASSES = (
    BinarySensorDeviceClass.WINDOW,
    BinarySensorDeviceClass.DOOR,
    BinarySensorDeviceClass.OPENING,
    BinarySensorDeviceClass.GARAGE_DOOR,
)
# hass.data key: the node device ids each entry saw at its last poll. It
# survives a reload, which creates the devices of a new pairing.
_SEEN = f"{DOMAIN}_seen_devices"
# A device registered before the integration loaded in this run is not new.
_LOADED_AT = dt_util.utcnow()


def zone_identifier(entry_id: str, number: int) -> tuple[str, str]:
    return (DOMAIN, f"{entry_id}_zone{number}")


def node_identifier(entry_id: str, short: str) -> tuple[str, str]:
    return (DOMAIN, f"{entry_id}_{short}")


def issue_id(entry_id: str) -> str:
    return f"{ISSUE_NO_AREA}_{entry_id}"


def sensor_issue_id(entry_id: str) -> str:
    return f"{ISSUE_SENSOR_IGNORED}_{entry_id}"


def ignored_sensor_rooms(entry: FreedoConfigEntry, data: FreedoData) -> list[str]:
    """The names of the heating rooms that map a sensor, with its pushes not
    paused, and that a device owns: the daemon refuses the readings and shows
    the last refused push as externalTemp.refused."""
    mapped = room_temp_zones(entry.options.get(CONF_ROOM_TEMPS) or {})
    paused = room_temp_paused(entry.options.get(CONF_ROOM_TEMP_PAUSED) or [])
    return [
        zone.name
        for zone in sorted(data.zones.values(), key=lambda z: z.number)
        if zone.kind == KIND_HEATING
        and zone.number in mapped
        and zone.number not in paused
        and not zone_owned(data, zone)
        and (data.room_temps.get(zone.number) or {}).get("refused")
    ]


def zone_area(hass: HomeAssistant, entry_id: str, number: int) -> str | None:
    """The area of the zone device, None without a device or an area."""
    device = dr.async_get(hass).async_get_device_by_identifier(
        zone_identifier(entry_id, number), entry_id
    )
    return device.area_id if device else None


def _members(
    hass: HomeAssistant, entry_id: str, data: FreedoData, number: int
) -> list[dr.DeviceEntry]:
    """The node devices that follow zone number."""
    registry = dr.async_get(hass)
    out = []
    for short, zone in device_zones(data).items():
        if zone != number:
            continue
        device = registry.async_get_device_by_identifier(node_identifier(entry_id, short), entry_id)
        if device is not None:
            out.append(device)
    return out


@callback
def async_place_members(
    hass: HomeAssistant,
    entry_id: str,
    data: FreedoData,
    number: int,
    area_id: str | None,
    previous: str | None,
) -> None:
    """Give area_id to the devices that follow zone number and have no area or
    the previous area of the zone. Run it when the zone area changes."""
    registry = dr.async_get(hass)
    for device in _members(hass, entry_id, data, number):
        if device.area_id != area_id and device.area_id in (None, previous):
            registry.async_update_device(device.id, area_id=area_id)


@callback
def async_set_zone_area(
    hass: HomeAssistant, entry_id: str, data: FreedoData, number: int, area_id: str | None
) -> None:
    """Write the area of a zone device and place its members, as the options
    step does on submit."""
    registry = dr.async_get(hass)
    device = registry.async_get_device_by_identifier(zone_identifier(entry_id, number), entry_id)
    if device is None or device.area_id == area_id:
        return
    previous = device.area_id
    registry.async_update_device(device.id, area_id=area_id)
    async_place_members(hass, entry_id, data, number, area_id, previous)


@callback
def async_remove_issues(hass: HomeAssistant, entry_id: str) -> None:
    """Delete the repair issue and the device memory of an entry that goes
    away."""
    ir.async_delete_issue(hass, DOMAIN, issue_id(entry_id))
    ir.async_delete_issue(hass, DOMAIN, sensor_issue_id(entry_id))
    hass.data.get(_SEEN, {}).pop(entry_id, None)


def _created_before_load(device: dr.DeviceEntry) -> bool:
    created = getattr(device, "created_at", None)
    return isinstance(created, datetime) and created < _LOADED_AT


@callback
def async_track_rooms(
    hass: HomeAssistant, coordinator: FreedoCoordinator
) -> Callable[[], None]:
    """Keep the zone devices, their members, the repair issues and the daemon
    display field in step with the zone areas at each poll. async_set_source
    runs the listeners after it changes the mapping, so the issue about the
    ignored room sensors follows it at once."""
    devices = dr.async_get(hass)
    areas = ar.async_get(hass)
    entry: FreedoConfigEntry = coordinator.config_entry
    entry_id = entry.entry_id
    seen_by_entry: dict[str, set[str]] = hass.data.setdefault(_SEEN, {})
    # The area of each zone at the previous poll.
    previous: dict[int, str | None] = {}
    # The room names of the open repair issue, None before the first poll.
    issue_rooms: list[str] | None = None
    # The same for the issue about the ignored room sensors.
    ignored_rooms: list[str] | None = None
    # Zones whose ha_area write runs, and zones whose last write failed.
    sending: set[int] = set()
    failed: set[int] = set()

    def area_view(area_id: str | None) -> dict[str, str] | None:
        if area_id is None:
            return None
        area = areas.async_get_area(area_id)
        return {"id": area_id, "name": area.name if area else area_id}

    async def send(number: int, view: dict[str, str] | None) -> None:
        try:
            result = await coordinator.client.patch_install_zone(number, ha_area=view)
        except FreedoError as err:
            if number not in failed:
                _LOGGER.warning("Area of zone %s not sent to the daemon: %s", number, err)
            failed.add(number)
            return
        finally:
            sending.discard(number)
        failed.discard(number)
        row = result.get("zone") if isinstance(result, Mapping) else None
        if isinstance(row, Mapping) and coordinator.data is not None:
            coordinator.data.zone_settings[number] = dict(row)

    @callback
    def sync_issue(rooms: list[str]) -> None:
        nonlocal issue_rooms
        if rooms == issue_rooms:
            return
        issue_rooms = rooms
        if not rooms:
            ir.async_delete_issue(hass, DOMAIN, issue_id(entry_id))
            return
        ir.async_create_issue(
            hass,
            DOMAIN,
            issue_id(entry_id),
            is_fixable=False,
            is_persistent=False,
            severity=ir.IssueSeverity.WARNING,
            translation_key=ISSUE_NO_AREA,
            translation_placeholders={"rooms": ", ".join(rooms)},
        )

    @callback
    def sync_ignored() -> None:
        nonlocal ignored_rooms
        if coordinator.data is None:
            return
        rooms = ignored_sensor_rooms(entry, coordinator.data)
        if rooms == ignored_rooms:
            return
        ignored_rooms = rooms
        if not rooms:
            ir.async_delete_issue(hass, DOMAIN, sensor_issue_id(entry_id))
            return
        ir.async_create_issue(
            hass,
            DOMAIN,
            sensor_issue_id(entry_id),
            is_fixable=False,
            is_persistent=False,
            severity=ir.IssueSeverity.WARNING,
            translation_key=ISSUE_SENSOR_IGNORED,
            translation_placeholders={"rooms": ", ".join(rooms)},
        )

    @callback
    def sync_daemon(number: int, area_id: str | None, row: Mapping[str, Any]) -> None:
        view = area_view(area_id)
        if view == row.get("ha_area") or number in sending:
            return
        sending.add(number)
        entry.async_create_background_task(
            hass, send(number, view), f"freedo area of zone {number}"
        )

    @callback
    def update() -> None:
        data = coordinator.data
        if data is None:
            return
        seen = seen_by_entry.get(entry_id)
        present: set[str] = set()
        unmapped: list[str] = []
        for zone in sorted(data.zones.values(), key=lambda z: z.number):
            device = devices.async_get_device_by_identifier(
                zone_identifier(entry_id, zone.number), entry_id
            )
            if device is None:
                continue
            if (
                zone.kind == KIND_HEATING
                and device.name_by_user is None
                and device.name != zone.name
            ):
                devices.async_update_device(device.id, name=zone.name)
            area_id = device.area_id
            row = data.zone_settings.get(zone.number) or {}
            served = "ha_area" in row
            if zone.number not in previous:
                stored = row.get("ha_area")
                previous[zone.number] = (
                    stored.get("id") if isinstance(stored, Mapping) else None
                ) if served else area_id
            members = _members(hass, entry_id, data, zone.number)
            present.update(m.id for m in members)
            if area_id != previous[zone.number]:
                async_place_members(hass, entry_id, data, zone.number, area_id, previous[zone.number])
            elif area_id is not None:
                for member in members:
                    new = member.id not in seen if seen is not None else not _created_before_load(member)
                    if new and member.area_id is None:
                        devices.async_update_device(member.id, area_id=area_id)
            previous[zone.number] = area_id
            if zone.kind == KIND_HEATING and area_id is None:
                unmapped.append(zone.name)
            if served:
                sync_daemon(zone.number, area_id, row)
        seen_by_entry[entry_id] = present
        sync_issue(unmapped)
        sync_ignored()

    update()
    return coordinator.async_add_listener(update)


def _class_states(
    hass: HomeAssistant, domain: str, classes: tuple[str, ...]
) -> list[tuple[State, str | None]]:
    """The states of domain with a device class of classes, each with its area:
    the area of the entity, else the area of its device. The entities of freedo
    are left out."""
    entities = er.async_get(hass)
    devices = dr.async_get(hass)
    out: list[tuple[State, str | None]] = []
    for state in hass.states.async_all(domain):
        if state.attributes.get(ATTR_DEVICE_CLASS) not in classes:
            continue
        entry = entities.async_get(state.entity_id)
        area_id = None
        if entry is not None:
            if entry.platform == DOMAIN:
                continue
            area_id = entry.area_id
            if area_id is None and entry.device_id:
                device = devices.async_get(entry.device_id)
                area_id = device.area_id if device else None
        out.append((state, area_id))
    out.sort(key=lambda item: (item[0].name.casefold(), item[0].entity_id))
    return out


def temperature_sensors(hass: HomeAssistant) -> list[tuple[State, str | None]]:
    """The temperature sensors of Home Assistant. The entities of freedo are
    left out: the devices of a room are sources of their own."""
    return _class_states(hass, "sensor", (SensorDeviceClass.TEMPERATURE,))


def opening_sensors(hass: HomeAssistant) -> list[tuple[State, str | None]]:
    """The window and door sensors of Home Assistant."""
    return _class_states(hass, "binary_sensor", OPENING_CLASSES)


def short_names(hass: HomeAssistant, states: list[State]) -> dict[str, str]:
    """The name to show for each state, by entity id. The name of a state
    repeats its device and adds the name of the entity, such as "Kitchen
    thermometer Temperature". When a device has one state in the list, the
    device name tells the sensor, and it is shorter. A name the user gave to
    the entity stays."""
    entities = er.async_get(hass)
    devices = dr.async_get(hass)
    names = {state.entity_id: state.name for state in states}
    by_device: dict[str, list[str]] = {}
    for state in states:
        entry = entities.async_get(state.entity_id)
        if entry is not None and entry.device_id and not entry.name:
            by_device.setdefault(entry.device_id, []).append(state.entity_id)
    for device_id, entity_ids in by_device.items():
        device = devices.async_get(device_id)
        name = (device.name_by_user or device.name) if device else None
        if name and len(entity_ids) == 1:
            names[entity_ids[0]] = name
    return names


def sensor_label(state: State, name: str | None = None) -> str:
    """The sensor name with its reading, the name alone without a number."""
    name = name or state.name
    try:
        float(state.state)
    except ValueError:
        return name
    unit = state.attributes.get(ATTR_UNIT_OF_MEASUREMENT)
    return f"{name} ({state.state} {unit})" if unit else f"{name} ({state.state})"


def source_choices(
    hass: HomeAssistant, entry: FreedoConfigEntry, data: FreedoData, zone: Zone, area_id: str | None
) -> list[tuple[str, str, str]]:
    """The temperature sources of a heating zone as (kind, target, label), in
    this order: the devices of the room, the temperature sensors in area_id,
    then the other temperature sensors with their area. The device the zone
    measures on and the mapped sensor are always in the list, so the current
    source is one of the choices."""
    rows = zone_measuring_candidates(data, zone)
    if zone.measuring and all(row.get("short") != zone.measuring for row in rows):
        rows.append(data.install_devices.get(zone.measuring) or {"short": zone.measuring})
    choices = [(SOURCE_DEVICE, str(row.get("short")), device_option_label(row)) for row in rows]
    areas = ar.async_get(hass)
    mapped = room_temp_zones(entry.options.get(CONF_ROOM_TEMPS) or {}).get(zone.number)
    near: list[tuple[str, str, str]] = []
    far: list[tuple[str, str, str]] = []
    sensors = temperature_sensors(hass)
    names = short_names(hass, [state for state, _ in sensors])
    for state, sensor_area in sensors:
        label = sensor_label(state, names[state.entity_id])
        if state.entity_id == mapped:
            mapped = None
        if area_id is not None and sensor_area == area_id:
            near.append((SOURCE_SENSOR, state.entity_id, label))
            continue
        area = areas.async_get_area(sensor_area) if sensor_area else None
        far.append((SOURCE_SENSOR, state.entity_id, f"{label}, {area.name}" if area else label))
    if mapped:
        # A mapped sensor the list does not know, such as one without a device
        # class, stays a choice.
        state = hass.states.get(mapped)
        near.insert(0, (SOURCE_SENSOR, mapped, sensor_label(state) if state else mapped))
    return choices + near + far


def current_source(
    entry: FreedoConfigEntry, data: FreedoData, zone: Zone
) -> tuple[str, str] | None:
    """The source the zone regulates on, as (kind, target): the mapped sensor
    while its pushes are not paused and the coordinator owns the zone, else the
    device the zone measures on."""
    mapped = room_temp_zones(entry.options.get(CONF_ROOM_TEMPS) or {}).get(zone.number)
    paused = room_temp_paused(entry.options.get(CONF_ROOM_TEMP_PAUSED) or [])
    if mapped and zone.number not in paused and zone_owned(data, zone):
        return (SOURCE_SENSOR, mapped)
    return (SOURCE_DEVICE, zone.measuring) if zone.measuring else None


async def _write_owner(runtime: FreedoRuntime, number: int, owner: str) -> None:
    """PATCH the owner of zone number and keep the row the daemon answers."""
    result = await runtime.client.patch_install_zone(number, owner=owner)
    row = result.get("zone") if isinstance(result, Mapping) else None
    if isinstance(row, Mapping):
        runtime.coordinator.data.zone_settings[number] = dict(row)


async def async_set_source(
    hass: HomeAssistant, entry: FreedoConfigEntry, number: int, kind: str, target: str
) -> None:
    """Make a device of the room or a Home Assistant sensor the temperature
    source of zone number. It raises FreedoError when the daemon refuses.

    A device becomes the measuring device of the zone (PUT
    /api/install/zones/{n}/measuring), and a zone the coordinator owns goes back
    to its devices (owner device), because the daemon refuses a pushed reading
    there (409). When a sensor is mapped, its pushes pause and the zone reading
    is cleared.

    A sensor is mapped to the zone in the options, its pushes resume and its
    reading goes out at once. The coordinator becomes the owner of the zone
    (owner coordinator). The measuring device stays: the daemon regulates on it
    while the reading is missing or stale."""
    runtime = entry.runtime_data
    client = runtime.client
    data = runtime.coordinator.data
    zone = data.zones.get(number)
    if zone is None:
        return

    def save(paused: bool, sensor: str | None = None) -> None:
        zones = room_temp_paused(entry.options.get(CONF_ROOM_TEMP_PAUSED) or [])
        if paused:
            zones.add(number)
        else:
            zones.discard(number)
        options = {**entry.options, CONF_ROOM_TEMP_PAUSED: sorted(zones)}
        if sensor is not None:
            options[CONF_ROOM_TEMPS] = {
                **(entry.options.get(CONF_ROOM_TEMPS) or {}),
                str(number): sensor,
            }
        hass.config_entries.async_update_entry(entry, options=options)
        # The repair issue about the ignored room sensors reads the options.
        runtime.coordinator.async_update_listeners()

    if kind == SOURCE_DEVICE:
        await client.set_zone_measuring(number, target)
        if zone_owned(data, zone):
            await _write_owner(runtime, number, OWNER_DEVICE)
        if room_temp_zones(entry.options.get(CONF_ROOM_TEMPS) or {}).get(number):
            # The lock keeps a push that started earlier from landing after
            # the clear.
            async with runtime.room_temp_lock:
                save(paused=True)
                await client.clear_room_temp(number)
        return
    if not zone_owned(data, zone):
        await _write_owner(runtime, number, OWNER_COORDINATOR)
    save(paused=False, sensor=target)
    if runtime.room_temps is not None:
        runtime.room_temps.async_remap()
        await runtime.room_temps.push(number)
