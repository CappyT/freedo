"""Binary sensors: boiler fault, zone window, heating and fallback, device
alerts."""

from __future__ import annotations

from typing import Any

from homeassistant.components.binary_sensor import (
    BinarySensorDeviceClass,
    BinarySensorEntity,
)
from homeassistant.const import EntityCategory
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FreedoEntity
from .model import KIND_BRIDGE, KIND_VALVE, Zone, boiler_fault_codes, zone_heating


async def async_setup_entry(
    hass: HomeAssistant,
    entry: FreedoConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    coordinator = entry.runtime_data.coordinator
    data = coordinator.data
    entities: list[BinarySensorEntity] = [BoilerFault(coordinator)]
    for zone in data.heating_zones():
        entities.append(ZoneWindow(coordinator, zone))
        entities.append(ZoneHeating(coordinator, zone))
    for zone in sorted(data.zones.values(), key=lambda z: z.number):
        entities.append(ZoneFallback(coordinator, zone))
    for short, dev in sorted(data.devices.items()):
        if dev.get("kind") == KIND_BRIDGE:
            continue
        entities.append(DeviceAlert(coordinator, short, "lowBattery"))
        entities.append(DeviceAlert(coordinator, short, "fault"))
        # A daemon older than the link bits of the fault word sends no list.
        if "commsWarnings" in dev:
            entities.append(DeviceAlert(coordinator, short, "commsWarnings"))
        # Only the valve has a state code, and only a daemon that decodes it
        # sends the list.
        if dev.get("kind") == KIND_VALVE and "deviceWarnings" in dev:
            entities.append(DeviceAlert(coordinator, short, "deviceWarnings"))
        entities.append(DeviceConnectivity(coordinator, short))
    async_add_entities(entities)


class BoilerFault(FreedoEntity, BinarySensorEntity):
    """GET /api/boiler FaultCodes: on while the last report of the boiler holds
    a fault code, unknown before a report. The attribute codes lists them. The
    codes are those of the boiler maker: the manual of the boiler gives their
    meaning."""

    _attr_device_class = BinarySensorDeviceClass.PROBLEM
    _attr_entity_category = EntityCategory.DIAGNOSTIC
    _attr_translation_key = "boiler_fault"

    def __init__(self, coordinator: FreedoCoordinator) -> None:
        super().__init__(coordinator, "boiler_fault")
        self._attr_device_info = self.hub_device()

    @property
    def is_on(self) -> bool | None:
        codes = boiler_fault_codes(self.coordinator.data)
        return bool(codes) if codes is not None else None

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        return {"codes": boiler_fault_codes(self.coordinator.data) or []}


class ZoneWindow(FreedoEntity, BinarySensorEntity):
    """GET /api/zones declared[].window: open from the valve detector
    ("valve"), from Home Assistant ("external") or from both ("both")."""

    _attr_device_class = BinarySensorDeviceClass.WINDOW
    _attr_translation_key = "zone_window"

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, f"zone{zone.number}_window")
        self._number = zone.number
        self._attr_device_info = self.zone_device(zone)

    @property
    def _window(self) -> dict[str, Any] | None:
        return self.coordinator.data.windows.get(self._number)

    @property
    def available(self) -> bool:
        return super().available and self._window is not None

    @property
    def is_on(self) -> bool | None:
        window = self._window
        return bool(window.get("open")) if window else None

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        window = self._window or {}
        return {
            "source": window.get("source"),
            "since": window.get("since"),
            "deadline": window.get("deadline"),
            "undelivered": window.get("undelivered") or [],
        }


class ZoneHeating(FreedoEntity, BinarySensorEntity):
    """GET /api/zones demand DemandPct above 0: the zone calls for heat, as the
    hvac_action of the climate entity. The recorder keeps the state of this
    entity, which history_stats counts, and not an attribute."""

    _attr_device_class = BinarySensorDeviceClass.RUNNING
    _attr_translation_key = "zone_heating"

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, f"zone{zone.number}_heating")
        self._number = zone.number
        self._attr_device_info = self.zone_device(zone)

    @property
    def is_on(self) -> bool | None:
        zone = self.coordinator.data.zones.get(self._number)
        return zone_heating(self.coordinator.data, zone) if zone else None


class ZoneFallback(FreedoEntity, BinarySensorEntity):
    """GET /api/health zoneFallback: the wall thermostat reports the zone in
    fallback (tag 6460)."""

    _attr_device_class = BinarySensorDeviceClass.PROBLEM
    _attr_entity_category = EntityCategory.DIAGNOSTIC
    _attr_translation_key = "zone_fallback"

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, f"zone{zone.number}_fallback")
        self._number = zone.number
        self._attr_device_info = self.zone_device(zone)

    @property
    def is_on(self) -> bool:
        return self._number in self.coordinator.data.fallback

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        fb = self.coordinator.data.fallback.get(self._number) or {}
        return {"mode": fb.get("name"), "source": fb.get("source"), "since": fb.get("since")}


_ALERTS = {
    "lowBattery": (BinarySensorDeviceClass.BATTERY, "low_battery"),
    "fault": (BinarySensorDeviceClass.PROBLEM, "fault"),
    "commsWarnings": (BinarySensorDeviceClass.PROBLEM, "comms_warning"),
    "deviceWarnings": (BinarySensorDeviceClass.PROBLEM, "valve_warning"),
}


class _DeviceEntity(FreedoEntity):
    def __init__(self, coordinator: FreedoCoordinator, short: str, key: str) -> None:
        super().__init__(coordinator, f"{short}_{key}")
        self._short = short
        self._attr_device_info = self.node_device(short)

    @property
    def _device(self) -> dict[str, Any] | None:
        return self.coordinator.data.devices.get(self._short)

    @property
    def available(self) -> bool:
        return super().available and self._device is not None


class DeviceAlert(_DeviceEntity, BinarySensorEntity):
    """GET /api/health devices[].lowBattery, .fault, .commsWarnings or
    .deviceWarnings. The communication warnings are the link bits of the fault
    word, which a missed answer sets and the next one clears. The device
    warnings are valve states such as not mounted. Neither is a device fault."""

    _attr_entity_category = EntityCategory.DIAGNOSTIC

    def __init__(self, coordinator: FreedoCoordinator, short: str, field: str) -> None:
        super().__init__(coordinator, short, field)
        self._field = field
        self._attr_device_class, self._attr_translation_key = _ALERTS[field]

    @property
    def is_on(self) -> bool | None:
        dev = self._device
        return bool(dev.get(self._field)) if dev else None

    @property
    def extra_state_attributes(self) -> dict[str, Any] | None:
        dev = self._device or {}
        if self._field == "fault":
            return {
                "fault_word": dev.get("faultWord"),
                "faults": dev.get("faults") or [],
                "fallback_mode": dev.get("fallbackMode"),
            }
        if self._field in ("commsWarnings", "deviceWarnings"):
            return {"warnings": dev.get(self._field) or []}
        return {"battery_mv": dev.get("batteryMV")}


class DeviceConnectivity(_DeviceEntity, BinarySensorEntity):
    """GET /api/health devices[].offline, inverted: on means heard within
    offline_after_sec."""

    _attr_device_class = BinarySensorDeviceClass.CONNECTIVITY
    _attr_entity_category = EntityCategory.DIAGNOSTIC

    def __init__(self, coordinator: FreedoCoordinator, short: str) -> None:
        super().__init__(coordinator, short, "connectivity")

    @property
    def is_on(self) -> bool | None:
        dev = self._device
        return not dev.get("offline") if dev else None

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        return {"last_seen": (self._device or {}).get("lastSeen")}
