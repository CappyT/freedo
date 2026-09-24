"""The freedo integration: a local Tado coordinator."""

from __future__ import annotations

from homeassistant.const import CONF_HOST, CONF_PORT, Platform
from homeassistant.core import HomeAssistant
from homeassistant.helpers.aiohttp_client import async_get_clientsession

from .api import FreedoClient
from .const import CONF_TOKEN
from .coordinator import FreedoConfigEntry, FreedoCoordinator, FreedoRuntime
from .environment import async_setup_environment

PLATFORMS = [
    Platform.BINARY_SENSOR,
    Platform.CLIMATE,
    Platform.SELECT,
    Platform.SENSOR,
    Platform.SWITCH,
    Platform.WATER_HEATER,
]


async def async_setup_entry(hass: HomeAssistant, entry: FreedoConfigEntry) -> bool:
    client = FreedoClient(
        async_get_clientsession(hass),
        entry.data[CONF_HOST],
        entry.data[CONF_PORT],
        entry.data.get(CONF_TOKEN),
    )
    coordinator = FreedoCoordinator(hass, entry, client)
    await coordinator.async_config_entry_first_refresh()
    entry.runtime_data = FreedoRuntime(client=client, coordinator=coordinator)
    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)
    async_setup_environment(hass, entry, client)
    return True


async def async_unload_entry(hass: HomeAssistant, entry: FreedoConfigEntry) -> bool:
    return await hass.config_entries.async_unload_platforms(entry, PLATFORMS)
