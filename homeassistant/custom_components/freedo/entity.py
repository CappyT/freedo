"""Base entities and device info of the freedo integration."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers.device_registry import DeviceInfo
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .api import FreedoError
from .const import DOMAIN
from .coordinator import FreedoCoordinator
from .model import Zone, node_display

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
        return DeviceInfo(
            identifiers={(DOMAIN, f"{self.coordinator.config_entry.entry_id}_zone{zone.number}")},
            name=zone.name,
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
            via_device_id=self._hub_device_id,
        )


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
