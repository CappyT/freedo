"""Numbers: the temperature correction of each heating zone."""

from __future__ import annotations

from homeassistant.components.number import NumberEntity, NumberMode
from homeassistant.const import EntityCategory, UnitOfTemperature
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FreedoEntity, call_api
from .model import OFFSET_LIMIT_C, OFFSET_MODE_SETPOINT, Zone, zone_offset_mode


async def async_setup_entry(
    hass: HomeAssistant,
    entry: FreedoConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    coordinator = entry.runtime_data.coordinator
    data = coordinator.data
    entities: list[NumberEntity] = []
    for zone in data.heating_zones():
        # The route writes the measuring device, or the daemon itself in mode
        # setpoint: a zone with neither has no correction to set.
        if zone.measuring or zone_offset_mode(data, zone) == OFFSET_MODE_SETPOINT:
            entities.append(ZoneOffsetNumber(coordinator, zone))
    async_add_entities(entities)


class ZoneOffsetNumber(FreedoEntity, NumberEntity):
    """POST /api/zones/{n}/offset: the correction of the room temperature.

    No device class: HA would convert a temperature difference as an absolute
    temperature. Only mode setpoint reports the value (GET /api/install/zones
    setpoint_offset_c). In mode config and dbg2 the correction goes to the
    measuring device and the daemon reports nothing, so the entity shows the
    last value set from Home Assistant.
    """

    _attr_translation_key = "temperature_offset"
    _attr_entity_category = EntityCategory.CONFIG
    _attr_native_unit_of_measurement = UnitOfTemperature.CELSIUS
    _attr_native_min_value = -OFFSET_LIMIT_C
    _attr_native_max_value = OFFSET_LIMIT_C
    _attr_native_step = 0.1
    _attr_mode = NumberMode.BOX

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, f"zone{zone.number}_offset")
        self._number = zone.number
        self._attr_device_info = self.zone_device(zone)
        self._written: float | None = None

    @property
    def _zone(self) -> Zone | None:
        return self.coordinator.data.zones.get(self._number)

    @property
    def available(self) -> bool:
        return super().available and self._zone is not None

    @property
    def native_value(self) -> float | None:
        zone = self._zone
        if zone and zone_offset_mode(self.coordinator.data, zone) == OFFSET_MODE_SETPOINT:
            return zone.shift_c
        return self._written

    async def async_set_native_value(self, value: float) -> None:
        await call_api(self.coordinator.client.set_zone_offset(self._number, round(value, 2)))
        self._written = value
        self.async_write_ha_state()
        await self.coordinator.async_request_refresh()
