"""Base entities and device info of the freedo integration."""

from __future__ import annotations

from collections.abc import Awaitable, Callable, Mapping
from functools import partial
from typing import Any

from homeassistant.const import UnitOfVolumeFlowRate
from homeassistant.core import HomeAssistant, callback
from homeassistant.exceptions import HomeAssistantError, ServiceValidationError
from homeassistant.helpers import device_registry as dr
from homeassistant.helpers.device_registry import DeviceInfo
from homeassistant.helpers.entity_platform import async_get_platforms
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .api import FreedoError
from .const import DOMAIN
from .coordinator import FreedoCoordinator
from .model import (
    FUEL_KG,
    FUEL_LITRE,
    FUEL_M3,
    KIND_HOT_WATER,
    Zone,
    node_display,
    node_firmware,
    schedule_payload,
    schedule_programs,
)

# The unit of the fuel flow by options.fuel_unit. A mass flow has no device
# class.
FUEL_FLOW_UNITS = {
    FUEL_M3: UnitOfVolumeFlowRate.CUBIC_METERS_PER_HOUR,
    FUEL_LITRE: UnitOfVolumeFlowRate.LITERS_PER_HOUR,
    FUEL_KG: "kg/h",
}

_MODELS = {"valve": "Smart Radiator Thermostat", "thermostat": "Wired Smart Thermostat"}


def hub_device_info(entry_id: str) -> DeviceInfo:
    """The hub device. async_setup_entry registers it before the platforms,
    so the zone and node devices can name it as their via device."""
    return DeviceInfo(
        identifiers={(DOMAIN, entry_id)},
        name="freedo",
        manufacturer="freedo",
        model="Local coordinator",
    )


@callback
def async_track_firmware(
    hass: HomeAssistant, coordinator: FreedoCoordinator
) -> Callable[[], None]:
    """Keep the firmware of each node device in step with the daemon. The
    device info of an entity is read only when the entity is added, so a
    firmware the device reports after start, or an OTA, needs this write."""
    registry = dr.async_get(hass)
    entry_id = coordinator.config_entry.entry_id
    seen: dict[str, str | None] = {}

    @callback
    def update() -> None:
        for short in coordinator.data.devices:
            firmware = node_firmware(coordinator.data, short)
            if short in seen and seen[short] == firmware:
                continue
            seen[short] = firmware
            device = registry.async_get_device_by_identifier((DOMAIN, f"{entry_id}_{short}"), entry_id)
            if device is not None and device.sw_version != firmware:
                registry.async_update_device(device.id, sw_version=firmware)

    update()
    return coordinator.async_add_listener(update)


class FreedoEntity(CoordinatorEntity[FreedoCoordinator]):
    _attr_has_entity_name = True

    def __init__(self, coordinator: FreedoCoordinator, key: str) -> None:
        super().__init__(coordinator)
        self._attr_unique_id = f"{coordinator.config_entry.entry_id}_{key}"

    @property
    def _hub_device_id(self) -> str:
        return self.coordinator.config_entry.runtime_data.hub_device_id

    def hub_device(self) -> DeviceInfo:
        return hub_device_info(self.coordinator.config_entry.entry_id)

    def zone_device(self, zone: Zone) -> DeviceInfo:
        # The hot water zone shows one translated name, whatever the installation stores.
        return DeviceInfo(
            identifiers={(DOMAIN, f"{self.coordinator.config_entry.entry_id}_zone{zone.number}")},
            name=zone.name,
            translation_key="hot_water_zone" if zone.kind == KIND_HOT_WATER else None,
            manufacturer="tado",
            model=f"{zone.kind} zone",
            via_device_id=self._hub_device_id,
        )

    def node_device(self, short: str) -> DeviceInfo:
        dev = self.coordinator.data.devices.get(short, {})
        name = dev.get("label") or dev.get("device") or short
        return DeviceInfo(
            identifiers={(DOMAIN, f"{self.coordinator.config_entry.entry_id}_{short}")},
            name=f"{name} ({dev.get('device') or short})" if dev.get("label") else name,
            manufacturer="tado",
            model=_MODELS.get(dev.get("kind", ""), dev.get("kind") or None),
            serial_number=dev.get("device"),
            sw_version=node_firmware(self.coordinator.data, short),
            via_device_id=self._hub_device_id,
        )


