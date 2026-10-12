"""Environment inputs: Home Assistant states pushed to the daemon.

- The outdoor temperature sensor goes to POST /api/outdoor on every state
  change and every OUTDOOR_PUSH_INTERVAL. The daemon keeps the reading in
  memory only and drops a curve back to its default after outdoor_max_age_sec,
  so the periodic push also covers a daemon restart.
- The temperature sensor of a heating zone goes to POST /api/zones/{n}/temperature
  on every state change and every ROOM_TEMP_PUSH_INTERVAL. The daemon keeps the
  reading in memory only and ends the correction when the reading is older than
  room_temp_max_age_sec, so the periodic push also covers a daemon restart. A
  failed push is logged once until a push succeeds.
- The window sensors of a zone go to POST /api/zones/{n}/window on each
  change of the zone state, one push per zone at a time. A push the daemon
  did not take (no answer or a 5xx) is sent again, with the state the
  sensors have then, every WINDOW_RETRY_INTERVAL until it succeeds. A 4xx is
  not retried.

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

from .api import FreedoApiError, FreedoClient, FreedoError
from .const import (
    CONF_OUTDOOR_ENTITY,
    CONF_ROOM_TEMP_PAUSED,
    CONF_ROOM_TEMPS,
    CONF_WINDOWS,
    OUTDOOR_PUSH_INTERVAL,
    ROOM_TEMP_PUSH_INTERVAL,
    WINDOW_RETRY_INTERVAL,
)
from .coordinator import FreedoConfigEntry
from .model import (
    outdoor_payload,
    room_temp_payload,
    room_temp_paused,
    room_temp_zones,
    window_payload,
    window_zones,
)

_LOGGER = logging.getLogger(__name__)


def _transient(err: FreedoError) -> bool:
    """A push that may pass later: the daemon did not answer or failed itself.
    A 4xx refuses the request, and the same push would fail again."""
    return not isinstance(err, FreedoApiError) or err.status >= 500


@callback
def async_setup_environment(
    hass: HomeAssistant, entry: FreedoConfigEntry, client: FreedoClient
) -> None:
    """Register the listeners. The entry unload removes them."""
    outdoor = entry.options.get(CONF_OUTDOOR_ENTITY)
    zones = window_zones(entry.options.get(CONF_WINDOWS) or {})
    room_temps = room_temp_zones(entry.options.get(CONF_ROOM_TEMPS) or {})

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

    if room_temps:
        # Zones whose last push failed.
        room_failed: set[int] = set()

        async def push_room_temp(zone: int) -> None:
            # The temperature source select pauses the pushes of a zone whose
            # room measures on a device. The options are read here, so the
            # change needs no reload.
            if zone in room_temp_paused(entry.options.get(CONF_ROOM_TEMP_PAUSED) or []):
                return
            entity_id = room_temps[zone]
            state = hass.states.get(entity_id)
            if state is None:
                return
            payload = room_temp_payload(
                state.state, state.attributes.get(ATTR_UNIT_OF_MEASUREMENT)
            )
            if payload is None:
                _LOGGER.debug("%s is %s: room temperature not pushed", entity_id, state.state)
                return
            try:
                await client.push_room_temp(zone, payload["tempC"])
            except FreedoError as err:
                if zone not in room_failed:
                    _LOGGER.warning("Room temperature push for zone %s failed: %s", zone, err)
                room_failed.add(zone)
                return
            room_failed.discard(zone)

        room_entity_zones: dict[str, list[int]] = {}
        for zone, entity_id in room_temps.items():
            room_entity_zones.setdefault(entity_id, []).append(zone)

        @callback
        def room_temp_changed(event: Event[EventStateChangedData]) -> None:
            for zone in room_entity_zones[event.data["entity_id"]]:
                hass.async_create_task(push_room_temp(zone))

        @callback
        def room_temp_tick(_: datetime) -> None:
            for zone in room_temps:
                hass.async_create_task(push_room_temp(zone))

        entry.async_on_unload(
            async_track_state_change_event(hass, list(room_entity_zones), room_temp_changed)
        )
        entry.async_on_unload(
            async_track_time_interval(hass, room_temp_tick, ROOM_TEMP_PUSH_INTERVAL)
        )

        async def initial_room_temp_push() -> None:
            for zone in room_temps:
                await push_room_temp(zone)

        entry.async_create_background_task(
            hass, initial_room_temp_push(), "freedo room temperature initial push"
        )

    if zones:
        last: dict[int, bool] = {}
        # Zones whose last push failed on a transient error.
        failed: set[int] = set()
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
                    if _transient(err):
                        if zone not in failed:
                            _LOGGER.warning(
                                "Window push for zone %s failed, retrying every %s: %s",
                                zone, WINDOW_RETRY_INTERVAL, err,
                            )
                        failed.add(zone)
                    else:
                        failed.discard(zone)
                        _LOGGER.warning("Window push for zone %s refused: %s", zone, err)
                    return
                failed.discard(zone)
                last[zone] = payload["open"]

        @callback
        def window_retry_tick(_: datetime) -> None:
            for zone in sorted(failed):
                hass.async_create_task(push_window(zone, force=True))

        entry.async_on_unload(
            async_track_time_interval(hass, window_retry_tick, WINDOW_RETRY_INTERVAL)
        )

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
