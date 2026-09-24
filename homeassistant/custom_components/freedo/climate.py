"""Climate entity per heating zone."""

from __future__ import annotations

import time
from typing import Any

import voluptuous as vol

from homeassistant.components.climate import (
    PRESET_BOOST,
    PRESET_NONE,
    ClimateEntity,
    ClimateEntityFeature,
    HVACAction,
    HVACMode,
)
from homeassistant.const import ATTR_TEMPERATURE, UnitOfTemperature
from homeassistant.core import HomeAssistant, callback
from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers import config_validation as cv, entity_platform
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .const import SERVICE_BOOST
from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FreedoEntity, call_api
from .model import (
    Zone,
    zone_boost,
    zone_current_humidity,
    zone_current_temperature,
    zone_demand_pct,
    zone_mode,
    zone_operation,
    zone_overlay_end,
    zone_program_temperature,
    zone_target_source,
    zone_target_temperature,
)

# A write reaches a valve at its next CSL wake-up, so the reported values lag.
# The entity shows the requested value until the valve reports it, or at most
# this long.
PENDING_SEC = 15 * 60

_OPERATION_TO_HVAC = {
    "auto": HVACMode.AUTO,
    "heat": HVACMode.HEAT,
    "off": HVACMode.OFF,
}


async def async_setup_entry(
    hass: HomeAssistant,
    entry: FreedoConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    coordinator = entry.runtime_data.coordinator
    async_add_entities(
        FreedoZoneClimate(coordinator, zone) for zone in coordinator.data.heating_zones()
    )
    entity_platform.async_get_current_platform().async_register_entity_service(
        SERVICE_BOOST, {vol.Optional("on", default=True): cv.boolean}, "async_boost"
    )


class FreedoZoneClimate(FreedoEntity, ClimateEntity):
    _attr_name = None
    _attr_temperature_unit = UnitOfTemperature.CELSIUS
    _attr_hvac_modes = [HVACMode.OFF, HVACMode.AUTO, HVACMode.HEAT]
    _attr_preset_modes = [PRESET_NONE, PRESET_BOOST]
    _attr_supported_features = (
        ClimateEntityFeature.TARGET_TEMPERATURE
        | ClimateEntityFeature.PRESET_MODE
        | ClimateEntityFeature.TURN_OFF
        | ClimateEntityFeature.TURN_ON
    )
    _attr_min_temp = 5.0
    _attr_max_temp = 25.0
    _attr_target_temperature_step = 0.1

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, f"zone{zone.number}_climate")
        self._number = zone.number
        self._attr_device_info = self.zone_device(zone)
        self._pending_target: tuple[float, float] | None = None
        self._pending_mode: tuple[HVACMode, float] | None = None

    @property
    def _zone(self) -> Zone | None:
        return self.coordinator.data.zones.get(self._number)

    @property
    def available(self) -> bool:
        return super().available and self._zone is not None

    @callback
    def _handle_coordinator_update(self) -> None:
        now = time.monotonic()
        reported = self._reported_target()
        if self._pending_target and (
            now > self._pending_target[1]
            or (reported is not None and abs(reported - self._pending_target[0]) < 0.05)
        ):
            self._pending_target = None
        if self._pending_mode and (
            now > self._pending_mode[1] or self._reported_mode() == self._pending_mode[0]
        ):
            self._pending_mode = None
        super()._handle_coordinator_update()

    def _reported_target(self) -> float | None:
        zone = self._zone
        return zone_target_temperature(self.coordinator.data, zone) if zone else None

    def _reported_mode(self) -> HVACMode | None:
        zone = self._zone
        operation = zone_operation(self.coordinator.data, zone) if zone else None
        return _OPERATION_TO_HVAC.get(operation) if operation else None

    @property
    def current_temperature(self) -> float | None:
        zone = self._zone
        return zone_current_temperature(self.coordinator.data, zone) if zone else None

    @property
    def current_humidity(self) -> float | None:
        zone = self._zone
        return zone_current_humidity(self.coordinator.data, zone) if zone else None

    @property
    def target_temperature(self) -> float | None:
        if self._pending_target:
            return self._pending_target[0]
        return self._reported_target()

    @property
    def hvac_mode(self) -> HVACMode | None:
        if self._pending_mode:
            return self._pending_mode[0]
        return self._reported_mode()

    @property
    def preset_mode(self) -> str:
        zone = self._zone
        boosting = zone_boost(self.coordinator.data, zone) if zone else False
        return PRESET_BOOST if boosting else PRESET_NONE

    @property
    def hvac_action(self) -> HVACAction | None:
        zone = self._zone
        demand = zone_demand_pct(self.coordinator.data, zone) if zone else None
        if demand is None:
            return None
        return HVACAction.HEATING if demand > 0 else HVACAction.IDLE

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        data = self.coordinator.data
        window = data.windows.get(self._number) or {}
        report = data.demand.get(self._number) or {}
        fallback = data.fallback.get(self._number)
        zone = self._zone
        return {
            "zone": self._number,
            "window_open": bool(window.get("open")),
            "window_source": window.get("source"),
            "window_since": window.get("since"),
            "window_deadline": window.get("deadline"),
            "demand_pct": report.get("DemandPct"),
            "heat_pct": report.get("HeatPct"),
            "fallback": fallback.get("name") if fallback else None,
            "overlay": zone_mode(data, zone) if zone else None,
            "overlay_ends_at": zone_overlay_end(data, zone) if zone else None,
            "target_source": zone_target_source(data, zone) if zone else None,
            "program_temperature": zone_program_temperature(data, zone) if zone else None,
            "members": list(zone.members) if zone else [],
        }

    async def async_set_temperature(self, **kwargs: Any) -> None:
        zone = self._zone
        temp = kwargs.get(ATTR_TEMPERATURE)
        if zone is None or temp is None:
            return
        temp = round(float(temp), 1)
        await call_api(self.coordinator.client.zone_setpoint(zone.number, temp))
        deadline = time.monotonic() + PENDING_SEC
        self._pending_target = (temp, deadline)
        self._pending_mode = (HVACMode.HEAT, deadline)
        self.async_write_ha_state()
        await self.coordinator.async_request_refresh()

    async def async_set_hvac_mode(self, hvac_mode: HVACMode) -> None:
        zone = self._zone
        if zone is None:
            return
        client = self.coordinator.client
        if hvac_mode == HVACMode.AUTO:
            await call_api(client.zone_auto(zone.number))
        elif hvac_mode == HVACMode.OFF:
            await call_api(client.zone_off(zone.number))
        else:
            temp = self.target_temperature or zone_program_temperature(
                self.coordinator.data, zone
            )
            if temp is None:
                raise HomeAssistantError("Set a target temperature first")
            await call_api(client.zone_setpoint(zone.number, temp))
        self._pending_mode = (hvac_mode, time.monotonic() + PENDING_SEC)
        self.async_write_ha_state()
        await self.coordinator.async_request_refresh()

    async def async_boost(self, on: bool = True) -> None:
        zone = self._zone
        if zone is None:
            return
        await call_api(self.coordinator.client.zone_boost(zone.number, on))
        await self.coordinator.async_request_refresh()

    async def async_set_preset_mode(self, preset_mode: str) -> None:
        if preset_mode == PRESET_BOOST:
            await self.async_boost(True)
        elif self.preset_mode == PRESET_BOOST:
            await self.async_boost(False)
