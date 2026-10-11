"""Sensors: device battery, boiler values, the boiler energy and gas estimates
and the flow limit state."""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

from homeassistant.components.sensor import (
    SensorDeviceClass,
    SensorEntity,
    SensorEntityDescription,
    SensorStateClass,
)
from homeassistant.const import (
    PERCENTAGE,
    EntityCategory,
    UnitOfElectricPotential,
    UnitOfEnergy,
    UnitOfMass,
    UnitOfPower,
    UnitOfPressure,
    UnitOfTemperature,
    UnitOfTime,
    UnitOfVolume,
)
from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FUEL_FLOW_UNITS, FreedoEntity, FreedoNodeEntity
from .model import (
    CALIBRATION_STATES,
    ENERGY_REASONS,
    ENERGY_RUNNING,
    FUEL_KG,
    FUEL_LITRE,
    FUEL_M3,
    GAS_REASONS,
    KIND_BRIDGE,
    KIND_VALVE,
    FreedoData,
    Zone,
    boiler_energy,
    boiler_gas,
    boiler_unsupported,
    energy_state,
    fuel_unit,
    gas_state,
    node_calibration,
    node_firmware,
    node_humidity,
    node_temperature,
    zone_demand_pct,
)


def _boiler(key: str) -> Callable[[FreedoData], Any]:
    def value(data: FreedoData) -> Any:
        return data.boiler.get(key) if data.boiler.get("Present") else None

    return value


def _outdoor(key: str) -> Callable[[FreedoData], Any]:
    def value(data: FreedoData) -> Any:
        outdoor = (data.boiler.get("flowLimit") or {}).get("outdoor")
        return outdoor.get(key) if outdoor else None

    return value


def _energy(key: str) -> Callable[[FreedoData], Any]:
    def value(data: FreedoData) -> Any:
        energy = boiler_energy(data)
        return energy.get(key) if energy else None

    return value


def _energy_power(data: FreedoData) -> Any:
    """The output now. Unknown while the estimate does not run: the daemon
    sends 0 then."""
    energy = boiler_energy(data)
    return energy.get("powerKW") if energy and energy.get("running") else None


def _gas(key: str) -> Callable[[FreedoData], Any]:
    def value(data: FreedoData) -> Any:
        gas = boiler_gas(data)
        return gas.get(key) if gas else None

    return value


def _gas_flow(data: FreedoData) -> Any:
    """The gas flow now. Unknown while the estimate does not run: the daemon
    sends 0 then."""
    gas = boiler_gas(data)
    return gas.get("flowM3H") if gas and gas.get("running") else None


@dataclass(frozen=True, kw_only=True)
class FreedoSensorDescription(SensorEntityDescription):
    value_fn: Callable[[FreedoData], Any]
    # The boiler catalogue name of the value, skipped when the boiler lists it
    # as unsupported.
    boiler_attr: str | None = None


_TEMP = {
    "device_class": SensorDeviceClass.TEMPERATURE,
    "native_unit_of_measurement": UnitOfTemperature.CELSIUS,
    "state_class": SensorStateClass.MEASUREMENT,
}

