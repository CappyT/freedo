"""The freedo integration: a local Tado coordinator."""

from __future__ import annotations

import asyncio
import logging
import re

from homeassistant.config_entries import SOURCE_HASSIO
from homeassistant.const import CONF_HOST, CONF_PORT, Platform
from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers import (
    config_validation as cv,
    device_registry as dr,
    entity_registry as er,
)
from homeassistant.helpers.aiohttp_client import async_get_clientsession
from homeassistant.helpers.typing import ConfigType

from .api import FreedoClient
from .const import CONF_TOKEN, DOMAIN
from .coordinator import FreedoConfigEntry, FreedoCoordinator, FreedoRuntime
from .entity import async_track_firmware, hub_device_info
from .environment import async_setup_environment
from .model import FreedoData, installation_structure
from .rooms import async_remove_issues, async_track_rooms, node_identifier, zone_identifier
from .services import async_setup_services

PLATFORMS = [
    Platform.BINARY_SENSOR,
    Platform.BUTTON,
    Platform.CLIMATE,
    Platform.EVENT,
    Platform.NUMBER,
    Platform.SELECT,
    Platform.SENSOR,
    Platform.SWITCH,
    Platform.WATER_HEATER,
]

_LOGGER = logging.getLogger(__name__)

# Zone entities that 0.11.0-beta.12 no longer creates: the select Room
# temperature correction, the switch Valve calibration and the number
# Calibration.
_REMOVED_ZONE_ENTITY = re.compile(
    r"_zone\d+_(room_temperature_correction|auto_offset|auto_offset_interval_sec)$"
)

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
        config_entry_id=entry.entry_id,
        configuration_url=_dashboard_url(entry),
        **hub_device_info(entry.entry_id),
    )
    entry.runtime_data = FreedoRuntime(
        client=client,
        coordinator=coordinator,
        hub_device_id=hub.id,
        room_temp_lock=asyncio.Lock(),
    )
    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)
    _remove_departed(hass, entry, coordinator.data)
    _remove_retired_entities(hass, entry)
    _follow_installation(hass, entry, coordinator)
    entry.async_on_unload(async_track_firmware(hass, coordinator))
    entry.async_on_unload(async_track_rooms(hass, coordinator))
    entry.runtime_data.room_temps, entry.runtime_data.windows = async_setup_environment(
        hass, entry, client
    )
    return True


def _dashboard_url(entry: FreedoConfigEntry) -> str:
    """The freedo dashboard, which manages the rooms and pairs a device. The
    add-on serves it through ingress: its host name is its slug with a hyphen
    for each underscore."""
    host = entry.data[CONF_HOST]
    if entry.source == SOURCE_HASSIO:
        return f"homeassistant://hassio/ingress/{host.replace('-', '_')}"
    return f"http://{host}:{entry.data[CONF_PORT]}/"


@callback
def _remove_departed(hass: HomeAssistant, entry: FreedoConfigEntry, data: FreedoData) -> None:
    """Remove the devices, with their entities, of the rooms and the devices
    that left the installation. A device that stays keeps its area and its
    names. A poll that shows no installation removes nothing."""
    if not data.zones or not data.install_devices:
        return
    keep = {(DOMAIN, entry.entry_id)}
    keep.update(zone_identifier(entry.entry_id, number) for number in data.zones)
    keep.update(
        node_identifier(entry.entry_id, short)
        for short in data.devices.keys() | data.install_devices.keys()
    )
    registry = dr.async_get(hass)
    for device in dr.async_entries_for_config_entry(registry, entry.entry_id):
        if not device.identifiers & keep:
            registry.async_remove_device(device.id)


@callback
def _remove_retired_entities(hass: HomeAssistant, entry: FreedoConfigEntry) -> None:
    """Remove the zone entities that a release no longer creates. An upgrade
    keeps them in the registry as unavailable."""
    registry = er.async_get(hass)
    for entity in er.async_entries_for_config_entry(registry, entry.entry_id):
        if _REMOVED_ZONE_ENTITY.search(entity.unique_id):
            _LOGGER.debug("Removing the retired entity %s", entity.entity_id)
            registry.async_remove(entity.entity_id)


@callback
def _follow_installation(
    hass: HomeAssistant, entry: FreedoConfigEntry, coordinator: FreedoCoordinator
) -> None:
    """Reload the entry when a poll shows other rooms or devices than the ones
    the entities were built from: a room or a device added, removed, renamed or
    moved. The revision of the installation does not fit: it also moves at each
    option write, such as the area the integration itself sends."""
    built = installation_structure(coordinator.data)
    reloading = False

    @callback
    def check() -> None:
        nonlocal reloading
        if not reloading and installation_structure(coordinator.data) != built:
            reloading = True
            hass.config_entries.async_schedule_reload(entry.entry_id)

    entry.async_on_unload(coordinator.async_add_listener(check))


async def async_unload_entry(hass: HomeAssistant, entry: FreedoConfigEntry) -> bool:
    return await hass.config_entries.async_unload_platforms(entry, PLATFORMS)


async def async_remove_entry(hass: HomeAssistant, entry: FreedoConfigEntry) -> None:
    async_remove_issues(hass, entry.entry_id)
