"""Numbers: the temperature correction of each heating zone, its open window
detection time and the timer of a change made on a device, and the flow limit
of each circuit."""

from __future__ import annotations

from homeassistant.components.number import NumberDeviceClass, NumberEntity, NumberMode
from homeassistant.const import EntityCategory, UnitOfTemperature, UnitOfTime
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FreedoEntity, FreedoZoneSettingEntity, call_api
from .model import (
    FLOW_LIMIT_MAX_C,
    FLOW_LIMIT_MIN_C,
    OFFSET_LIMIT_C,
    OFFSET_MODE_SETPOINT,
    OPEN_WINDOW_MAX_SEC,
    OVERLAY_END_MAX_SEC,
    Zone,
    flow_circuit,
    zone_offset_mode,
    zone_setting,
)


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
        if zone_setting(data, zone, "open_window_duration_sec") is not None:
            entities.append(OpenWindowDurationNumber(coordinator, zone))
        if zone_setting(data, zone, "device_overlay_end") is not None:
            entities.append(DeviceOverlayTimerNumber(coordinator, zone))
    for circuit in sorted(data.circuit_sources):
        if flow_circuit(data, circuit) is not None:
            entities.append(FlowLimitNumber(coordinator, circuit))
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


class _MinutesNumber(FreedoZoneSettingEntity, NumberEntity):
    """A duration the daemon holds in seconds, shown in minutes."""

    _attr_device_class = NumberDeviceClass.DURATION
    _attr_entity_category = EntityCategory.CONFIG
    _attr_native_unit_of_measurement = UnitOfTime.MINUTES
    _attr_native_min_value = 1
    _attr_native_step = 1
    _attr_mode = NumberMode.BOX
    field: str

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, zone, self.field)

    @property
    def native_value(self) -> float | None:
        seconds = (self._setting or {}).get(self.field)
        return None if seconds is None else round(seconds / 60, 2)

    async def async_set_native_value(self, value: float) -> None:
        await self._write(**{self.field: round(value * 60)})


class OpenWindowDurationNumber(_MinutesNumber):
    """How long the zone stays open after a detection (tag 62c0)."""

    _attr_translation_key = "open_window_duration"
    _attr_native_max_value = OPEN_WINDOW_MAX_SEC // 60
    field = "open_window_duration_sec"


class DeviceOverlayTimerNumber(_MinutesNumber):
    """The length of the timer rule of a change made on a device. The daemon
    holds a duration with the rule timer alone."""

    _attr_translation_key = "device_overlay_timer"
    _attr_native_max_value = OVERLAY_END_MAX_SEC // 60
    field = "device_overlay_end_sec"

    @property
    def available(self) -> bool:
        return super().available and (self._setting or {}).get("device_overlay_end") == "timer"


class FlowLimitNumber(FreedoEntity, NumberEntity):
    """POST /api/boiler/flow: the flow limit (tag 2040) the wall thermostat
    that drives the circuit is served. The thermostat takes a new value by
    re-opening its OpenTherm session, at its next poll. A circuit that follows
    its weather curve takes no value from here: the number is unavailable."""

    _attr_device_class = NumberDeviceClass.TEMPERATURE
    _attr_native_unit_of_measurement = UnitOfTemperature.CELSIUS
    _attr_entity_category = EntityCategory.CONFIG
    _attr_translation_key = "flow_limit"
    _attr_native_min_value = FLOW_LIMIT_MIN_C
    _attr_native_max_value = FLOW_LIMIT_MAX_C
    _attr_native_step = 1
    _attr_mode = NumberMode.BOX

    def __init__(self, coordinator: FreedoCoordinator, circuit: int) -> None:
        super().__init__(coordinator, f"circuit{circuit}_flow_limit")
        self._circuit = circuit
        self._attr_translation_placeholders = {"circuit": str(circuit)}
        self._attr_device_info = self.hub_device()

    @property
    def _view(self):
        return flow_circuit(self.coordinator.data, self._circuit)

    @property
    def available(self) -> bool:
        view = self._view
        return (
            super().available
            and view is not None
            and not view.get("curve")
            and self._circuit in self.coordinator.data.circuit_sources
        )

    @property
    def native_value(self) -> float | None:
        # servedC is 0 until the circuit is served a value.
        return (self._view or {}).get("servedC") or None

    async def async_set_native_value(self, value: float) -> None:
        source = self.coordinator.data.circuit_sources[self._circuit]
        await call_api(self.coordinator.client.set_flow_limit(self._circuit, value, source))
        await self.coordinator.async_request_refresh()
