"""Buttons of the devices: identify on the valves and the wall thermostat,
motor calibration on the valves."""

from __future__ import annotations

from homeassistant.components.button import ButtonDeviceClass, ButtonEntity
from homeassistant.const import EntityCategory
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .coordinator import FreedoConfigEntry
from .entity import FreedoNodeEntity, call_api
from .model import KIND_THERMOSTAT, KIND_VALVE, in_zone


async def async_setup_entry(
    hass: HomeAssistant,
    entry: FreedoConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    coordinator = entry.runtime_data.coordinator
    entities: list[ButtonEntity] = []
    for short, dev in sorted(coordinator.data.devices.items()):
        kind = dev.get("kind")
        if kind in (KIND_VALVE, KIND_THERMOSTAT) and in_zone(coordinator.data, short):
            entities.append(IdentifyButton(coordinator, short))
        if kind == KIND_VALVE:
            entities.append(CalibrateButton(coordinator, short))
    async_add_entities(entities)


class IdentifyButton(FreedoNodeEntity, ButtonEntity):
    """POST /api/nodes/{id}/identify: the device signals itself. The wall
    thermostat shows "Hi!" on its display."""

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
