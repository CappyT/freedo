"""Polling coordinator of the freedo integration."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
import logging

from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import ConfigEntryAuthFailed
from homeassistant.helpers.update_coordinator import DataUpdateCoordinator, UpdateFailed

from .api import FreedoApiError, FreedoAuthError, FreedoClient, FreedoError
from .const import DOMAIN, SCAN_INTERVAL
from .model import FreedoData, parse

_LOGGER = logging.getLogger(__name__)


@dataclass
class FreedoRuntime:
    client: FreedoClient
    coordinator: FreedoCoordinator
    # Device registry id of the hub device, the via device of every other one.
    hub_device_id: str


type FreedoConfigEntry = ConfigEntry[FreedoRuntime]


class FreedoCoordinator(DataUpdateCoordinator[FreedoData]):
    """Reads health, zones, nodes, boiler, home state, the installation
    options, the zone installation and the circuits in one poll."""

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
            health, zones, nodes, boiler, home, install, settings, circuits = await asyncio.gather(
                self.client.health(),
                self.client.zones(),
                self.client.nodes(),
                self.client.boiler(),
                self.client.home(),
                self._install_zones(),
                self._install(),
                self._install_circuits(),
            )
        except FreedoAuthError as err:
            raise ConfigEntryAuthFailed(str(err)) from err
        except FreedoError as err:
            raise UpdateFailed(str(err)) from err
        return parse(health, zones, nodes, boiler, home, install, settings, circuits)

    async def _install_zones(self) -> dict:
        """GET /api/install/zones, for the setpoint offset of each zone."""
        return await _without_install(self.client.install_zones())

    async def _install(self) -> dict:
        """GET /api/install, for options.offline_schedule."""
        return await _without_install(self.client.install())

    async def _install_circuits(self) -> dict:
        """GET /api/install/circuits, for the heat source of each circuit."""
        return await _without_install(self.client.install_circuits())


async def _without_install(request) -> dict:
    """The body of an installation read, empty on a 503: the daemon holds no
    installation."""
    try:
        return await request
    except FreedoApiError as err:
        if err.status == 503:
            return {}
        raise
