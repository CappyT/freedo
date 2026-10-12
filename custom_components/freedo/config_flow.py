"""Config flow and options flow of the freedo integration."""

from __future__ import annotations

from collections.abc import Mapping
import logging
from typing import Any

import voluptuous as vol

from homeassistant.config_entries import (
    ConfigEntry,
    ConfigFlow,
    ConfigFlowResult,
    OptionsFlowWithReload,
)
from homeassistant.const import CONF_HOST, CONF_PORT
from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers import area_registry as ar, selector
from homeassistant.helpers.aiohttp_client import async_get_clientsession
from homeassistant.helpers.service_info.hassio import HassioServiceInfo

from .api import FreedoAuthError, FreedoClient, FreedoConnectionError, FreedoError
from .const import (
    CONF_OUTDOOR_ENTITY,
    CONF_ROOM_TEMP_PAUSED,
    CONF_ROOM_TEMPS,
    CONF_TOKEN,
    CONF_WINDOW_ENTITIES,
    CONF_WINDOWS,
    DEFAULT_PORT,
    DOMAIN,
)
from .model import (
    CORRECTION_NONE,
    CORRECTION_SHIFT,
    CORRECTIONS,
    FreedoData,
    Zone,
    correction_patch,
    room_temp_paused,
    room_temp_zones,
    zone_auto_offset,
    zone_correction,
    zone_offset_mode,
)
from .rooms import async_set_zone_area, sensor_label, temperature_sensors, zone_area

_LOGGER = logging.getLogger(__name__)

# The fields of a page of the step Rooms.
CONF_AREA = "area"
CONF_ROOM_SENSOR = "room_temperature"
CONF_CORRECTION = "correction"

_TOKEN_SELECTOR = selector.TextSelector(
    selector.TextSelectorConfig(type=selector.TextSelectorType.PASSWORD)
)


async def _validate(hass: HomeAssistant, host: str, port: int, token: str) -> str | None:
    """Return an error key, or None when the daemon accepts the token."""
    client = FreedoClient(async_get_clientsession(hass), host, port, token)
    try:
        await client.home()
    except FreedoAuthError:
        return "invalid_auth"
    except FreedoConnectionError:
        return "cannot_connect"
    except FreedoError:
        _LOGGER.exception("Unexpected answer from freedo at %s:%s", host, port)
        return "unknown"
    return None


