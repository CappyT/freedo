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
- The area goes to the daemon as the display field ha_area of the zone row, on a
  daemon that serves it. The stored value is also the previous area of the zone
  after a restart.
"""

from __future__ import annotations

from collections.abc import Callable, Mapping
from datetime import datetime
import logging
from typing import Any

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
from .const import DOMAIN
from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .model import KIND_HEATING, FreedoData, device_zones

_LOGGER = logging.getLogger(__name__)

ISSUE_NO_AREA = "rooms_no_area"
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
    hass.data.get(_SEEN, {}).pop(entry_id, None)


def _created_before_load(device: dr.DeviceEntry) -> bool:
    created = getattr(device, "created_at", None)
    return isinstance(created, datetime) and created < _LOADED_AT


@callback
def async_track_rooms(
    hass: HomeAssistant, coordinator: FreedoCoordinator
) -> Callable[[], None]:
    """Keep the zone devices, their members, the repair issue and the daemon
    display field in step with the zone areas at each poll."""
    devices = dr.async_get(hass)
    areas = ar.async_get(hass)
    entry: FreedoConfigEntry = coordinator.config_entry
    entry_id = entry.entry_id
    seen_by_entry: dict[str, set[str]] = hass.data.setdefault(_SEEN, {})
    # The area of each zone at the previous poll.
    previous: dict[int, str | None] = {}
    # The room names of the open repair issue, None before the first poll.
    issue_rooms: list[str] | None = None
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

    update()
    return coordinator.async_add_listener(update)


def temperature_sensors(hass: HomeAssistant) -> list[tuple[State, str | None]]:
    """The sensor states with the device class temperature, each with its area:
    the area of the entity, else the area of its device. The entities of freedo
    are left out: the devices of a room are options of their own."""
    entities = er.async_get(hass)
    devices = dr.async_get(hass)
    out: list[tuple[State, str | None]] = []
    for state in hass.states.async_all("sensor"):
        if state.attributes.get(ATTR_DEVICE_CLASS) != SensorDeviceClass.TEMPERATURE:
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


def sensor_label(state: State) -> str:
    """The sensor name with its reading, the name alone without a number."""
    try:
        float(state.state)
    except ValueError:
        return state.name
    unit = state.attributes.get(ATTR_UNIT_OF_MEASUREMENT)
    return f"{state.name} ({state.state} {unit})" if unit else f"{state.name} ({state.state})"