class FreedoScheduleEntity(FreedoEntity):
    """The entity of a zone with a weekly schedule: a heating zone or the hot
    water zone. The daemon serves both at GET and PUT /api/zones/{n}/schedule
    and keeps each to the temperature range of its zone kind."""

    # The bounds of a schedule entry temperature.
    _schedule_range: tuple[float, float]
    _zone: Zone | None

    def checked_schedule(self, programs: list[dict[str, Any]]) -> list[dict[str, Any]]:
        """The daemon body of a schedule, or a service error naming the problem."""
        try:
            return schedule_payload(programs, *self._schedule_range)
        except ValueError as err:
            raise ServiceValidationError(
                translation_domain=DOMAIN,
                translation_key="invalid_schedule",
                translation_placeholders={"reason": str(err)},
            ) from err

    async def async_get_schedule(self) -> dict[str, Any]:
        """The weekly schedule of the zone, no programs when it has none."""
        zone = self._zone
        if zone is None:
            return {}
        view = await call_api(self.coordinator.client.zone_schedule(zone.number))
        return {"zone": zone.number, "programs": schedule_programs(view or {})}

    async def async_set_schedule(self, programs: list[dict[str, Any]]) -> None:
        zone = self._zone
        if zone is None:
            return
        await self._put_schedule(zone, self.checked_schedule(programs))

    async def async_copy_schedule(self, to: list[str]) -> None:
        """Copy the weekly schedule of the zone to the zones of other freedo
        entities of the same zone kind. The daemon has no copy route: it reads
        this zone and writes each target."""
        zone = self._zone
        if zone is None:
            return
        targets: list[FreedoScheduleEntity] = []
        for entity_id in dict.fromkeys(to):
            target = self._zone_entity(entity_id)
            if target is None or target._zone is None:
                raise ServiceValidationError(
                    translation_domain=DOMAIN,
                    translation_key="not_a_zone",
                    translation_placeholders={"entity_id": entity_id},
                )
            if target is self:
                raise ServiceValidationError(
                    translation_domain=DOMAIN, translation_key="copy_to_itself"
                )
            if target._zone.kind != zone.kind:
                raise ServiceValidationError(
                    translation_domain=DOMAIN,
                    translation_key="copy_across_kinds",
                    translation_placeholders={"entity_id": entity_id},
                )
            targets.append(target)
        view = await call_api(self.coordinator.client.zone_schedule(zone.number))
        if view is None:
            raise ServiceValidationError(
                translation_domain=DOMAIN,
                translation_key="no_schedule",
                translation_placeholders={"zone": zone.name},
            )
        programs = self.checked_schedule(schedule_programs(view))
        await each(
            [(t.entity_id, partial(t._put_schedule, t._zone, programs)) for t in targets]
        )

    def _zone_entity(self, entity_id: str) -> FreedoScheduleEntity | None:
        for platform in async_get_platforms(self.hass, DOMAIN):
            entity = platform.entities.get(entity_id)
            if isinstance(entity, FreedoScheduleEntity):
                return entity
        return None

    async def _put_schedule(self, zone: Zone, programs: list[dict[str, Any]]) -> None:
        await call_api(self.coordinator.client.set_zone_schedule(zone.number, programs))
        await self.coordinator.async_request_refresh()


class FreedoNodeEntity(FreedoEntity):
    """An entity of one device, by its short address."""

    def __init__(self, coordinator: FreedoCoordinator, short: str, key: str) -> None:
        super().__init__(coordinator, f"{short}_{key}")
        self._short = short
        self._attr_device_info = self.node_device(short)

    @property
    def _node(self) -> Mapping[str, Any]:
        return self.coordinator.data.nodes.get(self._short) or {}

    @property
    def available(self) -> bool:
        return super().available and self._short in self.coordinator.data.nodes


class FreedoZoneSettingEntity(FreedoEntity):
    """A setting of one heating zone: the row of GET /api/install/zones,
    written with PATCH /api/install/zones/{n}."""

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone, key: str) -> None:
        super().__init__(coordinator, f"zone{zone.number}_{key}")
        self._number = zone.number
        self._attr_device_info = self.zone_device(zone)

    @property
    def _setting(self) -> Mapping[str, Any] | None:
        return self.coordinator.data.zone_settings.get(self._number)

    @property
    def available(self) -> bool:
        return super().available and self._setting is not None

    async def _write(self, **fields: Any) -> None:
        result = await call_api(
            self.coordinator.client.patch_install_zone(self._number, **fields)
        )
        row = result.get("zone") if isinstance(result, Mapping) else None
        if isinstance(row, Mapping):
            self.coordinator.data.zone_settings[self._number] = dict(row)
            self.coordinator.async_update_listeners()


class FreedoDisplayEntity(FreedoEntity):
    """A display setting of one valve: GET /api/nodes Display, written with
    PUT /api/nodes/{id}/display."""

    def __init__(self, coordinator: FreedoCoordinator, short: str, key: str) -> None:
        super().__init__(coordinator, f"{short}_{key}")
        self._short = short
        self._attr_device_info = self.node_device(short)

    @property
    def _display(self) -> Mapping[str, Any] | None:
        return node_display(self.coordinator.data, self._short)

    @property
    def available(self) -> bool:
        return super().available and self._display is not None

    async def _write(self, **fields: Any) -> None:
        result = await call_api(self.coordinator.client.set_display(self._short, **fields))
        display = result.get("display") if isinstance(result, Mapping) else None
        if isinstance(display, Mapping):
            self.coordinator.data.nodes.setdefault(self._short, {})["Display"] = dict(display)
            self.coordinator.async_update_listeners()


async def call_api(coro) -> Any:
    """Run one API write and turn its error into a Home Assistant error."""
    try:
        return await coro
    except FreedoError as err:
        raise HomeAssistantError(f"freedo: {err}") from err


async def each(calls: list[tuple[str, Callable[[], Awaitable[None]]]]) -> None:
    """Run every call. A zone the daemon refuses does not stop the others: the
    failures come back as one error."""
    failures: list[str] = []
    for entity_id, call in calls:
        try:
            await call()
        except HomeAssistantError as err:
            failures.append(f"{entity_id}: {err}")
    if failures:
        raise HomeAssistantError(
            translation_domain=DOMAIN,
            translation_key="zones_failed",
            translation_placeholders={"failures": "; ".join(failures)},
        )
