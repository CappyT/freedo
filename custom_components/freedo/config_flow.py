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
from homeassistant.data_entry_flow import section
from homeassistant.helpers import area_registry as ar, selector
from homeassistant.helpers.aiohttp_client import async_get_clientsession
from homeassistant.helpers.service_info.hassio import HassioServiceInfo
from homeassistant.helpers.translation import async_get_translations

from .api import FreedoAuthError, FreedoClient, FreedoConnectionError, FreedoError
from .const import (
    CONF_OUTDOOR_ENTITY,
    CONF_ROOM_TEMP_PAUSED,
    CONF_ROOM_TEMPS,
    CONF_TOKEN,
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
    window_zones,
    zone_auto_offset,
    zone_correction,
    zone_offset_mode,
)
from .rooms import (
    SOURCE_SENSOR,
    async_set_source,
    async_set_zone_area,
    current_source,
    opening_sensors,
    source_choices,
    zone_area,
)

_LOGGER = logging.getLogger(__name__)

# The field of the step Rooms, and the fields of the page of a room.
CONF_ROOM = "room"
CONF_AREA = "area"
CONF_SOURCE = "temperature_source"
CONF_ADVANCED = "advanced"
CONF_CORRECTION = "correction"
# The choice of the step Rooms that ends the options.
ROOM_DONE = "done"

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
        # The beta add-on has the host of the stable one plus "-beta". Both
        # serve the same plant, so they share the id and the entry follows the
        # add-on that started last.
        await self.async_set_unique_id(
            f"{self._discovered[CONF_HOST].removesuffix('-beta')}:{self._discovered[CONF_PORT]}"
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
    """A menu of two branches. Rooms lists the heating zones, each with what it
    uses, and opens the page of one: area, temperature source, window sensors
    and correction. A page writes its room on submit, with no reload, and
    returns to the list. Outdoor maps the outdoor temperature sensor and
    reloads the entry."""

    def __init__(self) -> None:
        self._room = 0
        # The words of the room labels: they are built at run time.
        self._words: dict[str, str] = {}

    async def async_step_init(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        return self.async_show_menu(step_id="init", menu_options=["rooms", "outdoor"])

    async def async_step_outdoor(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        options = self.config_entry.options
        if user_input is not None:
            saved = {k: v for k, v in options.items() if k != CONF_OUTDOOR_ENTITY}
            if user_input.get(CONF_OUTDOOR_ENTITY):
                saved[CONF_OUTDOOR_ENTITY] = user_input[CONF_OUTDOOR_ENTITY]
            return self.async_create_entry(data=saved)
        schema = vol.Schema(
            {
                vol.Optional(CONF_OUTDOOR_ENTITY): selector.EntitySelector(
                    selector.EntitySelectorConfig(domain="sensor")
                )
            }
        )
        return self.async_show_form(
            step_id="outdoor",
            data_schema=self.add_suggested_values_to_schema(
                schema, {CONF_OUTDOOR_ENTITY: options.get(CONF_OUTDOOR_ENTITY)}
            ),
        )

    def _loaded_data(self) -> FreedoData | None:
        runtime = getattr(self.config_entry, "runtime_data", None)
        return runtime.coordinator.data if runtime is not None else None

    def _word(self, key: str) -> str:
        return self._words.get(f"component.{DOMAIN}.selector.room_text.options.{key}", key)

    async def async_step_rooms(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        """The heating rooms, each with its area, its temperature source and
        the number of its window sensors. The page of the chosen room follows."""
        data = self._loaded_data()
        if data is None:
            return self.async_abort(reason="not_loaded")
        zones = data.heating_zones()
        if not zones:
            return self.async_abort(reason="no_heating_zone")
        if user_input is not None:
            if user_input[CONF_ROOM] == ROOM_DONE:
                # The pages saved the options already, so nothing reloads.
                return self.async_create_entry(data=dict(self.config_entry.options))
            self._room = int(user_input[CONF_ROOM])
            return await self.async_step_room()
        self._words = await async_get_translations(
            self.hass, self.hass.config.language, "selector", [DOMAIN]
        )
        options = [
            selector.SelectOptionDict(value=str(z.number), label=self._room_label(data, z))
            for z in zones
        ]
        options.append(selector.SelectOptionDict(value=ROOM_DONE, label=self._word("done")))
        schema = vol.Schema(
            {
                vol.Required(CONF_ROOM, default=ROOM_DONE): selector.SelectSelector(
                    selector.SelectSelectorConfig(
                        options=options, mode=selector.SelectSelectorMode.LIST
                    )
                )
            }
        )
        return self.async_show_form(step_id="rooms", data_schema=schema)

    def _room_label(self, data: FreedoData, zone: Zone) -> str:
        """The room name, its area, its temperature source and the number of
        its window sensors."""
        entry = self.config_entry
        area_id = zone_area(self.hass, entry.entry_id, zone.number)
        area = ar.async_get(self.hass).async_get_area(area_id) if area_id else None
        parts = [zone.name, area.name if area else self._word("no_area")]
        current = current_source(entry, data, zone)
        parts.extend(
            label
            for kind, target, label in source_choices(self.hass, entry, data, zone, area_id)
            if (kind, target) == current
        )
        windows = window_zones(entry.options.get(CONF_WINDOWS) or {}).get(zone.number, [])
        parts.append(f"{self._word('window_sensors')}: {len(windows)}")
        return " · ".join(parts)

    async def async_step_room(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        """The page of one room. It writes the room on submit and returns to
        the list."""
        data = self._loaded_data()
        zone = data.zones.get(self._room) if data is not None else None
        if zone is None:
            return self.async_abort(reason="not_loaded")
        errors: dict[str, str] = {}
        if user_input is not None:
            if await self._apply_room(data, zone, user_input):
                # A poll shows the new source in the list, sends the new area
                # to the daemon and updates the repair issue now.
                await self.config_entry.runtime_data.coordinator.async_refresh()
                return await self.async_step_rooms()
            errors["base"] = "write_failed"
        return self._room_form(data, zone, errors)

    def _room_form(
        self, data: FreedoData, zone: Zone, errors: dict[str, str]
    ) -> ConfigFlowResult:
        entry = self.config_entry
        area_id = zone_area(self.hass, entry.entry_id, zone.number)
        if area_id is None:
            # A zone with no area starts from the area of the same name.
            match = ar.async_get(self.hass).async_get_area_by_name(zone.name)
            area_id = match.id if match else None
        fields: dict[Any, Any] = {vol.Optional(CONF_AREA): selector.AreaSelector()}
        sources = [
            selector.SelectOptionDict(value=f"{kind}:{target}", label=label)
            for kind, target, label in source_choices(self.hass, entry, data, zone, area_id)
        ]
        if sources:
            current = current_source(entry, data, zone)
            key = (
                vol.Required(CONF_SOURCE, default=":".join(current))
                if current is not None
                else vol.Optional(CONF_SOURCE)
            )
            fields[key] = selector.SelectSelector(
                selector.SelectSelectorConfig(
                    options=sources, mode=selector.SelectSelectorMode.DROPDOWN
                )
            )
        fields[vol.Optional(CONF_WINDOWS)] = self._window_selector(data, zone, area_id)
        method = zone_correction(data, zone)
        if method is not None:
            fields[vol.Required(CONF_ADVANCED)] = section(
                vol.Schema(
                    {
                        vol.Required(CONF_CORRECTION, default=method): selector.SelectSelector(
                            selector.SelectSelectorConfig(
                                options=list(CORRECTIONS),
                                translation_key="correction_method",
                                mode=selector.SelectSelectorMode.DROPDOWN,
                            )
                        )
                    }
                ),
                {"collapsed": True},
            )
        windows = window_zones(entry.options.get(CONF_WINDOWS) or {}).get(zone.number, [])
        return self.async_show_form(
            step_id="room",
            data_schema=self.add_suggested_values_to_schema(
                vol.Schema(fields), {CONF_AREA: area_id, CONF_WINDOWS: windows}
            ),
            errors=errors,
            description_placeholders={"room": zone.name},
        )

    def _window_selector(self, data: FreedoData, zone: Zone, area_id: str | None) -> Any:
        """The window and door sensors, the ones in area_id first. A sensor of
        another room names that room: an entity selector cannot label an
        option, so the list is a select of entity ids. A mapped sensor without
        such a device class stays a choice. With no sensor at all it is the
        entity selector."""
        areas = ar.async_get(self.hass)
        mapped = {
            entity_id: number
            for number, entities in window_zones(
                self.config_entry.options.get(CONF_WINDOWS) or {}
            ).items()
            for entity_id in entities
        }

        def option(entity_id: str, label: str) -> selector.SelectOptionDict:
            other = data.zones.get(mapped.get(entity_id, zone.number))
            if other is not None and other.number != zone.number:
                label = f"{label} ({self._word('used_by')} {other.name})"
            return selector.SelectOptionDict(value=entity_id, label=label)

        near: list[selector.SelectOptionDict] = []
        far: list[selector.SelectOptionDict] = []
        known = set()
        for state, sensor_area in opening_sensors(self.hass):
            known.add(state.entity_id)
            if area_id is not None and sensor_area == area_id:
                near.append(option(state.entity_id, state.name))
                continue
            area = areas.async_get_area(sensor_area) if sensor_area else None
            far.append(
                option(state.entity_id, f"{state.name}, {area.name}" if area else state.name)
            )
        for entity_id in sorted(mapped.keys() - known):
            state = self.hass.states.get(entity_id)
            near.append(option(entity_id, state.name if state else entity_id))
        if not near and not far:
            return selector.EntitySelector(
                selector.EntitySelectorConfig(domain="binary_sensor", multiple=True)
            )
        return selector.SelectSelector(
            selector.SelectSelectorConfig(
                options=near + far, multiple=True, mode=selector.SelectSelectorMode.DROPDOWN
            )
        )

    async def _apply_room(
        self, data: FreedoData, zone: Zone, user_input: dict[str, Any]
    ) -> bool:
        """Write what the page changes in one zone: the correction, the
        temperature source, the area and the window sensors. False when the
        daemon refuses the correction or the source."""
        entry = self.config_entry
        runtime = entry.runtime_data
        kind, _, target = (user_input.get(CONF_SOURCE) or "").partition(":")
        if (kind, target) == current_source(entry, data, zone) and (
            kind == SOURCE_SENSOR
            or zone.number in room_temp_paused(entry.options.get(CONF_ROOM_TEMP_PAUSED) or [])
            or zone.number not in room_temp_zones(entry.options.get(CONF_ROOM_TEMPS) or {})
        ):
            # The page shows the source in use. A mapped sensor that is not in
            # use, because the room has no correction, still sends readings:
            # the device choice pauses it, so a later correction does not
            # bring it back unseen.
            kind = ""
        method = (user_input.get(CONF_ADVANCED) or {}).get(CONF_CORRECTION)
        try:
            if method is not None:
                # A new sensor needs a correction to change the regulation, so
                # it starts the continuous shift where none was set.
                if (
                    kind == SOURCE_SENSOR
                    and method == CORRECTION_NONE
                    and zone_correction(data, zone) == CORRECTION_NONE
                ):
                    method = CORRECTION_SHIFT
                fields = correction_patch(
                    zone_offset_mode(data, zone), zone_auto_offset(data, zone), method
                )
                if fields:
                    result = await runtime.client.patch_install_zone(zone.number, **fields)
                    row = result.get("zone") if isinstance(result, Mapping) else None
                    if isinstance(row, Mapping):
                        data.zone_settings[zone.number] = dict(row)
            if kind:
                await async_set_source(self.hass, entry, zone.number, kind, target)
        except FreedoError as err:
            _LOGGER.warning("Change of zone %s refused: %s", zone.number, err)
            return False
        async_set_zone_area(
            self.hass, entry.entry_id, data, zone.number, user_input.get(CONF_AREA) or None
        )
        chosen = list(user_input.get(CONF_WINDOWS) or [])
        windows = dict(entry.options.get(CONF_WINDOWS) or {})
        own = window_zones(windows).get(zone.number, [])
        if sorted(chosen) != own:
            # A sensor of another room moves to this one.
            kept = {e: z for e, z in windows.items() if e not in own and e not in chosen}
            kept.update(dict.fromkeys(chosen, zone.number))
            self.hass.config_entries.async_update_entry(
                entry, options={**entry.options, CONF_WINDOWS: kept}
            )
            runtime.windows.async_remap()
        return True
