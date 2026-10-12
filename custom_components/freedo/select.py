"""Display orientation of each valve (d/config tag 0149). End rule of a change
made on a device, room temperature correction and offset mode, of each heating
zone. Temperature source: a Home Assistant sensor or a device of the room that
measures the room temperature of a heating zone. Active program preset of the
daemon: the home and away program of every zone."""

from __future__ import annotations

from homeassistant.components.select import SelectEntity
from homeassistant.const import EntityCategory
from homeassistant.core import HomeAssistant, callback
from homeassistant.exceptions import ServiceValidationError
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .const import CONF_ROOM_TEMP_PAUSED, CONF_ROOM_TEMPS, DOMAIN
from .coordinator import FreedoConfigEntry, FreedoCoordinator
from .entity import FreedoDisplayEntity, FreedoEntity, FreedoZoneSettingEntity, call_api
from .model import (
    CORRECTION_NONE,
    CORRECTION_SHIFT,
    CORRECTIONS,
    KIND_VALVE,
    OFFSET_MODE_SETPOINT,
    OFFSET_MODES,
    ORIENTATIONS,
    OVERLAY_END_DEFAULT_SEC,
    OVERLAY_END_RULES,
    SOURCE_OTHER_SENSOR,
    Zone,
    correction_patch,
    device_option_label,
    in_zone,
    room_temp_paused,
    room_temp_zones,
    zone_auto_offset,
    zone_correction,
    zone_measuring_candidates,
    zone_offset_mode,
    zone_setting,
)
from .rooms import sensor_label, temperature_sensors, zone_area


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
    """Where the room temperature of a heating zone comes from. The options are,
    in order: the Home Assistant temperature sensors in the area of the zone
    device, labelled with their reading, and the mapped sensor when it lies
    elsewhere; the devices of the room; then other_sensor, which maps nothing
    and says where to map a sensor of another area.

    Choosing a sensor maps it to the zone in the options, resumes and sends its
    pushes, and sets the correction continuous shift when the zone has none.
    Choosing a device writes the measuring device of the zone (PUT
    /api/install/zones/{n}/measuring); when a sensor is mapped, it also pauses
    its pushes and clears the zone reading, so the daemon returns to the manual
    correction. The correction never changes on a device choice.

    A sensor or device option is a display string, and a sensor label carries
    its reading. So the options and their targets are built once per poll and
    kept: the service checks an option against the list the state shows, and
    the label maps back to its entity id or device. A label that repeats gets
    its entity id or short address."""

    _attr_translation_key = "temperature_source"
    _attr_entity_category = EntityCategory.CONFIG

    def __init__(self, coordinator: FreedoCoordinator, zone: Zone) -> None:
        super().__init__(coordinator, f"zone{zone.number}_temperature_source")
        self._number = zone.number
        self._attr_device_info = self.zone_device(zone)
        # Option text -> (kind, target): ("sensor", entity id), ("device",
        # short address) or ("other", "").
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

    def _build(self) -> dict[str, tuple[str, str]]:
        if self._zone is None:
            return {}
        choices: dict[str, tuple[str, str]] = {}

        def add(label: str, target: tuple[str, str]) -> None:
            if label in choices or label == SOURCE_OTHER_SENSOR:
                label = f"{label} [{target[1]}]"
            choices[label] = target

        area_id = zone_area(self.hass, self.coordinator.config_entry.entry_id, self._number)
        in_area = [
            state
            for state, sensor_area in temperature_sensors(self.hass)
            if area_id is not None and sensor_area == area_id
        ]
        mapped = self._mapped_sensor
        if mapped and all(state.entity_id != mapped for state in in_area):
            state = self.hass.states.get(mapped)
            add(sensor_label(state) if state else mapped, ("sensor", mapped))
        for state in in_area:
            add(sensor_label(state), ("sensor", state.entity_id))
        for row in self._candidates:
            add(device_option_label(row), ("device", str(row.get("short"))))
        choices[SOURCE_OTHER_SENSOR] = ("other", "")
        return choices

    def _option(self, kind: str, target: str) -> str | None:
        return next((o for o, t in self._choices.items() if t == (kind, target)), None)

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
        mapped = self._mapped_sensor
        if (
            mapped
            and not self._paused
            and (
                zone_offset_mode(self.coordinator.data, zone) == OFFSET_MODE_SETPOINT
                or zone_auto_offset(self.coordinator.data, zone)
            )
        ):
            return self._option("sensor", mapped)
        short = zone.measuring
        if not short:
            return None
        option = self._option("device", short)
        if option is not None:
            return option
        row = self.coordinator.data.install_devices.get(short)
        return device_option_label(row) if row else short

    async def async_select_option(self, option: str) -> None:
        kind, target = self._choices.get(option, ("", ""))
        if kind == "other":
            zone = self._zone
            raise ServiceValidationError(
                translation_domain=DOMAIN,
                translation_key="another_sensor",
                translation_placeholders={"room": zone.name if zone else str(self._number)},
            )
        if kind == "device":
            await self._select_device(target)
        elif kind == "sensor":
            await self._select_sensor(target)

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
                self._set_options(paused=True)
                await call_api(client.clear_room_temp(self._number))
        await self.coordinator.async_request_refresh()

    async def _select_sensor(self, entity_id: str) -> None:
        """Measure the room temperature on a Home Assistant sensor."""
        zone = self._zone
        if zone is None:
            return
        data = self.coordinator.data
        # The daemon refuses a reading (409) in a zone with no correction.
        if zone_correction(data, zone) == CORRECTION_NONE:
            fields = correction_patch(
                zone_offset_mode(data, zone), zone_auto_offset(data, zone), CORRECTION_SHIFT
            )
            await call_api(self.coordinator.client.patch_install_zone(self._number, **fields))
        self._set_options(paused=False, sensor=entity_id)
        room_temps = self.coordinator.config_entry.runtime_data.room_temps
        if room_temps is not None:
            room_temps.async_remap()
            await room_temps.push(self._number)
        await self.coordinator.async_request_refresh()

    def _set_options(self, paused: bool, sensor: str | None = None) -> None:
        """Persist the pause of the pushes of the zone, and a newly chosen
        sensor, in the options, so they survive a restart."""
        entry = self.coordinator.config_entry
        zones = room_temp_paused(entry.options.get(CONF_ROOM_TEMP_PAUSED) or [])
        if paused:
            zones.add(self._number)
        else:
            zones.discard(self._number)
        options = {**entry.options, CONF_ROOM_TEMP_PAUSED: sorted(zones)}
        if sensor is not None:
            options[CONF_ROOM_TEMPS] = {
                **(entry.options.get(CONF_ROOM_TEMPS) or {}),
                str(self._number): sensor,
            }
        self.hass.config_entries.async_update_entry(entry, options=options)
