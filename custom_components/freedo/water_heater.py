"""Water heater entity for the hot-water zone.

The target, the on/off state and the overlay come from the zone target of
GET /api/zones. The current temperature is the boiler DHW temperature.

The frontend draws the temperature control only while the target is not null.
The target shown is the daemon target, else the program temperature, else the
setpoint the boiler reports. With none of them the entity shows the lowest
temperature of the range, so that the control exists, and says so in the
attribute target_note. That value is never sent to the daemon: heat with no
other value raises an error.
"""

from __future__ import annotations

import time
from typing import Any

from homeassistant.components.water_heater import (
    WaterHeaterEntity,
    WaterHeaterEntityFeature,
)
from homeassistant.const import ATTR_TEMPERATURE, STATE_OFF, UnitOfTemperature
from homeassistant.core import HomeAssistant, callback
from homeassistant.exceptions import ServiceValidationError
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .const import DOMAIN, NOTE_NO_TARGET, NOTE_REPORTED_TARGET
from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FreedoScheduleEntity, call_api
from .model import (
    HOT_WATER_MAX_C,
    HOT_WATER_MIN_C,
    Zone,
    zone_has_program,
    zone_mode,
    zone_operation,
    zone_overlay_end,
    zone_overlay_rule,
    zone_program_temperature,
    zone_reported_setpoint,
    zone_target_temperature,
)

STATE_AUTO = "auto"
STATE_HEAT = "heat"

# The thermostat applies a write at its next wake-up, so the reported values
# lag. The entity shows the requested state until the daemon reports it, or at
# most this long.
PENDING_SEC = 15 * 60


async def async_setup_entry(
    hass: HomeAssistant,
    entry: FreedoConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    coordinator = entry.runtime_data.coordinator
    zone = coordinator.data.hot_water_zone
    if zone is not None:
        async_add_entities([FreedoHotWater(coordinator, zone)])


class FreedoHotWater(FreedoScheduleEntity, WaterHeaterEntity):
    _attr_name = None
    _attr_temperature_unit = UnitOfTemperature.CELSIUS
    _attr_operation_list = [STATE_AUTO, STATE_HEAT, STATE_OFF]
    _attr_supported_features = (
        WaterHeaterEntityFeature.TARGET_TEMPERATURE
        | WaterHeaterEntityFeature.OPERATION_MODE
        | WaterHeaterEntityFeature.ON_OFF
    )
    _attr_min_temp = HOT_WATER_MIN_C
    _attr_max_temp = HOT_WATER_MAX_C
    _attr_target_temperature_step = 1.0
    _schedule_range = (HOT_WATER_MIN_C, HOT_WATER_MAX_C)

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, f"zone{zone.number}_water_heater")
        self._zone = zone
        self._attr_device_info = self.zone_device(zone)
        self._pending: tuple[str, float | None, float] | None = None

    def _reported_operation(self) -> str | None:
        return zone_operation(self.coordinator.data, self._zone)

    @callback
    def _handle_coordinator_update(self) -> None:
        if self._pending and (
            time.monotonic() > self._pending[2]
            or (
                self._reported_operation() == self._pending[0]
                and self._pending[1] in (None, self._reported_target())
            )
        ):
            self._pending = None
        super()._handle_coordinator_update()

    def _reported_target(self) -> float | None:
        return zone_target_temperature(self.coordinator.data, self._zone)

    @property
    def current_temperature(self) -> float | None:
        boiler = self.coordinator.data.boiler
        if not boiler.get("Present") or not boiler.get("DHWC"):
            return None
        return float(boiler["DHWC"])

    @property
    def current_operation(self) -> str | None:
        if self._pending:
            return self._pending[0]
        return self._reported_operation()

    def _known_target(self) -> tuple[float | None, str | None]:
        """The temperature the entity can write as a setpoint, None when no
        value exists, and the note that says why a value is no daemon target."""
        if self._pending and self._pending[1] is not None:
            return self._pending[1], None
        data = self.coordinator.data
        for value in (self._reported_target(), zone_program_temperature(data, self._zone)):
            if value is not None:
                return value, None
        reported = zone_reported_setpoint(data, self._zone)
        if reported is not None:
            return reported, NOTE_REPORTED_TARGET
        return None, NOTE_NO_TARGET

    @property
    def target_temperature(self) -> float | None:
        target, _ = self._known_target()
        return self.min_temp if target is None else target

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        data = self.coordinator.data
        return {
            "zone": self._zone.number,
            "overlay": zone_mode(data, self._zone),
            "overlay_rule": zone_overlay_rule(data, self._zone),
            "overlay_ends_at": zone_overlay_end(data, self._zone),
            "has_program": zone_has_program(data, self._zone),
            "program_temperature": zone_program_temperature(data, self._zone),
            "reported_setpoint": zone_reported_setpoint(data, self._zone),
            "target_note": self._known_target()[1],
        }

    async def _write(self, coro, operation: str, temp: float | None) -> None:
        await call_api(coro)
        self._pending = (operation, temp, time.monotonic() + PENDING_SEC)
        self.async_write_ha_state()
        await self.coordinator.async_request_refresh()

    async def async_set_temperature(self, **kwargs: Any) -> None:
        temp = kwargs.get(ATTR_TEMPERATURE)
        if temp is None:
            return
        temp = float(round(float(temp)))
        client = self.coordinator.client
        await self._write(client.zone_setpoint(self._zone.number, temp), STATE_HEAT, temp)

    async def async_set_operation_mode(self, operation_mode: str) -> None:
        client = self.coordinator.client
        number = self._zone.number
        if operation_mode == STATE_OFF:
            await self._write(client.zone_off(number), STATE_OFF, None)
        elif operation_mode == STATE_AUTO:
            await self._write(client.zone_auto(number), STATE_AUTO, None)
        else:
            temp, _ = self._known_target()
            if temp is None:
                raise ServiceValidationError(
                    translation_domain=DOMAIN, translation_key="hot_water_temperature_required"
                )
            await self._write(client.zone_setpoint(number, temp), STATE_HEAT, temp)

    async def async_turn_on(self, **kwargs: Any) -> None:
        await self.async_set_operation_mode(STATE_HEAT)

    async def async_turn_off(self, **kwargs: Any) -> None:
        await self.async_set_operation_mode(STATE_OFF)
