"""Rooms: each tado zone is a room, and the area of its zone device in the
device registry maps it to a Home Assistant area. The options step Rooms and the
device page both set that area; the integration reads it at each poll.

- The member devices of a zone take its area when they have no area or still
  carry the previous area of the zone. A device placed elsewhere keeps its area.
  device_zones says which zone a device follows.
- The zone device of a heating zone follows the zone name of the daemon, unless
  the user named the device in Home Assistant. The hot water zone device keeps
  its translated name.
- A heating zone without an area raises one repair issue, which the user can
  ignore.
- The area goes to the daemon as the display field ha_area of the zone row, on a
  daemon that serves it. The stored value is also the previous area of the zone
  after a restart.
"""

from __future__ import annotations

from collections.abc import Callable, Mapping
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

from .api import FreedoError
from .const import DOMAIN
from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .model import KIND_HEATING, FreedoData, Zone, device_zones

_LOGGER = logging.getLogger(__name__)

ISSUE_NO_AREA = "zone_no_area"


def zone_identifier(entry_id: str, number: int) -> tuple[str, str]:
    return (DOMAIN, f"{entry_id}_zone{number}")


def node_identifier(entry_id: str, short: str) -> tuple[str, str]:
    return (DOMAIN, f"{entry_id}_{short}")


def _issue_id(entry_id: str, number: int) -> str:
    return f"{ISSUE_NO_AREA}_{entry_id}_{number}"


def zone_area(hass: HomeAssistant, entry_id: str, number: int) -> str | None:
    """The area of the zone device, None without a device or an area."""
    device = dr.async_get(hass).async_get_device_by_identifier(
        zone_identifier(entry_id, number), entry_id
    )
    return device.area_id if device else None


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
    the previous area of the zone."""
    registry = dr.async_get(hass)
    for short, zone in device_zones(data).items():
        if zone != number:
            continue
        device = registry.async_get_device_by_identifier(node_identifier(entry_id, short), entry_id)
        if device is None or device.area_id == area_id:
            continue
        if device.area_id is None or device.area_id == previous:
            registry.async_update_device(device.id, area_id=area_id)


@callback
def async_set_zone_area(
    hass: HomeAssistant, entry_id: str, data: FreedoData, number: int, area_id: str | None
) -> None:
    """Write the area of a zone device and place its members, as the options
    step does on submit."""
    registry = dr.async_get(hass)
    device = registry.async_get_device_by_identifier(zone_identifier(entry_id, number), entry_id)
    if device is None:
        return
    previous = device.area_id
    if previous != area_id:
        registry.async_update_device(device.id, area_id=area_id)
    async_place_members(hass, entry_id, data, number, area_id, previous)


@callback
def async_remove_issues(hass: HomeAssistant, entry_id: str) -> None:
    """Delete the repair issues of an entry that goes away."""
    prefix = f"{ISSUE_NO_AREA}_{entry_id}_"
    for domain, issue_id in list(ir.async_get(hass).issues):
        if domain == DOMAIN and issue_id.startswith(prefix):
            ir.async_delete_issue(hass, DOMAIN, issue_id)


@callback
def async_track_rooms(
    hass: HomeAssistant, coordinator: FreedoCoordinator
) -> Callable[[], None]:
    """Keep the zone devices, their members, the repair issues and the daemon
    display field in step with the zone areas at each poll."""
    devices = dr.async_get(hass)
    areas = ar.async_get(hass)
    entry: FreedoConfigEntry = coordinator.config_entry
    entry_id = entry.entry_id
    # The area of each zone at the previous poll.
    previous: dict[int, str | None] = {}
    # The zone name of each open repair issue.
    issues: dict[int, str] = {}
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
    def sync_issue(zone: Zone, area_id: str | None) -> None:
        if area_id is not None:
            # The issue may come from before a reload, so delete it even when
            # this listener did not raise it.
            issues.pop(zone.number, None)
            ir.async_delete_issue(hass, DOMAIN, _issue_id(entry_id, zone.number))
            return
        if issues.get(zone.number) == zone.name:
            return
        issues[zone.number] = zone.name
        ir.async_create_issue(
            hass,
            DOMAIN,
            _issue_id(entry_id, zone.number),
            is_fixable=False,
            is_persistent=False,
            severity=ir.IssueSeverity.WARNING,
            translation_key=ISSUE_NO_AREA,
            translation_placeholders={"zone": zone.name},
        )

    @callback
    def sync_daemon(zone: Zone, area_id: str | None, row: Mapping[str, Any]) -> None:
        view = area_view(area_id)
        if view == row.get("ha_area") or zone.number in sending:
            return
        sending.add(zone.number)
        entry.async_create_background_task(
            hass, send(zone.number, view), f"freedo area of zone {zone.number}"
        )

    @callback
    def update() -> None:
        data = coordinator.data
        if data is None:
            return
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
            row = data.zone_settings.get(zone.number) or {}
            served = "ha_area" in row
            if zone.number not in previous:
                stored = row.get("ha_area")
                previous[zone.number] = (
                    stored.get("id") if isinstance(stored, Mapping) else None
                ) if served else device.area_id
            async_place_members(
                hass, entry_id, data, zone.number, device.area_id, previous[zone.number]
            )
            previous[zone.number] = device.area_id
            if zone.kind == KIND_HEATING:
                sync_issue(zone, device.area_id)
            if served:
                sync_daemon(zone, device.area_id, row)
        heating = {z.number for z in data.heating_zones()}
        for number in [n for n in issues if n not in heating]:
            del issues[number]
            ir.async_delete_issue(hass, DOMAIN, _issue_id(entry_id, number))

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
