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

from .api import FreedoAuthError, FreedoClient, FreedoConnectionError, FreedoError
from .const import (
    CONF_OUTDOOR_ENTITY,
    CONF_TOKEN,
    CONF_WINDOW_ENTITIES,
    CONF_WINDOWS,
    DEFAULT_PORT,
    DOMAIN,
)

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

    async def async_step_init(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        if user_input is not None:
            self._outdoor = user_input.get(CONF_OUTDOOR_ENTITY) or None
            self._window_entities = list(user_input.get(CONF_WINDOW_ENTITIES) or [])
            if self._window_entities:
                return await self.async_step_windows()
            return self._save({})
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
            return self._save({e: int(user_input[e]) for e in self._window_entities})
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

    def _save(self, windows: dict[str, int]) -> ConfigFlowResult:
        data: dict[str, Any] = {CONF_WINDOWS: windows}
        if self._outdoor:
            data[CONF_OUTDOOR_ENTITY] = self._outdoor
        return self.async_create_entry(data=data)
