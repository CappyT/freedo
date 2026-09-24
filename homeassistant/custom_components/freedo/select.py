"""Display orientation of each valve (d/config tag 0149)."""

from __future__ import annotations

from homeassistant.components.select import SelectEntity
from homeassistant.const import EntityCategory
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FreedoDisplayEntity
from .model import KIND_VALVE, ORIENTATIONS


async def async_setup_entry(
    hass: HomeAssistant,
    entry: FreedoConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    coordinator = entry.runtime_data.coordinator
    async_add_entities(
        OrientationSelect(coordinator, short)
        for short, dev in sorted(coordinator.data.devices.items())
        if dev.get("kind") == KIND_VALVE
    )


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
