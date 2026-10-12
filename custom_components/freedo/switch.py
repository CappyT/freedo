"""Away switch: the home state of GET and PATCH /api/home. Offline schedule
switch: options.offline_schedule of GET and PATCH /api/install. Notification
switch of each valve: the change notification of d/config tag 0158. Child lock
switch of each valve: d/lock. Early start and open window detection switch of
each heating zone: the zone row of GET and PATCH /api/install/zones. Pairing
mode switch: the pair window of GET and POST /api/pair/window. Default weather
curve and flow adaptation switch: options.flow_default_curve and
options.flow_auto_adapt of GET and PATCH /api/install."""

from __future__ import annotations

import time
from collections.abc import Mapping
from typing import Any

from homeassistant.components.switch import SwitchEntity
from homeassistant.const import EntityCategory
from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .const import PENDING_SEC
from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import (
    FreedoDisplayEntity,
    FreedoEntity,
    FreedoNodeEntity,
    FreedoZoneSettingEntity,
    call_api,
)
from .model import (
    KIND_VALVE,
    PAIR_WINDOW_MAX_SEC,
    Zone,
    in_zone,
    node_child_lock,
    zone_setting,
)


async def async_setup_entry(
    hass: HomeAssistant,
    entry: FreedoConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    coordinator = entry.runtime_data.coordinator
    entities: list[SwitchEntity] = [
        AwaySwitch(coordinator),
        OfflineScheduleSwitch(coordinator),
    ]
    entities.extend(
        switch(coordinator)
        for switch in (FlowDefaultCurveSwitch, FlowAutoAdaptSwitch)
        if switch.field in coordinator.data.settings
    )
    if coordinator.data.pair_window is not None:
        entities.append(PairingModeSwitch(coordinator))
    for short, dev in sorted(coordinator.data.devices.items()):
        if dev.get("kind") == KIND_VALVE and in_zone(coordinator.data, short):
            entities.append(NotificationSwitch(coordinator, short))
            # The node view serves ChildLock on a valve alone.
            if node_child_lock(coordinator.data, short) is not None:
                entities.append(ChildLockSwitch(coordinator, short))
    for zone in coordinator.data.heating_zones():
        for switch in (EarlyStartSwitch, OpenWindowDetectionSwitch, AutoOffsetSwitch):
            if zone_setting(coordinator.data, zone, switch.field) is not None:
                entities.append(switch(coordinator, zone))
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


class OfflineScheduleSwitch(FreedoEntity, SwitchEntity):
    """d/config tag 02b3 of every device without a per-device value: on, each
    device fetches its weekly program and runs it without the coordinator."""

    _attr_translation_key = "offline_schedule"
    _attr_entity_category = EntityCategory.CONFIG

    def __init__(self, coordinator: FreedoCoordinator) -> None:
        super().__init__(coordinator, "offline_schedule")
        self._attr_device_info = self.hub_device()

    @property
    def is_on(self) -> bool | None:
        return self.coordinator.data.offline_schedule

    async def _set(self, enabled: bool) -> None:
        await call_api(self.coordinator.client.set_offline_schedule(enabled))
        self.coordinator.data.offline_schedule = enabled
        self.async_write_ha_state()

    async def async_turn_on(self, **kwargs: Any) -> None:
        await self._set(True)

    async def async_turn_off(self, **kwargs: Any) -> None:
        await self._set(False)


class _InstallFlagSwitch(FreedoEntity, SwitchEntity):
    """A flag of the installation options (GET and PATCH /api/install). The
    daemon answers a write with the whole view. A refused write, such as the
    default curve without a flow limit, comes back as the daemon error."""

    _attr_entity_category = EntityCategory.CONFIG
    field: str

    def __init__(self, coordinator: FreedoCoordinator) -> None:
        super().__init__(coordinator, self.field)
        self._attr_device_info = self.hub_device()

    @property
    def available(self) -> bool:
        return super().available and self.field in self.coordinator.data.settings

    @property
    def is_on(self) -> bool | None:
        value = self.coordinator.data.settings.get(self.field)
        return value if isinstance(value, bool) else None

    async def _set(self, enabled: bool) -> None:
        view = await call_api(self.coordinator.client.patch_install(**{self.field: enabled}))
        if isinstance(view, Mapping):
            self.coordinator.data.settings.update(view)
        self.async_write_ha_state()
        # The flow limit view of the boiler follows the flag.
        await self.coordinator.async_request_refresh()

    async def async_turn_on(self, **kwargs: Any) -> None:
        await self._set(True)

    async def async_turn_off(self, **kwargs: Any) -> None:
        await self._set(False)


class FlowDefaultCurveSwitch(_InstallFlagSwitch):
    """The circuits without a weather curve of their own follow the default
    curve under the manual flow limit. The daemon needs flow_limit_c for it."""

    _attr_translation_key = "flow_default_curve"
    field = "flow_default_curve"


class FlowAutoAdaptSwitch(_InstallFlagSwitch):
    """The daemon trims the flow limit of each circuit from the room feedback,
    below the manual limit or the curve."""

    _attr_translation_key = "flow_auto_adapt"
    field = "flow_auto_adapt"


class PairingModeSwitch(FreedoEntity, SwitchEntity):
    """The pair window of the bridge: on, the daemon answers the beacon requests
    of a device that joins. It opens the longest window the daemon allows and
    closes by itself at the end, which the state shows at the next poll."""

    _attr_translation_key = "pairing_mode"
    _attr_entity_category = EntityCategory.CONFIG

    def __init__(self, coordinator: FreedoCoordinator) -> None:
        super().__init__(coordinator, "pairing_mode")
        self._attr_device_info = self.hub_device()

    @property
    def available(self) -> bool:
        return super().available and self.coordinator.data.pair_window is not None

    @property
    def is_on(self) -> bool | None:
        window = self.coordinator.data.pair_window
        return bool(window["open"]) if window and "open" in window else None

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        window = self.coordinator.data.pair_window or {}
        return {"until": window.get("until") if window.get("open") else None}

    async def _set(self, seconds: int) -> None:
        view = await call_api(self.coordinator.client.set_pair_window(seconds))
        if isinstance(view, dict):
            self.coordinator.data.pair_window = view
            self.async_write_ha_state()

    async def async_turn_on(self, **kwargs: Any) -> None:
        await self._set(PAIR_WINDOW_MAX_SEC)

    async def async_turn_off(self, **kwargs: Any) -> None:
        await self._set(0)


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


class ChildLockSwitch(FreedoNodeEntity, SwitchEntity):
    """GET /api/nodes ChildLock, written with POST /api/nodes/{id}/childlock.
    The valve takes the write at its next wake-up and the daemon reports the
    new state once the valve confirms it."""

    _attr_translation_key = "child_lock"
    _attr_entity_category = EntityCategory.CONFIG

    def __init__(self, coordinator: FreedoCoordinator, short: str) -> None:
        super().__init__(coordinator, short, "child_lock")
        self._pending: tuple[bool, float] | None = None

    @callback
    def _handle_coordinator_update(self) -> None:
        if self._pending and (
            time.monotonic() > self._pending[1]
            or node_child_lock(self.coordinator.data, self._short) == self._pending[0]
        ):
            self._pending = None
        super()._handle_coordinator_update()

    @property
    def is_on(self) -> bool | None:
        if self._pending:
            return self._pending[0]
        return node_child_lock(self.coordinator.data, self._short)

    async def _set(self, on: bool) -> None:
        await call_api(self.coordinator.client.set_child_lock(self._short, on))
        self._pending = (on, time.monotonic() + PENDING_SEC)
        self.async_write_ha_state()
        await self.coordinator.async_request_refresh()

    async def async_turn_on(self, **kwargs: Any) -> None:
        await self._set(True)

    async def async_turn_off(self, **kwargs: Any) -> None:
        await self._set(False)


class _ZoneFlagSwitch(FreedoZoneSettingEntity, SwitchEntity):
    _attr_entity_category = EntityCategory.CONFIG
    field: str

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, zone, self.field)

    @property
    def is_on(self) -> bool | None:
        value = (self._setting or {}).get(self.field)
        return value if isinstance(value, bool) else None

    async def async_turn_on(self, **kwargs: Any) -> None:
        await self._write(**{self.field: True})

    async def async_turn_off(self, **kwargs: Any) -> None:
        await self._write(**{self.field: False})


