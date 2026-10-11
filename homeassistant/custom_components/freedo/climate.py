"""Climate entity per heating zone."""

from __future__ import annotations

from collections.abc import Mapping
from datetime import timedelta
import time
from typing import Any

import voluptuous as vol

from homeassistant.components.climate import (
    ATTR_HVAC_MODE,
    PRESET_BOOST,
    PRESET_NONE,
    ClimateEntity,
    ClimateEntityFeature,
    HVACAction,
    HVACMode,
)
from homeassistant.const import ATTR_TEMPERATURE, UnitOfTemperature
from homeassistant.core import HomeAssistant, callback
from homeassistant.exceptions import HomeAssistantError, ServiceValidationError
from homeassistant.helpers import config_validation as cv, entity_platform
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .const import DOMAIN, SERVICE_BOOST, SERVICE_SET_OVERLAY, SERVICE_SET_WINDOW
from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FreedoScheduleEntity, call_api
from .model import (
    OVERLAY_END_MAX_SEC,
    OVERLAY_END_RULES,
    ZONE_MAX_C,
    ZONE_MIN_C,
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

MAX_OVERLAY = timedelta(seconds=OVERLAY_END_MAX_SEC)

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
    platform = entity_platform.async_get_current_platform()
    platform.async_register_entity_service(
        SERVICE_BOOST, {vol.Optional("on", default=True): cv.boolean}, "async_boost"
    )
    platform.async_register_entity_service(
        SERVICE_SET_OVERLAY,
        {
            vol.Optional(ATTR_HVAC_MODE, default=HVACMode.HEAT): vol.In(
                [HVACMode.HEAT, HVACMode.OFF]
            ),
            vol.Optional(ATTR_TEMPERATURE): vol.All(
                vol.Coerce(float), vol.Range(min=ZONE_MIN_C, max=ZONE_MAX_C)
            ),
            vol.Optional("end_rule", default="manual"): vol.In(OVERLAY_END_RULES),
            vol.Optional("duration"): vol.All(
                cv.time_period, vol.Range(min=timedelta(seconds=1), max=MAX_OVERLAY)
            ),
        },
        "async_set_overlay",
    )
    platform.async_register_entity_service(
        SERVICE_SET_WINDOW, {vol.Optional("open", default=True): cv.boolean}, "async_set_window"
    )


class FreedoZoneClimate(FreedoScheduleEntity, ClimateEntity):
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
    _attr_min_temp = ZONE_MIN_C
    _attr_max_temp = ZONE_MAX_C
    _attr_target_temperature_step = 0.1
    _schedule_range = (ZONE_MIN_C, ZONE_MAX_C)

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

    def _valid_target(self, temp: float) -> float:
        """The daemon answers 400 to a setpoint outside min_temp..max_temp on
        the corrected scale, where a reported target can exceed max_temp."""
        return round(min(max(temp, self.min_temp), self.max_temp), 2)

    async def async_set_temperature(self, **kwargs: Any) -> None:
        zone = self._zone
        if zone is None:
            return
        hvac_mode = kwargs.get(ATTR_HVAC_MODE)
        temp = kwargs.get(ATTR_TEMPERATURE)
        # On auto the program sets the target and off has none: the mode wins
        # and the temperature is not applied.
        if hvac_mode in (HVACMode.AUTO, HVACMode.OFF):
            await self.async_set_hvac_mode(hvac_mode)
            return
        if temp is None:
            return
        temp = self._valid_target(round(float(temp), 1))
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
            self._pending_target = None
        elif hvac_mode == HVACMode.OFF:
            await call_api(client.zone_off(zone.number))
            self._pending_target = None
        else:
            temp = self.target_temperature or zone_program_temperature(
                self.coordinator.data, zone
            )
            if temp is None:
                raise HomeAssistantError("Set a target temperature first")
            await call_api(client.zone_setpoint(zone.number, self._valid_target(temp)))
        self._pending_mode = (hvac_mode, time.monotonic() + PENDING_SEC)
        self.async_write_ha_state()
        await self.coordinator.async_request_refresh()

    async def async_set_overlay(
        self,
        hvac_mode: HVACMode = HVACMode.HEAT,
        temperature: float | None = None,
        end_rule: str = "manual",
        duration: timedelta | None = None,
    ) -> None:
        """Heat to a temperature or switch off, until the end rule ends it."""
        zone = self._zone
        if zone is None:
            return
        heat = hvac_mode == HVACMode.HEAT
        if heat != (temperature is not None):
            raise ServiceValidationError(
                translation_domain=DOMAIN,
                translation_key="overlay_temperature_required" if heat else "overlay_temperature_not_allowed",
            )
        if (end_rule == "timer") != (duration is not None):
            raise ServiceValidationError(
                translation_domain=DOMAIN,
                translation_key="overlay_duration_required" if end_rule == "timer" else "overlay_duration_not_allowed",
            )
        seconds = int(duration.total_seconds()) if duration is not None else None
        client = self.coordinator.client
        deadline = time.monotonic() + PENDING_SEC
        if heat:
            temp = self._valid_target(round(temperature, 1))
            await call_api(client.zone_setpoint(zone.number, temp, end_rule, seconds))
            self._pending_target = (temp, deadline)
        else:
            await call_api(client.zone_off(zone.number, end_rule, seconds))
            self._pending_target = None
        self._pending_mode = (hvac_mode, deadline)
        self.async_write_ha_state()
        await self.coordinator.async_request_refresh()

    async def async_set_window(self, open: bool = True) -> None:
        """Open the window state of the zone, or reset it. The daemon ends only
        the hold of this source: a hold from a valve detector runs on."""
        zone = self._zone
        if zone is None:
            return
        view = await call_api(self.coordinator.client.push_window(zone.number, open))
        if isinstance(view, Mapping):
            self.coordinator.data.windows[zone.number] = dict(view)
            self.coordinator.async_update_listeners()
        await self.coordinator.async_request_refresh()

    async def async_turn_on(self) -> None:
        """Return the zone to its program."""
        await self.async_set_hvac_mode(HVACMode.AUTO)

    async def async_boost(self, on: bool = True) -> None:
        zone = self._zone
        if zone is None:
            return
        await call_api(self.coordinator.client.zone_boost(zone.number, on))
        self._pending_target = None
        self._pending_mode = None
        self.async_write_ha_state()
        await self.coordinator.async_request_refresh()

    async def async_set_preset_mode(self, preset_mode: str) -> None:
        if preset_mode == PRESET_BOOST:
            await self.async_boost(True)
        elif self.preset_mode == PRESET_BOOST:
            await self.async_boost(False)