# GET /api/boiler: the boiler bus values the wall thermostat relays, and
# flowLimit with the outdoor reading the daemon holds.
BOILER_SENSORS = (
    FreedoSensorDescription(
        key="flow_temperature", translation_key="flow_temperature", value_fn=_boiler("FlowC"),
        boiler_attr="boiler_flow_C", **_TEMP,
    ),
    FreedoSensorDescription(
        key="return_temperature", translation_key="return_temperature", value_fn=_boiler("ReturnC"),
        boiler_attr="return_water_C", **_TEMP,
    ),
    FreedoSensorDescription(
        key="dhw_temperature", translation_key="dhw_temperature", value_fn=_boiler("DHWC"),
        boiler_attr="dhw_temp_C", **_TEMP,
    ),
    FreedoSensorDescription(
        key="modulation",
        translation_key="modulation",
        native_unit_of_measurement=PERCENTAGE,
        state_class=SensorStateClass.MEASUREMENT,
        value_fn=_boiler("ModulationPct"),
        boiler_attr="boiler_mod",
    ),
    FreedoSensorDescription(
        key="ch_pressure",
        translation_key="ch_pressure",
        device_class=SensorDeviceClass.PRESSURE,
        native_unit_of_measurement=UnitOfPressure.BAR,
        state_class=SensorStateClass.MEASUREMENT,
        value_fn=_boiler("ChPressureBar"),
        boiler_attr="ch_pressure_bar",
    ),
    FreedoSensorDescription(key="outdoor_temperature", translation_key="outdoor_temperature", value_fn=_outdoor("tempC"), **_TEMP),
    FreedoSensorDescription(
        key="outdoor_age",
        translation_key="outdoor_age",
        device_class=SensorDeviceClass.DURATION,
        native_unit_of_measurement=UnitOfTime.SECONDS,
        entity_category=EntityCategory.DIAGNOSTIC,
        value_fn=_outdoor("ageSec"),
    ),
    FreedoSensorDescription(
        key="flow_limit_default",
        translation_key="flow_limit_default",
        device_class=SensorDeviceClass.TEMPERATURE,
        native_unit_of_measurement=UnitOfTemperature.CELSIUS,
        entity_category=EntityCategory.DIAGNOSTIC,
        value_fn=lambda d: (d.boiler.get("flowLimit") or {}).get("defaultC"),
    ),
)

# GET /api/boiler energy: an estimate of the heat the boiler delivers, from the
# modulation and the boiler power of the installation. The two sensors need the
# maximum power: the estimate cannot run without it.
ENERGY_SENSORS = (
    FreedoSensorDescription(
        key="heat_energy",
        translation_key="heat_energy",
        device_class=SensorDeviceClass.ENERGY,
        native_unit_of_measurement=UnitOfEnergy.KILO_WATT_HOUR,
        state_class=SensorStateClass.TOTAL_INCREASING,
        value_fn=_energy("totalKWh"),
    ),
    FreedoSensorDescription(
        key="heat_power",
        translation_key="heat_power",
        device_class=SensorDeviceClass.POWER,
        native_unit_of_measurement=UnitOfPower.KILO_WATT,
        state_class=SensorStateClass.MEASUREMENT,
        value_fn=_energy_power,
    ),
)

ENERGY_STATUS = FreedoSensorDescription(
    key="energy_estimate",
    translation_key="energy_estimate",
    device_class=SensorDeviceClass.ENUM,
    options=[ENERGY_RUNNING, *ENERGY_REASONS.values()],
    entity_category=EntityCategory.DIAGNOSTIC,
    value_fn=energy_state,
)

# GET /api/boiler energy.gas: an estimate of the fuel the boiler burns, from the
# modulation and the flow range of the installation. The two sensors need both
# flows: the estimate cannot run without them. The unit of both follows
# options.fuel_unit, see GasSensor.
GAS_SENSORS = (
    FreedoSensorDescription(
        key="gas_volume",
        translation_key="gas_volume",
        state_class=SensorStateClass.TOTAL_INCREASING,
        value_fn=_gas("totalM3"),
    ),
    FreedoSensorDescription(
        key="gas_flow",
        translation_key="gas_flow",
        state_class=SensorStateClass.MEASUREMENT,
        value_fn=_gas_flow,
    ),
)

# The device class and unit of the fuel counter by options.fuel_unit: natural
# gas counts as gas, oil as a volume and LPG or pellets as a weight.
FUEL_TOTALS = {
    FUEL_M3: (SensorDeviceClass.GAS, UnitOfVolume.CUBIC_METERS),
    FUEL_LITRE: (SensorDeviceClass.VOLUME, UnitOfVolume.LITERS),
    FUEL_KG: (SensorDeviceClass.WEIGHT, UnitOfMass.KILOGRAMS),
}

