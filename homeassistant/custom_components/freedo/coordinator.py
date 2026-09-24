"""Polling coordinator of the freedo integration."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
import logging

from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import ConfigEntryAuthFailed
from homeassistant.helpers.update_coordinator import DataUpdateCoordinator, UpdateFailed

from .api import FreedoAuthError, FreedoClient, FreedoError
from .const import DOMAIN, SCAN_INTERVAL
from .model import FreedoData, parse

_LOGGER = logging.getLogger(__name__)


@dataclass
class FreedoRuntime:
    client: FreedoClient
    coordinator: FreedoCoordinator


type FreedoConfigEntry = ConfigEntry[FreedoRuntime]


class FreedoCoordinator(DataUpdateCoordinator[FreedoData]):
    """Reads health, zones, nodes, boiler and home state in one poll."""

    config_entry: FreedoConfigEntry

    def __init__(
        self, hass: HomeAssistant, entry: FreedoConfigEntry, client: FreedoClient
    ) -> None:
        super().__init__(
            hass,
            _LOGGER,
            name=DOMAIN,
            config_entry=entry,
            update_interval=SCAN_INTERVAL,
        )
        self.client = client

    async def _async_update_data(self) -> FreedoData:
        try:
            health, zones, nodes, boiler, home = await asyncio.gather(
                self.client.health(),
                self.client.zones(),
                self.client.nodes(),
                self.client.boiler(),
                self.client.home(),
            )
        except FreedoAuthError as err:
            raise ConfigEntryAuthFailed(str(err)) from err
        except FreedoError as err:
            raise UpdateFailed(str(err)) from err
        return parse(health, zones, nodes, boiler, home)
