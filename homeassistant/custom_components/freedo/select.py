"""Display orientation of each valve (d/config tag 0149). End rule of a change
made on a device, of each heating zone."""

from __future__ import annotations

from homeassistant.components.select import SelectEntity
from homeassistant.const import EntityCategory
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FreedoDisplayEntity, FreedoZoneSettingEntity
from .model import (
    KIND_VALVE,
    ORIENTATIONS,
    OVERLAY_END_DEFAULT_SEC,
    OVERLAY_END_RULES,
    Zone,
    zone_setting,
)


async def async_setup_entry(
    hass: HomeAssistant,
    entry: FreedoConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    coordinator = entry.runtime_data.coordinator
    entities: list[SelectEntity] = [
        OrientationSelect(coordinator, short)
        for short, dev in sorted(coordinator.data.devices.items())
        if dev.get("kind") == KIND_VALVE
    ]
    entities.extend(
        DeviceOverlayEndSelect(coordinator, zone)
        for zone in coordinator.data.heating_zones()
        if zone_setting(coordinator.data, zone, "device_overlay_end") in OVERLAY_END_RULES
    )
    async_add_entities(entities)


class OrientationSelect(FreedoDisplayEntity, SelectEntity):
    _attr_translation_key = "orientation"
    _attr_entity_category = EntityCategory.CONFIG
    _attr_options = list(ORIENTATIONS)

    def __init__(self, coordinator: FreedoCoordinator, short: str) -> None:
        super().__init__(coordinator, short, "orientation")

    @property
    def current_option(self) -> str | None:
        value = (self._display or {}).get("orientation")
        return value if value in ORIENTATIONS else None

    async def async_select_option(self, option: str) -> None:
        await self._write(orientation=option)


class DeviceOverlayEndSelect(FreedoZoneSettingEntity, SelectEntity):
    """The end rule the daemon gives a change made on a device of the zone: the
    rule does not travel on the air. Rule timer starts at one hour, which the
    Device change timer number adjusts."""

    _attr_translation_key = "device_overlay_end"
    _attr_entity_category = EntityCategory.CONFIG
    _attr_options = list(OVERLAY_END_RULES)

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, zone, "device_overlay_end")

    @property
    def current_option(self) -> str | None:
        value = (self._setting or {}).get("device_overlay_end")
        return value if value in OVERLAY_END_RULES else None

    async def async_select_option(self, option: str) -> None:
        if option != "timer":
            await self._write(device_overlay_end=option)
            return
        seconds = (self._setting or {}).get("device_overlay_end_sec") or OVERLAY_END_DEFAULT_SEC
        await self._write(device_overlay_end=option, device_overlay_end_sec=seconds)
