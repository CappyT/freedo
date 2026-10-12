"""Environment inputs: Home Assistant states pushed to the daemon.

- The outdoor temperature sensor goes to POST /api/outdoor on every state
  change and every OUTDOOR_PUSH_INTERVAL. The daemon keeps the reading in
  memory only and drops a curve back to its default after outdoor_max_age_sec,
  so the periodic push also covers a daemon restart.
- The temperature sensor of a heating zone goes to POST /api/zones/{n}/temperature
  on every state change and every ROOM_TEMP_PUSH_INTERVAL. The daemon keeps the
  reading in memory only and ends the correction when the reading is older than
  room_temp_max_age_sec, so the periodic push also covers a daemon restart. The
  push names the sensor as its source. A failed push is logged once until a push
  succeeds. See RoomTemps.
- The window sensors of a zone go to POST /api/zones/{n}/window on each
  change of the zone state, one push per zone at a time. A push the daemon
  did not take (no answer or a 5xx) is sent again, with the state the
  sensors have then, every WINDOW_RETRY_INTERVAL until it succeeds. A 4xx is
  not retried.

Unavailable or unknown states are never pushed.
"""

from __future__ import annotations

import asyncio
from collections.abc import Callable
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
) -> RoomTemps:
    """Register the listeners. The entry unload removes them. The room
    temperature pushes come back, so the temperature source select can map a
    sensor without a reload."""
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

    room_temps = RoomTemps(hass, entry, client)
    room_temps.async_start()
    return room_temps


class RoomTemps:
    """The room temperature pushes: the mapped sensor of each heating zone goes
    to POST /api/zones/{n}/temperature on every state change and every
    ROOM_TEMP_PUSH_INTERVAL, with the sensor name as the source label. The
    mapping and the pause are read from the options at each push, so a change
    needs no reload; async_remap follows a changed mapping. A failed push is
    logged once until a push of the zone succeeds."""

    def __init__(
        self, hass: HomeAssistant, entry: FreedoConfigEntry, client: FreedoClient
    ) -> None:
        self._hass = hass
        self._entry = entry
        self._client = client
        self._failed: set[int] = set()
        self._unsub_state: Callable[[], None] | None = None

    def mapping(self) -> dict[int, str]:
        return room_temp_zones(self._entry.options.get(CONF_ROOM_TEMPS) or {})

    @callback
    def async_start(self) -> None:
        """Follow the mapped sensors and send their readings at once."""
        self._entry.async_on_unload(self._stop)
        self._entry.async_on_unload(
            async_track_time_interval(self._hass, self._tick, ROOM_TEMP_PUSH_INTERVAL)
        )
        self.async_remap()
        if self.mapping():
            self._entry.async_create_background_task(
                self._hass, self._push_all(), "freedo room temperature initial push"
            )

    @callback
    def async_remap(self) -> None:
        """Follow the sensors the options map now."""
        self._stop()
        mapping = self.mapping()
        if mapping:
            self._unsub_state = async_track_state_change_event(
                self._hass, sorted(set(mapping.values())), self._changed
            )

    @callback
    def _stop(self) -> None:
        if self._unsub_state is not None:
            self._unsub_state()
            self._unsub_state = None

    @callback
    def _changed(self, event: Event[EventStateChangedData]) -> None:
        for zone, entity_id in self.mapping().items():
            if entity_id == event.data["entity_id"]:
                self._hass.async_create_task(self.push(zone))

    @callback
    def _tick(self, _: datetime) -> None:
        for zone in self.mapping():
            self._hass.async_create_task(self.push(zone))

    async def _push_all(self) -> None:
        for zone in self.mapping():
            await self.push(zone)

    async def push(self, zone: int) -> None:
        """Send the current reading of the sensor of zone. The temperature
        source select pauses the pushes of a zone whose room measures on a
        device. The lock serializes the push with the pause and the clear of
        the zone, so no push lands after the clear."""
        async with self._entry.runtime_data.room_temp_lock:
            if zone in room_temp_paused(self._entry.options.get(CONF_ROOM_TEMP_PAUSED) or []):
                return
            entity_id = self.mapping().get(zone)
            state = self._hass.states.get(entity_id) if entity_id else None
            if state is None:
                return
            payload = room_temp_payload(
                state.state, state.attributes.get(ATTR_UNIT_OF_MEASUREMENT)
            )
            if payload is None:
                _LOGGER.debug("%s is %s: room temperature not pushed", entity_id, state.state)
                return
            try:
                await self._client.push_room_temp(
                    zone, payload["tempC"], state.name if self._takes_source(zone) else None
                )
            except FreedoError as err:
                if zone not in self._failed:
                    _LOGGER.warning("Room temperature push for zone %s failed: %s", zone, err)
                self._failed.add(zone)
                return
            self._failed.discard(zone)

    def _takes_source(self, zone: int) -> bool:
        """A daemon that serves externalTemp.source takes the label; an older
        one refuses an unknown field."""
        data = self._entry.runtime_data.coordinator.data
        return data is not None and "source" in (data.room_temps.get(zone) or {})
