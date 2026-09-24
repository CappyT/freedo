"""Away switch: the home state of GET and PATCH /api/home. Notification
switch of each valve: the change notification of d/config tag 0158."""

from __future__ import annotations

from typing import Any

from homeassistant.components.switch import SwitchEntity
from homeassistant.const import EntityCategory
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FreedoDisplayEntity, FreedoEntity, call_api
from .model import KIND_VALVE


async def async_setup_entry(
    hass: HomeAssistant,
    entry: FreedoConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    coordinator = entry.runtime_data.coordinator
    entities: list[SwitchEntity] = [AwaySwitch(coordinator)]
    for short, dev in sorted(coordinator.data.devices.items()):
        if dev.get("kind") == KIND_VALVE:
            entities.append(NotificationSwitch(coordinator, short))
    async_add_entities(entities)


class AwaySwitch(FreedoEntity, SwitchEntity):
    _attr_translation_key = "away"

    def __init__(self, coordinator: FreedoCoordinator) -> None:
        super().__init__(coordinator, "away")
        self._attr_device_info = self.hub_device()

    @property
    def is_on(self) -> bool:
        return self.coordinator.data.away

    async def _set(self, away: bool) -> None:
        await call_api(self.coordinator.client.set_away(away))
        self.coordinator.data.away = away
        self.async_write_ha_state()

    async def async_turn_on(self, **kwargs: Any) -> None:
        await self._set(True)

    async def async_turn_off(self, **kwargs: Any) -> None:
        await self._set(False)


class NotificationSwitch(FreedoDisplayEntity, SwitchEntity):
    """The valve shows a notification when the zone changes remotely."""

    _attr_translation_key = "notification"
    _attr_entity_category = EntityCategory.CONFIG

    def __init__(self, coordinator: FreedoCoordinator, short: str) -> None:
        super().__init__(coordinator, short, "notification")

    @property
    def is_on(self) -> bool | None:
        value = (self._display or {}).get("notification")
        return value if isinstance(value, bool) else None

    async def async_turn_on(self, **kwargs: Any) -> None:
        await self._write(notification=True)

    async def async_turn_off(self, **kwargs: Any) -> None:
        await self._write(notification=False)
