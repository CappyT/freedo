"""Environment inputs: Home Assistant states pushed to the daemon.

- The outdoor temperature sensor goes to POST /api/outdoor on every state
  change and every OUTDOOR_PUSH_INTERVAL. The daemon keeps the reading in
  memory only and drops a curve back to its default after outdoor_max_age_sec,
  so the periodic push also covers a daemon restart.
- The window sensors of a zone go to POST /api/zones/{n}/window on each
  change of the zone state, one push per zone at a time.

Unavailable or unknown states are never pushed.
"""

from __future__ import annotations

import asyncio
from datetime import datetime
import logging

from homeassistant.const import ATTR_UNIT_OF_MEASUREMENT
from homeassistant.core import Event, EventStateChangedData, HomeAssistant, callback
from homeassistant.helpers.event import (
    async_track_state_change_event,
    async_track_time_interval,
)

from .api import FreedoClient, FreedoError
from .const import CONF_OUTDOOR_ENTITY, CONF_WINDOWS, OUTDOOR_PUSH_INTERVAL
from .coordinator import FreedoConfigEntry
from .model import outdoor_payload, window_payload, window_zones

_LOGGER = logging.getLogger(__name__)


@callback
def async_setup_environment(
    hass: HomeAssistant, entry: FreedoConfigEntry, client: FreedoClient
) -> None:
    """Register the listeners. The entry unload removes them."""
    outdoor = entry.options.get(CONF_OUTDOOR_ENTITY)
    zones = window_zones(entry.options.get(CONF_WINDOWS) or {})

    if outdoor:

        async def push_outdoor(*_: object) -> None:
            state = hass.states.get(outdoor)
            if state is None:
                return
            payload = outdoor_payload(
                state.state, state.attributes.get(ATTR_UNIT_OF_MEASUREMENT)
            )
            if payload is None:
                _LOGGER.debug("%s is %s: outdoor temperature not pushed", outdoor, state.state)
                return
            try:
                await client.push_outdoor(payload["tempC"])
            except FreedoError as err:
                _LOGGER.warning("Outdoor temperature push failed: %s", err)

        @callback
        def outdoor_changed(event: Event[EventStateChangedData]) -> None:
            hass.async_create_task(push_outdoor())

        @callback
        def outdoor_tick(_: datetime) -> None:
            hass.async_create_task(push_outdoor())

        entry.async_on_unload(
            async_track_state_change_event(hass, [outdoor], outdoor_changed)
        )
        entry.async_on_unload(
            async_track_time_interval(hass, outdoor_tick, OUTDOOR_PUSH_INTERVAL)
        )
        entry.async_create_background_task(
            hass, push_outdoor(), "freedo outdoor initial push"
        )

    if zones:
        last: dict[int, bool] = {}
        # One push per zone at a time. A push reads the sensor states once it
        # holds the lock, so a later change never lands before an earlier one.
        locks = {zone: asyncio.Lock() for zone in zones}

        async def push_window(zone: int, force: bool = False) -> None:
            async with locks[zone]:
                states = []
                for entity_id in zones[zone]:
                    state = hass.states.get(entity_id)
                    states.append(state.state if state else None)
                payload = window_payload(states)
                if payload is None:
                    return
                if not force and last.get(zone) == payload["open"]:
                    return
                try:
                    await client.push_window(zone, payload["open"])
                except FreedoError as err:
                    _LOGGER.warning("Window push for zone %s failed: %s", zone, err)
                    return
                last[zone] = payload["open"]

        entity_zone = {e: z for z, entities in zones.items() for e in entities}

        @callback
        def window_changed(event: Event[EventStateChangedData]) -> None:
            zone = entity_zone.get(event.data["entity_id"])
            if zone is not None:
                hass.async_create_task(push_window(zone))

        entry.async_on_unload(
            async_track_state_change_event(hass, list(entity_zone), window_changed)
        )

        async def initial_sync() -> None:
            # Align the daemon after a Home Assistant or options reload.
            for zone in zones:
                await push_window(zone, force=True)

        entry.async_create_background_task(
            hass, initial_sync(), "freedo window initial push"
        )
