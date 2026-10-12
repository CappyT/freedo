"""The freedo integration: a local Tado coordinator."""

from __future__ import annotations

from homeassistant.const import CONF_HOST, CONF_PORT, Platform
from homeassistant.core import HomeAssistant
from homeassistant.helpers import config_validation as cv, device_registry as dr
from homeassistant.helpers.aiohttp_client import async_get_clientsession
from homeassistant.helpers.typing import ConfigType

from .api import FreedoClient
from .const import CONF_TOKEN, DOMAIN
from .coordinator import FreedoConfigEntry, FreedoCoordinator, FreedoRuntime
from .entity import async_track_firmware, hub_device_info
from .environment import async_setup_environment
from .services import async_setup_services

PLATFORMS = [
    Platform.BINARY_SENSOR,
    Platform.BUTTON,
    Platform.CLIMATE,
    Platform.NUMBER,
    Platform.SELECT,
    Platform.SENSOR,
    Platform.SWITCH,
    Platform.WATER_HEATER,
]


CONFIG_SCHEMA = cv.config_entry_only_config_schema(DOMAIN)


async def async_setup(hass: HomeAssistant, config: ConfigType) -> bool:
    async_setup_services(hass)
    return True


async def async_setup_entry(hass: HomeAssistant, entry: FreedoConfigEntry) -> bool:
    client = FreedoClient(
        async_get_clientsession(hass),
        entry.data[CONF_HOST],
        entry.data[CONF_PORT],
        entry.data.get(CONF_TOKEN),
    )
    coordinator = FreedoCoordinator(hass, entry, client)
    await coordinator.async_config_entry_first_refresh()
    hub = dr.async_get(hass).async_get_or_create(
        config_entry_id=entry.entry_id, **hub_device_info(entry.entry_id)
    )
    entry.runtime_data = FreedoRuntime(
        client=client, coordinator=coordinator, hub_device_id=hub.id
    )
    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)
    entry.async_on_unload(async_track_firmware(hass, coordinator))
    async_setup_environment(hass, entry, client)
    return True


async def async_unload_entry(hass: HomeAssistant, entry: FreedoConfigEntry) -> bool:
    return await hass.config_entries.async_unload_platforms(entry, PLATFORMS)
