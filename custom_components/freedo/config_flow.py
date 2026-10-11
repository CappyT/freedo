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
from homeassistant.helpers import selector
from homeassistant.helpers.aiohttp_client import async_get_clientsession
from homeassistant.helpers.service_info.hassio import HassioServiceInfo

from .api import FreedoAuthError, FreedoClient, FreedoConnectionError, FreedoError
from .const import (
    CONF_OUTDOOR_ENTITY,
    CONF_ROOM_TEMPS,
    CONF_TOKEN,
    CONF_WINDOW_ENTITIES,
    CONF_WINDOWS,
    DEFAULT_PORT,
    DOMAIN,
)
from .model import room_temp_zones

_LOGGER = logging.getLogger(__name__)

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
    """Maps Home Assistant entities to the daemon's environment inputs."""

    def __init__(self) -> None:
        self._outdoor: str | None = None
        self._window_entities: list[str] = []
        self._windows: dict[str, int] = {}

    async def async_step_init(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        if user_input is not None:
            self._outdoor = user_input.get(CONF_OUTDOOR_ENTITY) or None
            self._window_entities = list(user_input.get(CONF_WINDOW_ENTITIES) or [])
            if self._window_entities:
                return await self.async_step_windows()
            return await self.async_step_temperatures()
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
            return await self.async_step_temperatures()
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

    async def async_step_temperatures(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        """One optional temperature sensor per heating zone. The field keys are
        the zone labels. Without the zones (the entry is not loaded) the step
        keeps the saved sensors."""
        runtime = getattr(self.config_entry, "runtime_data", None)
        data = runtime.coordinator.data if runtime is not None else None
        zones = data.heating_zones() if data is not None else []
        current = room_temp_zones(self.config_entry.options.get(CONF_ROOM_TEMPS) or {})
        if not zones:
            return self._save({str(z): e for z, e in current.items()})
        labels = {f"{z.number}: {z.name}": z.number for z in zones}
        if user_input is not None:
            chosen = {
                number: user_input[label]
                for label, number in labels.items()
                if user_input.get(label)
            }
            await self._release(
                {z for z in current if z in labels.values() and z not in chosen}
            )
            return self._save({str(z): e for z, e in chosen.items()})
        schema = vol.Schema(
            {
                vol.Optional(label): selector.EntitySelector(
                    selector.EntitySelectorConfig(domain="sensor", device_class="temperature")
                )
                for label in labels
            }
        )
        suggested = {label: current.get(number) for label, number in labels.items()}
        return self.async_show_form(
            step_id="temperatures",
            data_schema=self.add_suggested_values_to_schema(schema, suggested),
        )

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

    def _save(self, room_temps: dict[str, str]) -> ConfigFlowResult:
        data: dict[str, Any] = {CONF_WINDOWS: self._windows}
        if self._outdoor:
            data[CONF_OUTDOOR_ENTITY] = self._outdoor
        if room_temps:
            data[CONF_ROOM_TEMPS] = room_temps
        return self.async_create_entry(data=data)