GAS_STATUS = FreedoSensorDescription(
    key="gas_estimate",
    translation_key="gas_estimate",
    device_class=SensorDeviceClass.ENUM,
    options=[ENERGY_RUNNING, *GAS_REASONS.values()],
    entity_category=EntityCategory.DIAGNOSTIC,
    value_fn=gas_state,
)


async def async_setup_entry(
    hass: HomeAssistant,
    entry: FreedoConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    coordinator = entry.runtime_data.coordinator
    data = coordinator.data
    unsupported = boiler_unsupported(data)
    entities: list[SensorEntity] = [
        BoilerSensor(coordinator, d) for d in BOILER_SENSORS if d.boiler_attr not in unsupported
    ]
    if boiler_energy(data) is not None:
        entities.append(BoilerSensor(coordinator, ENERGY_STATUS))
    if boiler_gas(data) is not None:
        entities.append(BoilerSensor(coordinator, GAS_STATUS))
    for circuit in (data.boiler.get("flowLimit") or {}).get("circuits") or []:
        entities.append(FlowLimitSensor(coordinator, int(circuit["id"]), "servedC"))
        entities.append(FlowLimitSensor(coordinator, int(circuit["id"]), "computedC"))
    for short, dev in sorted(data.devices.items()):
        if dev.get("kind") == KIND_BRIDGE:
            continue
        entities.append(BatterySensor(coordinator, short))
        entities.append(BatteryVoltageSensor(coordinator, short))
        entities.append(FirmwareSensor(coordinator, short))
        entities.append(DeviceTemperatureSensor(coordinator, short))
        if dev.get("kind") == KIND_VALVE:
            entities.append(CalibrationSensor(coordinator, short))
    entities.extend(ZoneDemandSensor(coordinator, zone) for zone in data.heating_zones())
    async_add_entities(entities)

    # A device that reports a humidity gets its sensor once it has: the node
    # view carries the reading only after the first report.
    humidity: set[str] = set()

    @callback
    def add_humidity() -> None:
        new = [
            short
            for short, dev in sorted(coordinator.data.devices.items())
            if short not in humidity
            and dev.get("kind") != KIND_BRIDGE
            and node_humidity(coordinator.data, short) is not None
        ]
        humidity.update(new)
        async_add_entities(DeviceHumiditySensor(coordinator, short) for short in new)

    add_humidity()
    entry.async_on_unload(coordinator.async_add_listener(add_humidity))

    # The energy sensors appear once the installation holds the boiler power.
    estimate = False

    @callback
    def add_estimate() -> None:
        nonlocal estimate
        energy = boiler_energy(coordinator.data)
        if estimate or not energy or energy.get("maxPowerKW") is None:
            return
        estimate = True
        async_add_entities(BoilerSensor(coordinator, d) for d in ENERGY_SENSORS)

    add_estimate()
    entry.async_on_unload(coordinator.async_add_listener(add_estimate))

    # The gas sensors appear once the installation holds both gas flows.
    gas_estimate = False

    @callback
    def add_gas_estimate() -> None:
        nonlocal gas_estimate
        gas = boiler_gas(coordinator.data)
        if gas_estimate or not gas or gas.get("maxFlowM3H") is None or gas.get("minFlowM3H") is None:
            return
        gas_estimate = True
        async_add_entities(GasSensor(coordinator, d) for d in GAS_SENSORS)

    add_gas_estimate()
    entry.async_on_unload(coordinator.async_add_listener(add_gas_estimate))


class BoilerSensor(FreedoEntity, SensorEntity):
    entity_description: FreedoSensorDescription

    def __init__(self, coordinator: FreedoCoordinator, description: FreedoSensorDescription) -> None:
        super().__init__(coordinator, f"boiler_{description.key}")
        self.entity_description = description
        self._attr_device_info = self.hub_device()

    @property
    def native_value(self) -> Any:
        return self.entity_description.value_fn(self.coordinator.data)

    @property
    def extra_state_attributes(self) -> dict[str, Any] | None:
        if self.entity_description.key != "outdoor_temperature":
            return None
        outdoor = (self.coordinator.data.boiler.get("flowLimit") or {}).get("outdoor") or {}
        return {"at": outdoor.get("at"), "stale": outdoor.get("stale")}


class GasSensor(BoilerSensor):
    """The fuel counter or flow, in the unit of options.fuel_unit."""

    @property
    def device_class(self) -> SensorDeviceClass | None:
        unit = fuel_unit(self.coordinator.data)
        if self.entity_description.key == "gas_volume":
            return FUEL_TOTALS[unit][0]
        return None if unit == FUEL_KG else SensorDeviceClass.VOLUME_FLOW_RATE

    @property
    def native_unit_of_measurement(self) -> str:
        unit = fuel_unit(self.coordinator.data)
        if self.entity_description.key == "gas_volume":
            return FUEL_TOTALS[unit][1]
        return FUEL_FLOW_UNITS[unit]


class FlowLimitSensor(FreedoEntity, SensorEntity):
    """GET /api/boiler flowLimit.circuits[]: the flow limit served to the
    thermostat, or the one the weather curve computes. reduction_c is how far
    the default curve puts the limit under the manual limit now."""

    _attr_device_class = SensorDeviceClass.TEMPERATURE
    _attr_native_unit_of_measurement = UnitOfTemperature.CELSIUS

    def __init__(self, coordinator: FreedoCoordinator, circuit: int, field: str) -> None:
        super().__init__(coordinator, f"circuit{circuit}_{field}")
        self._circuit = circuit
        self._field = field
        self._attr_translation_key = "flow_limit_served" if field == "servedC" else "flow_limit_computed"
        self._attr_translation_placeholders = {"circuit": str(circuit)}
        self._attr_device_info = self.hub_device()

    @property
    def _view(self) -> dict[str, Any] | None:
        for circuit in (self.coordinator.data.boiler.get("flowLimit") or {}).get("circuits") or []:
            if circuit.get("id") == self._circuit:
                return circuit
        return None

    @property
    def native_value(self) -> Any:
        view = self._view
        if view is None:
            return None
        return view.get(self._field)

    @property
    def extra_state_attributes(self) -> dict[str, Any] | None:
        view = self._view or {}
        return {
            "curve": view.get("curve"),
            "default_curve": view.get("defaultCurve"),
            "reduction_c": view.get("reductionC"),
            "fetched": view.get("fetched"),
            "changed_at": view.get("changedAt"),
        }


class _BatteryBase(FreedoEntity, SensorEntity):
    _attr_state_class = SensorStateClass.MEASUREMENT

    def __init__(self, coordinator: FreedoCoordinator, short: str, key: str) -> None:
        super().__init__(coordinator, f"{short}_{key}")
        self._short = short
        self._attr_device_info = self.node_device(short)


class BatterySensor(_BatteryBase):
    """GET /api/nodes BatteryPct, derived by the daemon from tag 0162."""

    _attr_device_class = SensorDeviceClass.BATTERY
    _attr_native_unit_of_measurement = PERCENTAGE

    def __init__(self, coordinator: FreedoCoordinator, short: str) -> None:
        super().__init__(coordinator, short, "battery")

    @property
    def native_value(self) -> int | None:
        node = self.coordinator.data.nodes.get(self._short) or {}
        return node.get("BatteryPct") if node.get("BatteryMV") else None


class BatteryVoltageSensor(_BatteryBase):
    """GET /api/health devices[].batteryMV."""

    _attr_device_class = SensorDeviceClass.VOLTAGE
    _attr_native_unit_of_measurement = UnitOfElectricPotential.MILLIVOLT
    _attr_entity_category = EntityCategory.DIAGNOSTIC
    _attr_translation_key = "battery_voltage"

    def __init__(self, coordinator: FreedoCoordinator, short: str) -> None:
        super().__init__(coordinator, short, "battery_voltage")

    @property
    def native_value(self) -> int | None:
        dev = self.coordinator.data.devices.get(self._short) or {}
        return dev.get("batteryMV") or None


class CalibrationSensor(FreedoNodeEntity, SensorEntity):
    """The motor calibration state of a valve: Telemetry calib_state of
    GET /api/nodes, from the d/<serial>/mnt report the valve sends after a
    calibration. calib_error is the error code of a failed run."""

    _attr_device_class = SensorDeviceClass.ENUM
    _attr_options = list(CALIBRATION_STATES.values())
    _attr_entity_category = EntityCategory.DIAGNOSTIC
    _attr_translation_key = "calibration"

    def __init__(self, coordinator: FreedoCoordinator, short: str) -> None:
        super().__init__(coordinator, short, "calibration")

    @property
    def native_value(self) -> str | None:
        return node_calibration(self.coordinator.data, self._short)[0]

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        return {"error_code": node_calibration(self.coordinator.data, self._short)[1]}


class FirmwareSensor(FreedoNodeEntity, SensorEntity):
    """The running firmware of a device: Telemetry fw_run_version of
    GET /api/nodes, from the fw/state the device reports."""

    _attr_entity_category = EntityCategory.DIAGNOSTIC
    _attr_translation_key = "firmware_version"

    def __init__(self, coordinator: FreedoCoordinator, short: str) -> None:
        super().__init__(coordinator, short, "firmware")

    @property
    def native_value(self) -> str | None:
        return node_firmware(self.coordinator.data, self._short)


class DeviceTemperatureSensor(FreedoNodeEntity, SensorEntity):
    """The temperature one device reads (tag 012d), on the wire scale: without
    the correction of the zone. The climate entity shows the zone temperature."""

    _attr_device_class = SensorDeviceClass.TEMPERATURE
    _attr_native_unit_of_measurement = UnitOfTemperature.CELSIUS
    _attr_state_class = SensorStateClass.MEASUREMENT
    _attr_entity_category = EntityCategory.DIAGNOSTIC
    _attr_entity_registry_enabled_default = False

    def __init__(self, coordinator: FreedoCoordinator, short: str) -> None:
        super().__init__(coordinator, short, "temperature")

    @property
    def native_value(self) -> float | None:
        return node_temperature(self.coordinator.data, self._short)


class DeviceHumiditySensor(FreedoNodeEntity, SensorEntity):
    """The relative humidity one device reads (tag 0135)."""

    _attr_device_class = SensorDeviceClass.HUMIDITY
    _attr_native_unit_of_measurement = PERCENTAGE
    _attr_state_class = SensorStateClass.MEASUREMENT
    _attr_entity_category = EntityCategory.DIAGNOSTIC
    _attr_entity_registry_enabled_default = False

    def __init__(self, coordinator: FreedoCoordinator, short: str) -> None:
        super().__init__(coordinator, short, "humidity")

    @property
    def native_value(self) -> float | None:
        return node_humidity(self.coordinator.data, self._short)


class ZoneDemandSensor(FreedoEntity, SensorEntity):
    """The heat demand of a zone, 0..100 (tag 40a0 of the zone report)."""

    _attr_native_unit_of_measurement = PERCENTAGE
    _attr_state_class = SensorStateClass.MEASUREMENT
    _attr_translation_key = "heating_demand"

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, f"zone{zone.number}_demand")
        self._number = zone.number
        self._attr_device_info = self.zone_device(zone)

    @property
    def native_value(self) -> int | None:
        zone = self.coordinator.data.zones.get(self._number)
        return zone_demand_pct(self.coordinator.data, zone) if zone else None
