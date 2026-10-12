"""Display orientation of each valve (d/config tag 0149). End rule of a change
made on a device and offset mode, of each heating zone. Temperature source: the
device of the room or an external Home Assistant sensor that measures the room
temperature of a heating zone."""

from __future__ import annotations

import logging

from homeassistant.components.select import SelectEntity
from homeassistant.const import ATTR_UNIT_OF_MEASUREMENT, EntityCategory
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .api import FreedoError
from .const import CONF_ROOM_TEMP_PAUSED, CONF_ROOM_TEMPS
from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FreedoDisplayEntity, FreedoEntity, FreedoZoneSettingEntity, call_api
from .model import (
    KIND_VALVE,
    OFFSET_MODE_SETPOINT,
    OFFSET_MODES,
    ORIENTATIONS,
    OVERLAY_END_DEFAULT_SEC,
    OVERLAY_END_RULES,
    ROOM_TEMP_EXTERNAL,
    Zone,
    device_option_label,
    in_zone,
    room_temp_payload,
    room_temp_paused,
    room_temp_zones,
    zone_auto_offset,
    zone_measuring_candidates,
    zone_offset_mode,
    zone_setting,
)

_LOGGER = logging.getLogger(__name__)


def _mapped_room_temp(entry: FreedoConfigEntry, zone: Zone) -> str | None:
    """The external room temperature sensor mapped to the zone, None without
    one."""
    return room_temp_zones(entry.options.get(CONF_ROOM_TEMPS) or {}).get(zone.number)


async def async_setup_entry(
    hass: HomeAssistant,
    entry: FreedoConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    coordinator = entry.runtime_data.coordinator
    entities: list[SelectEntity] = [
        OrientationSelect(coordinator, short)
        for short, dev in sorted(coordinator.data.devices.items())
        if dev.get("kind") == KIND_VALVE and in_zone(coordinator.data, short)
    ]
    entities.extend(
        DeviceOverlayEndSelect(coordinator, zone)
        for zone in coordinator.data.heating_zones()
        if zone_setting(coordinator.data, zone, "device_overlay_end") in OVERLAY_END_RULES
    )
    entities.extend(
        ZoneOffsetModeSelect(coordinator, zone)
        for zone in coordinator.data.heating_zones()
        if zone_setting(coordinator.data, zone, "offset_mode") in OFFSET_MODES
    )
    # The source exists while the zone has a device of its own room to measure
    # the temperature, or a mapped external sensor.
    entities.extend(
        ZoneTemperatureSourceSelect(coordinator, zone)
        for zone in coordinator.data.heating_zones()
        if zone_measuring_candidates(coordinator.data, zone)
        or _mapped_room_temp(entry, zone)
    )
    async_add_entities(entities)


class OrientationSelect(FreedoDisplayEntity, SelectEntity):
    _attr_translation_key = "orientation"
    _attr_entity_category = EntityCategory.CONFIG
    _attr_options = list(ORIENTATIONS)

    def __init__(self, coordinator: FreedoCoordinator, short: str) -> None:
        super().__init__(coordinator, short, "orientation")

    @property
    def current_option(self) -> str | None:
        value = (self._display or {}).get("orientation")
        return value if value in ORIENTATIONS else None

    async def async_select_option(self, option: str) -> None:
        await self._write(orientation=option)


class DeviceOverlayEndSelect(FreedoZoneSettingEntity, SelectEntity):
    """The end rule the daemon gives a change made on a device of the zone: the
    rule does not travel on the air. Rule timer starts at one hour, which the
    Device change timer number adjusts."""

    _attr_translation_key = "device_overlay_end"
    _attr_entity_category = EntityCategory.CONFIG
    _attr_options = list(OVERLAY_END_RULES)

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, zone, "device_overlay_end")

    @property
    def current_option(self) -> str | None:
        value = (self._setting or {}).get("device_overlay_end")
        return value if value in OVERLAY_END_RULES else None

    async def async_select_option(self, option: str) -> None:
        if option != "timer":
            await self._write(device_overlay_end=option)
            return
        seconds = (self._setting or {}).get("device_overlay_end_sec") or OVERLAY_END_DEFAULT_SEC
        await self._write(device_overlay_end=option, device_overlay_end_sec=seconds)


class ZoneOffsetModeSelect(FreedoZoneSettingEntity, SelectEntity):
    """How the temperature correction of the zone reaches it (offset_mode).
    config writes the whole d/config document, and a valve homes its motor, a
    full stroke, on each change of the offset. dbg2 writes one record and the
    valve does not home. setpoint shifts the setpoints in the daemon and writes
    nothing to the device. Leaving setpoint drops the correction of the daemon
    and rewrites the zone. The room temperature sensors of the options need
    setpoint. The change moves the temperatures of the zone, so it asks for a
    new poll."""

    _attr_translation_key = "offset_mode"
    _attr_entity_category = EntityCategory.CONFIG
    _attr_options = list(OFFSET_MODES)

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, zone, "offset_mode")

    @property
    def current_option(self) -> str | None:
        value = (self._setting or {}).get("offset_mode")
        return value if value in OFFSET_MODES else None

    async def async_select_option(self, option: str) -> None:
        await self._write(offset_mode=option)
        await self.coordinator.async_request_refresh()