class FreedoConfigFlow(ConfigFlow, domain=DOMAIN):
    VERSION = 1

    _discovered: dict[str, Any]

    async def async_step_hassio(
        self, discovery_info: HassioServiceInfo
    ) -> ConfigFlowResult:
        """The freedo add-on announces its host, port and token."""
        config = discovery_info.config
        self._discovered = {
            CONF_HOST: config[CONF_HOST],
            CONF_PORT: int(config[CONF_PORT]),
            CONF_TOKEN: config.get(CONF_TOKEN, ""),
        }
        await self.async_set_unique_id(
            f"{self._discovered[CONF_HOST]}:{self._discovered[CONF_PORT]}"
        )
        self._abort_if_unique_id_configured(updates=self._discovered)
        return await self.async_step_hassio_confirm()

    async def async_step_hassio_confirm(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        errors: dict[str, str] = {}
        if user_input is not None:
            error = await _validate(
                self.hass,
                self._discovered[CONF_HOST],
                self._discovered[CONF_PORT],
                self._discovered[CONF_TOKEN],
            )
            if error is None:
                return self.async_create_entry(title="freedo", data=self._discovered)
            errors["base"] = error
        return self.async_show_form(step_id="hassio_confirm", errors=errors)

    async def async_step_user(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        errors: dict[str, str] = {}
        if user_input is not None:
            host = user_input[CONF_HOST].strip()
            port = int(user_input[CONF_PORT])
            token = user_input.get(CONF_TOKEN, "")
            await self.async_set_unique_id(f"{host}:{port}")
            self._abort_if_unique_id_configured()
            error = await _validate(self.hass, host, port, token)
            if error is None:
                return self.async_create_entry(
                    title=f"freedo {host}",
                    data={CONF_HOST: host, CONF_PORT: port, CONF_TOKEN: token},
                )
            errors["base"] = error
        schema = vol.Schema(
            {
                vol.Required(CONF_HOST): str,
                vol.Required(CONF_PORT, default=DEFAULT_PORT): vol.All(
                    vol.Coerce(int), vol.Range(min=1, max=65535)
                ),
                vol.Optional(CONF_TOKEN, default=""): _TOKEN_SELECTOR,
            }
        )
        return self.async_show_form(
            step_id="user",
            data_schema=self.add_suggested_values_to_schema(schema, user_input),
            errors=errors,
        )

    async def async_step_reauth(
        self, entry_data: Mapping[str, Any]
    ) -> ConfigFlowResult:
        return await self.async_step_reauth_confirm()

    async def async_step_reauth_confirm(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        errors: dict[str, str] = {}
        entry = self._get_reauth_entry()
        if user_input is not None:
            token = user_input.get(CONF_TOKEN, "")
            error = await _validate(
                self.hass, entry.data[CONF_HOST], entry.data[CONF_PORT], token
            )
            if error is None:
                return self.async_update_reload_and_abort(
                    entry, data_updates={CONF_TOKEN: token}
                )
            errors["base"] = error
        return self.async_show_form(
            step_id="reauth_confirm",
            data_schema=vol.Schema({vol.Optional(CONF_TOKEN, default=""): _TOKEN_SELECTOR}),
            errors=errors,
        )

    @staticmethod
    @callback
    def async_get_options_flow(config_entry: ConfigEntry) -> FreedoOptionsFlow:
        return FreedoOptionsFlow()


class FreedoOptionsFlow(OptionsFlowWithReload):
    """Maps Home Assistant entities to the daemon's environment inputs, and
    each heating zone to its room: area, temperature sensor and correction."""

    def __init__(self) -> None:
        self._outdoor: str | None = None
        self._window_entities: list[str] = []
        self._windows: dict[str, int] = {}
        # The heating zones of the step Rooms in page order, the page shown,
        # and the sensor of each zone so far.
        self._room_queue: list[int] | None = None
        self._room_index = 0
        self._room_temps: dict[int, str] = {}

    async def async_step_init(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        if user_input is not None:
            self._outdoor = user_input.get(CONF_OUTDOOR_ENTITY) or None
            self._window_entities = list(user_input.get(CONF_WINDOW_ENTITIES) or [])
            if self._window_entities:
                return await self.async_step_windows()
            return await self.async_step_rooms()
        options = self.config_entry.options
        schema = vol.Schema(
            {
                vol.Optional(CONF_OUTDOOR_ENTITY): selector.EntitySelector(
                    selector.EntitySelectorConfig(domain="sensor")
                ),
                vol.Optional(CONF_WINDOW_ENTITIES): selector.EntitySelector(
                    selector.EntitySelectorConfig(domain="binary_sensor", multiple=True)
                ),
            }
        )
        suggested = {
            CONF_OUTDOOR_ENTITY: options.get(CONF_OUTDOOR_ENTITY),
            CONF_WINDOW_ENTITIES: list((options.get(CONF_WINDOWS) or {}).keys()),
        }
        return self.async_show_form(
            step_id="init",
            data_schema=self.add_suggested_values_to_schema(schema, suggested),
        )

    async def async_step_windows(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        """One zone per window sensor. The field keys are the entity ids."""
        runtime = getattr(self.config_entry, "runtime_data", None)
        if runtime is None or runtime.coordinator.data is None:
            return self.async_abort(reason="not_loaded")
        zones = runtime.coordinator.data.heating_zones()
        if not zones:
            return self.async_abort(reason="no_heating_zone")
        if user_input is not None:
            self._windows = {e: int(user_input[e]) for e in self._window_entities}
            return await self.async_step_rooms()
        zone_selector = selector.SelectSelector(
            selector.SelectSelectorConfig(
                options=[
                    selector.SelectOptionDict(value=str(z.number), label=f"{z.number}: {z.name}")
                    for z in zones
                ],
                mode=selector.SelectSelectorMode.DROPDOWN,
            )
        )
        current = self.config_entry.options.get(CONF_WINDOWS) or {}
        valid = {str(z.number) for z in zones}
        fields: dict[Any, Any] = {}
        for entity_id in self._window_entities:
            default = str(current.get(entity_id, ""))
            key = (
                vol.Required(entity_id, default=default)
                if default in valid
                else vol.Required(entity_id)
            )
            fields[key] = zone_selector
        return self.async_show_form(step_id="windows", data_schema=vol.Schema(fields))

    async def async_step_rooms(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        """One page per heating zone: its area, an optional room temperature
        sensor and the correction method. The zones without an area come first.
        Each page writes the area and the correction on submit; the last page
        saves the sensors. Without the zones (the entry is not loaded) the step
        keeps the saved sensors."""
        runtime = getattr(self.config_entry, "runtime_data", None)
        data = runtime.coordinator.data if runtime is not None else None
        zones = data.heating_zones() if data is not None else []
        current = room_temp_zones(self.config_entry.options.get(CONF_ROOM_TEMPS) or {})
        if not zones:
            return self._save({str(z): e for z, e in current.items()})
        entry_id = self.config_entry.entry_id
        if self._room_queue is None:
            self._room_queue = [
                z.number
                for z in sorted(
                    zones,
                    key=lambda z: (zone_area(self.hass, entry_id, z.number) is not None, z.number),
                )
            ]
            self._room_temps = dict(current)
        zone = data.zones[self._room_queue[self._room_index]]
        errors: dict[str, str] = {}
        if user_input is not None:
            if await self._apply_room(data, zone, current, user_input):
                self._room_index += 1
                if self._room_index < len(self._room_queue):
                    return await self.async_step_rooms()
                known = {z.number for z in zones}
                await self._release(
                    {z for z in current if z in known and z not in self._room_temps}
                )
                changed = {z for z, e in self._room_temps.items() if current.get(z) != e}
                return self._save(
                    {str(z): e for z, e in sorted(self._room_temps.items())}, changed
                )
            errors["base"] = "correction_failed"
        return self._room_form(data, zone, errors)

    def _room_form(
        self, data: FreedoData, zone: Zone, errors: dict[str, str]
    ) -> ConfigFlowResult:
        area_id = zone_area(self.hass, self.config_entry.entry_id, zone.number)
        if area_id is None:
            # A zone with no area starts from the area of the same name.
            match = ar.async_get(self.hass).async_get_area_by_name(zone.name)
            area_id = match.id if match else None
        mapped = self._room_temps.get(zone.number)
        fields: dict[Any, Any] = {
            vol.Optional(CONF_AREA): selector.AreaSelector(),
            vol.Optional(CONF_ROOM_SENSOR): self._sensor_selector(area_id, mapped),
        }
        method = zone_correction(data, zone)
        if method is not None:
            fields[vol.Required(CONF_CORRECTION, default=method)] = selector.SelectSelector(
                selector.SelectSelectorConfig(
                    options=list(CORRECTIONS),
                    translation_key="correction_method",
                    mode=selector.SelectSelectorMode.DROPDOWN,
                )
            )
        return self.async_show_form(
            step_id="rooms",
            data_schema=self.add_suggested_values_to_schema(
                vol.Schema(fields), {CONF_AREA: area_id, CONF_ROOM_SENSOR: mapped}
            ),
            errors=errors,
            description_placeholders={
                "zone": zone.name,
                "index": str(self._room_index + 1),
                "count": str(len(self._room_queue or [])),
            },
        )

    def _sensor_selector(self, area_id: str | None, mapped: str | None) -> Any:
        """The temperature sensors, the ones in area_id first. An entity selector
        cannot order by area, so the list is a select of entity ids. With no
        temperature sensor at all it is the entity selector."""
        areas = ar.async_get(self.hass)
        sensors = temperature_sensors(self.hass)
        if not sensors and mapped is None:
            return selector.EntitySelector(
                selector.EntitySelectorConfig(domain="sensor", device_class="temperature")
            )
        first: list[selector.SelectOptionDict] = []
        rest: list[selector.SelectOptionDict] = []
        for state, sensor_area in sensors:
            label = sensor_label(state)
            area = areas.async_get_area(sensor_area) if sensor_area else None
            if sensor_area is not None and sensor_area == area_id:
                first.append(selector.SelectOptionDict(value=state.entity_id, label=label))
            else:
                if area is not None:
                    label = f"{label}, {area.name}"
                rest.append(selector.SelectOptionDict(value=state.entity_id, label=label))
        options = first + rest
        if mapped is not None and all(o["value"] != mapped for o in options):
            # A sensor mapped before the list knew it, such as one without a
            # device class, stays a choice.
            state = self.hass.states.get(mapped)
            options.insert(
                0, selector.SelectOptionDict(value=mapped, label=state.name if state else mapped)
            )
        return selector.SelectSelector(
            selector.SelectSelectorConfig(options=options, mode=selector.SelectSelectorMode.DROPDOWN)
        )

    async def _apply_room(
        self,
        data: FreedoData,
        zone: Zone,
        current: dict[int, str],
        user_input: dict[str, Any],
    ) -> bool:
        """Write the correction and the area of one zone, and keep its sensor
        for the save. False when the daemon refuses the correction."""
        sensor = user_input.get(CONF_ROOM_SENSOR) or None
        method = user_input.get(CONF_CORRECTION)
        if method is not None:
            # A new sensor needs a correction to change the regulation, so it
            # starts the continuous shift where none was set, as the select
            # Temperature source does.
            if (
                sensor is not None
                and sensor != current.get(zone.number)
                and method == CORRECTION_NONE
                and zone_correction(data, zone) == CORRECTION_NONE
            ):
                method = CORRECTION_SHIFT
            fields = correction_patch(
                zone_offset_mode(data, zone), zone_auto_offset(data, zone), method
            )
            if fields:
                client = self.config_entry.runtime_data.client
                try:
                    result = await client.patch_install_zone(zone.number, **fields)
                except FreedoError as err:
                    _LOGGER.warning("Correction of zone %s refused: %s", zone.number, err)
                    return False
                row = result.get("zone") if isinstance(result, Mapping) else None
                if isinstance(row, Mapping):
                    data.zone_settings[zone.number] = dict(row)
        async_set_zone_area(
            self.hass,
            self.config_entry.entry_id,
            data,
            zone.number,
            user_input.get(CONF_AREA) or None,
        )
        if sensor is None:
            self._room_temps.pop(zone.number, None)
        else:
            self._room_temps[zone.number] = sensor
        return True

    async def _release(self, zones: set[int]) -> None:
        """Clear the external temperature of the zones that lose their sensor.
        The daemon ends the correction by itself when the reading gets too old,
        but this call starts the release at once."""
        client = self.config_entry.runtime_data.client
        for zone in sorted(zones):
            try:
                await client.clear_room_temp(zone)
            except FreedoError as err:
                _LOGGER.warning("Room temperature release for zone %s failed: %s", zone, err)

    def _save(
        self, room_temps: dict[str, str], changed: set[int] | None = None
    ) -> ConfigFlowResult:
        """Save the options. A zone in changed got a new sensor, which ends
        its pause."""
        data: dict[str, Any] = {CONF_WINDOWS: self._windows}
        if self._outdoor:
            data[CONF_OUTDOOR_ENTITY] = self._outdoor
        if room_temps:
            data[CONF_ROOM_TEMPS] = room_temps
        # The step replaces the whole options mapping, so keep the pause of the
        # zones that still map the same sensor.
        paused = room_temp_paused(self.config_entry.options.get(CONF_ROOM_TEMP_PAUSED) or [])
        kept = sorted(
            zone for zone in paused if str(zone) in room_temps and zone not in (changed or ())
        )
        if kept:
            data[CONF_ROOM_TEMP_PAUSED] = kept
        return self.async_create_entry(data=data)
