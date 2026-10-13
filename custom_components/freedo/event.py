"""Event entity Alert of the hub: one event when a device or the boiler starts
to report a problem. An automation, such as the notification blueprint, sends
it on. The binary sensors show the same problems as states."""

from __future__ import annotations

from datetime import datetime
from typing import Any

from homeassistant.components.event import EventEntity
from homeassistant.core import CALLBACK_TYPE, HomeAssistant, callback
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback
from homeassistant.helpers.event import async_call_later

from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FreedoEntity
from .model import ALERTS, active_alerts


async def async_setup_entry(
    hass: HomeAssistant,
    entry: FreedoConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    async_add_entities([AlertEvent(entry.runtime_data.coordinator)])


class AlertEvent(FreedoEntity, EventEntity):
    """An event for each alert that starts between two polls. The alerts that
    hold when the integration starts raise none, and an alert raises no second
    event while it holds: so a restart or a reload repeats nothing. The event
    attributes are room, device and details.

    The state of an event entity is the time of its last event, and a trigger
    on the entity fires when that state changes. So the alerts of one poll go
    out one second apart."""

    _attr_translation_key = "alert"
    _attr_event_types = list(ALERTS)

    def __init__(self, coordinator: FreedoCoordinator) -> None:
        super().__init__(coordinator, "alert")
        self._attr_device_info = self.hub_device()
        self._active: dict[tuple[str, str], dict[str, Any]] = active_alerts(coordinator.data)
        self._queue: list[tuple[str, dict[str, Any]]] = []
        self._unsub_next: CALLBACK_TYPE | None = None

    async def async_will_remove_from_hass(self) -> None:
        if self._unsub_next is not None:
            self._unsub_next()
            self._unsub_next = None

    @callback
    def _handle_coordinator_update(self) -> None:
        active = active_alerts(self.coordinator.data)
        self._queue.extend((key[0], active[key]) for key in sorted(active.keys() - self._active.keys()))
        self._active = active
        if self._unsub_next is None:
            self._raise_next()

    @callback
    def _raise_next(self, _: datetime | None = None) -> None:
        self._unsub_next = None
        if not self._queue:
            return
        self._trigger_event(*self._queue.pop(0))
        self.async_write_ha_state()
        if self._queue:
            self._unsub_next = async_call_later(self.hass, 1, self._raise_next)