class ZoneTemperatureSourceSelect(FreedoEntity, SelectEntity):
    """Where the room temperature of a heating zone comes from: one device of the
    room (its measuring device) or an external Home Assistant sensor.

    Choosing a device writes the measuring device of the zone (PUT
    /api/install/zones/{n}/measuring). When an external sensor is mapped to the
    zone, the choice also pauses its pushes and clears the zone reading, so the
    daemon returns to the manual correction. Choosing the external sensor resumes
    the pushes, sends the current reading at once and sets the offset mode
    setpoint if it is not, unless auto_offset is on: that mode takes the reading
    too, and the daemon refuses setpoint while the automatic correction runs. The
    offset mode never changes on a device choice.

    The option text of a device is its label and serial, which is dynamic, so
    Home Assistant does not translate it; two devices of one room can carry the
    same label, and the serial keeps the options unique. The external option is a
    fixed name and carries a translation."""

    _attr_translation_key = "temperature_source"
    _attr_entity_category = EntityCategory.CONFIG

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, f"zone{zone.number}_temperature_source")
        self._number = zone.number
        self._attr_device_info = self.zone_device(zone)

    @property
    def _zone(self) -> Zone | None:
        return self.coordinator.data.zones.get(self._number)

    @property
    def _candidates(self) -> list[dict]:
        zone = self._zone
        return zone_measuring_candidates(self.coordinator.data, zone) if zone else []

    @property
    def _mapped_sensor(self) -> str | None:
        return room_temp_zones(
            self.coordinator.config_entry.options.get(CONF_ROOM_TEMPS) or {}
        ).get(self._number)

    @property
    def _paused(self) -> bool:
        return self._number in room_temp_paused(
            self.coordinator.config_entry.options.get(CONF_ROOM_TEMP_PAUSED) or []
        )

    @property
    def options(self) -> list[str]:
        labels = [device_option_label(row) for row in self._candidates]
        if self._mapped_sensor:
            labels.append(ROOM_TEMP_EXTERNAL)
        return labels

    @property
    def available(self) -> bool:
        return super().available and self._zone is not None and bool(self.options)

    @property
    def current_option(self) -> str | None:
        zone = self._zone
        if zone is None:
            return None
        if (
            self._mapped_sensor
            and not self._paused
            and (
                zone_offset_mode(self.coordinator.data, zone) == OFFSET_MODE_SETPOINT
                or zone_auto_offset(self.coordinator.data, zone)
            )
        ):
            return ROOM_TEMP_EXTERNAL
        short = zone.measuring
        if not short:
            return None
        for row in self._candidates:
            if row.get("short") == short:
                return device_option_label(row)
        row = self.coordinator.data.install_devices.get(short)
        return device_option_label(row) if row else short

    async def async_select_option(self, option: str) -> None:
        if option == ROOM_TEMP_EXTERNAL:
            await self._select_external()
            return
        short = next(
            (
                row.get("short")
                for row in self._candidates
                if device_option_label(row) == option
            ),
            None,
        )
        if short:
            await self._select_device(short)

    async def _select_device(self, short: str) -> None:
        """Measure the room temperature on a device of the room."""
        client = self.coordinator.client
        await call_api(client.set_zone_measuring(self._number, short))
        if self._mapped_sensor:
            # Stop the external pushes and end the reading, so the daemon falls
            # back to the manual correction of the device. The lock keeps a push
            # that started earlier from landing after the clear.
            lock = self.coordinator.config_entry.runtime_data.room_temp_lock
            async with lock:
                self._set_paused(True)
                await call_api(client.clear_room_temp(self._number))
        await self.coordinator.async_request_refresh()

    async def _select_external(self) -> None:
        """Measure the room temperature on the mapped external sensor."""
        zone = self._zone
        if zone is None:
            return
        client = self.coordinator.client
        # The reading needs the offset mode setpoint first, or the daemon refuses
        # it with 409. Mode config and dbg2 take it too while auto_offset is on,
        # and the daemon refuses a switch to setpoint then.
        if (
            zone_offset_mode(self.coordinator.data, zone) != OFFSET_MODE_SETPOINT
            and not zone_auto_offset(self.coordinator.data, zone)
        ):
            await call_api(
                client.patch_install_zone(self._number, offset_mode=OFFSET_MODE_SETPOINT)
            )
        self._set_paused(False)
        await self._push_external()
        await self.coordinator.async_request_refresh()

    async def _push_external(self) -> None:
        """Send the current state of the mapped sensor at once, as the options
        step and the environment listener do."""
        entity_id = self._mapped_sensor
        if entity_id is None:
            return
        state = self.hass.states.get(entity_id)
        payload = room_temp_payload(
            state.state if state else None,
            state.attributes.get(ATTR_UNIT_OF_MEASUREMENT) if state else None,
        )
        if payload is None:
            _LOGGER.debug(
                "Temperature source of zone %s: %s has no reading", self._number, entity_id
            )
            return
        try:
            await self.coordinator.client.push_room_temp(self._number, payload["tempC"])
        except FreedoError as err:
            _LOGGER.warning("Room temperature push for zone %s failed: %s", self._number, err)

    def _set_paused(self, paused: bool) -> None:
        """Persist the pause of the external pushes of the zone in the options,
        so it survives a restart."""
        entry = self.coordinator.config_entry
        zones = room_temp_paused(entry.options.get(CONF_ROOM_TEMP_PAUSED) or [])
        if paused:
            zones.add(self._number)
        else:
            zones.discard(self._number)
        self.hass.config_entries.async_update_entry(
            entry,
            options={**entry.options, CONF_ROOM_TEMP_PAUSED: sorted(zones)},
        )
