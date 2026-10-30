"""Numbers: the manual temperature correction of each heating zone, its open window
detection time, its away temperature and the timer of a change made on a
device, the flow limit of each circuit, and the power and the gas flow of the
boiler."""

from __future__ import annotations

from collections.abc import Mapping

from homeassistant.components.number import (
    NumberDeviceClass,
    NumberEntity,
    NumberMode,
    RestoreNumber,
)
from homeassistant.const import (
    EntityCategory,
    UnitOfPower,
    UnitOfTemperature,
    UnitOfTime,
)
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FUEL_FLOW_UNITS, FreedoEntity, FreedoZoneSettingEntity, call_api
from .model import (
    BOILER_GAS_MAX_M3H,
    FUEL_KG,
    BOILER_POWER_MAX_KW,
    FLOW_LIMIT_MAX_C,
    FLOW_LIMIT_MIN_C,
    OFFSET_LIMIT_C,
    OPEN_WINDOW_MAX_SEC,
    OVERLAY_END_MAX_SEC,
    ZONE_MAX_C,
    ZONE_MIN_C,
    Zone,
    flow_circuit,
    flow_managed,
    fuel_unit,
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
        # The route writes the measuring device: a zone without one has no
        # correction to set.
        if zone.measuring:
            entities.append(ZoneOffsetNumber(coordinator, zone))
        if zone_setting(data, zone, "open_window_duration_sec") is not None:
            entities.append(OpenWindowDurationNumber(coordinator, zone))
        if zone_setting(data, zone, "device_overlay_end") is not None:
            entities.append(DeviceOverlayTimerNumber(coordinator, zone))
        # The row carries the field, null when unset, from a daemon that has the
        # away temperature.
        if "away_temperature_c" in (data.zone_settings.get(zone.number) or {}):
            entities.append(AwayTemperatureNumber(coordinator, zone))
    for circuit in sorted(data.circuit_sources):
        if flow_circuit(data, circuit) is not None:
            entities.append(FlowLimitNumber(coordinator, circuit))
    # GET /api/install serves the boiler power and gas flow while the daemon
    # holds an installation.
    entities.extend(
        number(coordinator)
        for number in (
            BoilerMaxPowerNumber,
            BoilerMinPowerNumber,
            BoilerMaxGasNumber,
            BoilerMinGasNumber,
        )
        if number.field in data.settings
    )
    async_add_entities(entities)


class ZoneOffsetNumber(FreedoEntity, RestoreNumber):
    """POST /api/zones/{n}/offset: the manual correction of the room
    temperature.

    No device class: HA would convert a temperature difference as an absolute
    temperature. The correction goes to the measuring device and the daemon
    reports nothing: no captured device frame carries tag 0140, so the node
    Telemetry calib_offset stays empty. The entity shows the last value set
    from Home Assistant and restores it after a restart, so it is empty until
    the first write.
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

    async def async_added_to_hass(self) -> None:
        await super().async_added_to_hass()
        if (last := await self.async_get_last_number_data()) is not None:
            self._written = last.native_value

    @property
    def native_value(self) -> float | None:
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


class AwayTemperatureNumber(FreedoZoneSettingEntity, NumberEntity):
    """The temperature the zone keeps while the home is away
    (away_temperature_c). The room minimum means off in away, and so does a
    zone with no value: the number shows the minimum then."""

    _attr_device_class = NumberDeviceClass.TEMPERATURE
    _attr_entity_category = EntityCategory.CONFIG
    _attr_native_unit_of_measurement = UnitOfTemperature.CELSIUS
    _attr_translation_key = "away_temperature"
    _attr_native_min_value = ZONE_MIN_C
    _attr_native_max_value = ZONE_MAX_C
    _attr_native_step = 0.1
    _attr_mode = NumberMode.BOX

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, zone, "away_temperature_c")

    @property
    def native_value(self) -> float:
        value = (self._setting or {}).get("away_temperature_c")
        return ZONE_MIN_C if value is None else value

    async def async_set_native_value(self, value: float) -> None:
        await self._write(away_temperature_c=round(value, 2))


class FlowLimitNumber(FreedoEntity, NumberEntity):
    """The flow limit (tag 2040) the wall thermostat that drives the circuit is
    served. The thermostat takes a new value by re-opening its OpenTherm
    session, at its next poll. A circuit that follows a weather curve, the
    default curve or the adaptation refuses POST /api/boiler/flow (409): the
    number then shows and edits the manual limit flow_limit_c of PATCH
    /api/install, which the curve and the adaptation start from. The range is
    the circuit's minC and maxC, which the daemon cuts to the boiler's highest
    CH setpoint; a daemon that sends none gets the fixed range."""

    _attr_device_class = NumberDeviceClass.TEMPERATURE
    _attr_native_unit_of_measurement = UnitOfTemperature.CELSIUS
    _attr_entity_category = EntityCategory.CONFIG
    _attr_translation_key = "flow_limit"
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
    def _managed(self) -> bool:
        return flow_managed(self._view or {})

    @property
    def available(self) -> bool:
        return (
            super().available
            and self._view is not None
            and self._circuit in self.coordinator.data.circuit_sources
        )

    @property
    def native_min_value(self) -> float:
        value = (self._view or {}).get("minC")
        return FLOW_LIMIT_MIN_C if value is None else value

    @property
    def native_max_value(self) -> float:
        value = (self._view or {}).get("maxC")
        return FLOW_LIMIT_MAX_C if value is None else value

    @property
    def native_value(self) -> float | None:
        if self._managed:
            return self.coordinator.data.settings.get("flow_limit_c")
        return (self._view or {}).get("servedC")

    async def async_set_native_value(self, value: float) -> None:
        client = self.coordinator.client
        if self._managed:
            view = await call_api(client.patch_install(flow_limit_c=round(value)))
            if isinstance(view, Mapping):
                self.coordinator.data.settings.update(view)
        else:
            source = self.coordinator.data.circuit_sources[self._circuit]
            await call_api(client.set_flow_limit(self._circuit, value, source))
        await self.coordinator.async_request_refresh()


class _BoilerRangeNumber(FreedoEntity, NumberEntity):
    """A boiler range option of the installation (GET and PATCH /api/install).
    The daemon keeps the minimum below the maximum and answers 400 otherwise."""

    _attr_entity_category = EntityCategory.CONFIG
    _attr_mode = NumberMode.BOX
    field: str

    def __init__(self, coordinator: FreedoCoordinator) -> None:
        super().__init__(coordinator, self._attr_translation_key)
        self._attr_device_info = self.hub_device()

    @property
    def native_value(self) -> float | None:
        return self.coordinator.data.settings.get(self.field)

    @property
    def available(self) -> bool:
        return super().available and self.field in self.coordinator.data.settings

    async def async_set_native_value(self, value: float) -> None:
        view = await call_api(self.coordinator.client.patch_install(**{self.field: round(value, 2)}))
        if isinstance(view, Mapping):
            self.coordinator.data.settings.update(view)
            self.coordinator.async_update_listeners()


class _BoilerPowerNumber(_BoilerRangeNumber):
    """The boiler power, which sizes the energy estimate."""

    _attr_device_class = NumberDeviceClass.POWER
    _attr_native_unit_of_measurement = UnitOfPower.KILO_WATT
    _attr_native_max_value = BOILER_POWER_MAX_KW
    _attr_native_step = 0.1


class _BoilerGasNumber(_BoilerRangeNumber):
    """The fuel flow at the burner, which sizes the gas estimate, in the unit of
    options.fuel_unit per hour. Both flows are needed: the minimum has no
    default."""

    _attr_native_max_value = BOILER_GAS_MAX_M3H
    _attr_native_step = 0.01

    @property
    def device_class(self) -> NumberDeviceClass | None:
        return None if fuel_unit(self.coordinator.data) == FUEL_KG else NumberDeviceClass.VOLUME_FLOW_RATE

    @property
    def native_unit_of_measurement(self) -> str:
        return FUEL_FLOW_UNITS[fuel_unit(self.coordinator.data)]


class BoilerMaxPowerNumber(_BoilerPowerNumber):
    """The output of the boiler at 100 % modulation."""

    _attr_translation_key = "boiler_max_power"
    _attr_native_min_value = 0.1
    field = "boiler_max_power_kw"


class BoilerMinPowerNumber(_BoilerPowerNumber):
    """The output of the boiler at 0 % modulation."""

    _attr_translation_key = "boiler_min_power"
    _attr_native_min_value = 0
    field = "boiler_min_power_kw"


class BoilerMaxGasNumber(_BoilerGasNumber):
    """The gas flow of the boiler at 100 % modulation."""

    _attr_translation_key = "boiler_max_gas"
    _attr_native_min_value = 0.01
    field = "boiler_max_gas_m3h"


class BoilerMinGasNumber(_BoilerGasNumber):
    """The gas flow of the boiler at 0 % modulation."""

    _attr_translation_key = "boiler_min_gas"
    _attr_native_min_value = 0
    field = "boiler_min_gas_m3h"
