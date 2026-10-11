"""Buttons of each valve: identify and motor calibration."""

from __future__ import annotations

from homeassistant.components.button import ButtonDeviceClass, ButtonEntity
from homeassistant.const import EntityCategory
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .coordinator import FreedoConfigEntry
from .entity import FreedoNodeEntity, call_api
from .model import KIND_VALVE


async def async_setup_entry(
    hass: HomeAssistant,
    entry: FreedoConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    coordinator = entry.runtime_data.coordinator
    entities: list[ButtonEntity] = []
    for short, dev in sorted(coordinator.data.devices.items()):
        if dev.get("kind") == KIND_VALVE:
            entities.append(IdentifyButton(coordinator, short))
            entities.append(CalibrateButton(coordinator, short))
    async_add_entities(entities)


class IdentifyButton(FreedoNodeEntity, ButtonEntity):
    """POST /api/nodes/{id}/identify: the valve signals itself."""

    _attr_device_class = ButtonDeviceClass.IDENTIFY
    _attr_entity_category = EntityCategory.DIAGNOSTIC

    def __init__(self, coordinator, short: str) -> None:
        super().__init__(coordinator, short, "identify")

    async def async_press(self) -> None:
        await call_api(self.coordinator.client.identify(self._short))


class CalibrateButton(FreedoNodeEntity, ButtonEntity):
    """POST /api/nodes/{id}/calibrate: the valve runs its motor over the full
    travel. The result is the Calibration sensor."""

    _attr_translation_key = "calibrate"
    _attr_entity_category = EntityCategory.CONFIG

    def __init__(self, coordinator, short: str) -> None:
        super().__init__(coordinator, short, "calibrate")

    async def async_press(self) -> None:
        await call_api(self.coordinator.client.calibrate(self._short))
