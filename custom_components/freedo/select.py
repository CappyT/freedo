"""Display orientation of each valve (d/config tag 0149). End rule of a change
made on a device, room temperature correction and offset mode, of each heating
zone. Temperature source: a device of the room or a Home Assistant sensor that
measures the room temperature of a heating zone. Active program preset of the
daemon: the home and away program of every zone."""

from __future__ import annotations

from homeassistant.components.select import SelectEntity
from homeassistant.const import EntityCategory
from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FreedoDisplayEntity, FreedoEntity, FreedoZoneSettingEntity, call_api
from .model import (
    CORRECTIONS,
    KIND_VALVE,
    OFFSET_MODES,
    ORIENTATIONS,
    OVERLAY_END_DEFAULT_SEC,
    OVERLAY_END_RULES,
    Zone,
    correction_patch,
    in_zone,
    zone_auto_offset,
    zone_correction,
    zone_offset_mode,
    zone_setting,
)
from .rooms import async_set_source, current_source, source_choices, zone_area


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
    # The preset route is optional: a daemon without it gets no select.
    if coordinator.data.presets is not None:
        entities.append(PresetSelect(coordinator))
    entities.extend(
        DeviceOverlayEndSelect(coordinator, zone)
        for zone in coordinator.data.heating_zones()
        if zone_setting(coordinator.data, zone, "device_overlay_end") in OVERLAY_END_RULES
    )
    for zone in coordinator.data.heating_zones():
        if zone_setting(coordinator.data, zone, "offset_mode") in OFFSET_MODES:
            entities.append(ZoneOffsetModeSelect(coordinator, zone))
            entities.append(RoomTemperatureCorrectionSelect(coordinator, zone))
        entities.append(ZoneTemperatureSourceSelect(coordinator, zone))
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


class PresetSelect(FreedoEntity, SelectEntity):
    """The active program preset of the daemon: the home program and the
    optional away program of every zone. The daemon has no season calendar, so
    an automation follows the season or the weather here. Choosing an option
    activates that preset and asks for a new poll. The options come from the
    coordinator data on each update, so a rename, a new preset and a delete
    show up without a reload. The entity exists only on a daemon that serves
    GET /api/presets."""

    _attr_translation_key = "preset"

    def __init__(self, coordinator: FreedoCoordinator) -> None:
        super().__init__(coordinator, "preset")
        self._attr_device_info = self.hub_device()

    @property
    def _presets(self) -> list[dict]:
        return self.coordinator.data.presets or []

    @property
    def options(self) -> list[str]:
        return [p["name"] for p in self._presets if p.get("name")]

    @property
    def current_option(self) -> str | None:
        for preset in self._presets:
            if preset.get("active"):
                return preset.get("name")
        return None

    async def async_select_option(self, option: str) -> None:
        preset = next((p for p in self._presets if p.get("name") == option), None)
        if preset is None:
            return
        await call_api(self.coordinator.client.activate_preset(preset["id"]))
        await self.coordinator.async_request_refresh()


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
    # Select Room temperature correction replaces it for the user. It stays for
    # the automations that use it.
    _attr_entity_category = EntityCategory.DIAGNOSTIC
    _attr_entity_registry_enabled_default = False
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


class RoomTemperatureCorrectionSelect(FreedoZoneSettingEntity, SelectEntity):
    """How the zone corrects its regulation with the room temperature: none,
    a continuous shift of the setpoints (offset_mode setpoint), or a
    calibration of the valve offset (auto_offset on, offset_mode config). It
    replaces, for the user, the pair select Temperature offset mode and switch
    Automatic correction, which exclude each other. One PATCH carries both
    fields, which the daemon checks against the values the request leaves, so
    the switch never meets its refusal. The change moves the temperatures of
    the zone, so it asks for a new poll."""

    _attr_translation_key = "room_temperature_correction"
    _attr_entity_category = EntityCategory.CONFIG
    _attr_options = list(CORRECTIONS)

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, zone, "room_temperature_correction")

    @property
    def _zone(self) -> Zone | None:
        return self.coordinator.data.zones.get(self._number)

    @property
    def current_option(self) -> str | None:
        zone = self._zone
        return zone_correction(self.coordinator.data, zone) if zone else None

    async def async_select_option(self, option: str) -> None:
        zone = self._zone
        if zone is None:
            return
        data = self.coordinator.data
        fields = correction_patch(zone_offset_mode(data, zone), zone_auto_offset(data, zone), option)
        if fields:
            await self._write(**fields)
            await self.coordinator.async_request_refresh()


class ZoneTemperatureSourceSelect(FreedoEntity, SelectEntity):
    """Where the room temperature of a heating zone comes from: a device of the
    room or a Home Assistant temperature sensor. The options step of the room
    shows the same list and the same current source, and both write through
    async_set_source: see rooms.py.

    An option is a display string, and a sensor label carries its reading. So
    the options and their targets are built once per poll and kept: the service
    checks an option against the list the state shows, and the label maps back
    to its device or entity id. A label that repeats gets its short address or
    entity id."""

    _attr_translation_key = "temperature_source"
    _attr_entity_category = EntityCategory.CONFIG

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, f"zone{zone.number}_temperature_source")
        self._number = zone.number
        self._attr_device_info = self.zone_device(zone)
        # Option text -> (kind, target).
        self._choices: dict[str, tuple[str, str]] = {}

    async def async_added_to_hass(self) -> None:
        self._choices = self._build()
        await super().async_added_to_hass()

    @callback
    def _handle_coordinator_update(self) -> None:
        self._choices = self._build()
        super()._handle_coordinator_update()

    @property
    def _zone(self) -> Zone | None:
        return self.coordinator.data.zones.get(self._number)

    def _build(self) -> dict[str, tuple[str, str]]:
        zone = self._zone
        if zone is None:
            return {}
        entry = self.coordinator.config_entry
        choices: dict[str, tuple[str, str]] = {}
        for kind, target, label in source_choices(
            self.hass,
            entry,
            self.coordinator.data,
            zone,
            zone_area(self.hass, entry.entry_id, self._number),
        ):
            if label in choices:
                label = f"{label} [{target}]"
            choices[label] = (kind, target)
        return choices

    @property
    def options(self) -> list[str]:
        return list(self._choices)

    @property
    def available(self) -> bool:
        return super().available and self._zone is not None and bool(self._choices)

    @property
    def current_option(self) -> str | None:
        zone = self._zone
        if zone is None:
            return None
        current = current_source(self.coordinator.config_entry, self.coordinator.data, zone)
        return next((o for o, t in self._choices.items() if t == current), None)

    async def async_select_option(self, option: str) -> None:
        kind, target = self._choices[option]
        await call_api(
            async_set_source(
                self.hass, self.coordinator.config_entry, self._number, kind, target
            )
        )
        await self.coordinator.async_request_refresh()