class EarlyStartSwitch(_ZoneFlagSwitch):
    """The daemon starts a warmer program block early, as the heat-up model of
    the zone says."""

    _attr_translation_key = "early_start"
    field = "early_start"


class OpenWindowDetectionSwitch(_ZoneFlagSwitch):
    """The valves of the zone detect an open window (tag 60e0)."""

    _attr_translation_key = "open_window_detection"
    field = "open_window_detection"


class AutoOffsetSwitch(_ZoneFlagSwitch):
    """The daemon calibrates the correction of the zone from an external room
    temperature, at most once per auto_offset_interval_sec. It applies in mode
    config and dbg2, where the correction is tag 0140 on the valve: in mode
    config each write homes the motor. The daemon refuses it on the hot water
    zone and in mode setpoint. The attributes are the autoOffset view of
    GET /api/zones."""

    _attr_translation_key = "auto_offset"
    # Select Room temperature correction replaces it for the user. It stays for
    # the automations that use it.
    _attr_entity_category = EntityCategory.DIAGNOSTIC
    _attr_entity_registry_enabled_default = False
    field = "auto_offset"

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        view = self.coordinator.data.auto_offsets.get(self._number) or {}
        return {
            "last_write_at": view.get("lastWriteAt"),
            "next_at": view.get("nextAt"),
            "filtered_diff_c": view.get("filteredDiffC"),
            "sample_span_sec": view.get("sampleSpanSec"),
            "stop_reason": view.get("stopReason"),
        }
