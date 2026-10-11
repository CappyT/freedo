"""Home level services: boost or resume every zone in one call."""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from functools import partial

import voluptuous as vol

from homeassistant.components.climate import HVACMode
from homeassistant.core import HomeAssistant, ServiceCall, callback
from homeassistant.exceptions import HomeAssistantError, ServiceValidationError
from homeassistant.helpers import config_validation as cv
from homeassistant.helpers.entity_platform import async_get_platforms

from .climate import FreedoZoneClimate
from .const import DOMAIN, SERVICE_BOOST_ALL, SERVICE_RESUME_ALL
from .water_heater import STATE_AUTO, FreedoHotWater


def _zones(hass: HomeAssistant, kind: type) -> list:
    """The enabled zone entities of one platform, of every freedo entry."""
    return [
        entity
        for platform in async_get_platforms(hass, DOMAIN)
        for entity in platform.entities.values()
        if isinstance(entity, kind)
    ]


async def _each(calls: list[tuple[str, Callable[[], Awaitable[None]]]]) -> None:
    """Run every call. A zone the daemon refuses does not stop the others: the
    failures come back as one error."""
    failures: list[str] = []
    for entity_id, call in calls:
        try:
            await call()
        except HomeAssistantError as err:
            failures.append(f"{entity_id}: {err}")
    if failures:
        raise HomeAssistantError(
            translation_domain=DOMAIN,
            translation_key="zones_failed",
            translation_placeholders={"failures": "; ".join(failures)},
        )


@callback
def async_setup_services(hass: HomeAssistant) -> None:
    async def boost_all(call: ServiceCall) -> None:
        climates = _zones(hass, FreedoZoneClimate)
        if not climates:
            raise ServiceValidationError(translation_domain=DOMAIN, translation_key="no_zones")
        await _each([(z.entity_id, partial(z.async_boost, call.data["on"])) for z in climates])

    async def resume_all(call: ServiceCall) -> None:
        climates = _zones(hass, FreedoZoneClimate)
        water = _zones(hass, FreedoHotWater)
        if not climates and not water:
            raise ServiceValidationError(translation_domain=DOMAIN, translation_key="no_zones")
        await _each(
            [(z.entity_id, partial(z.async_set_hvac_mode, HVACMode.AUTO)) for z in climates]
            + [(z.entity_id, partial(z.async_set_operation_mode, STATE_AUTO)) for z in water]
        )

    hass.services.async_register(
        DOMAIN, SERVICE_BOOST_ALL, boost_all, vol.Schema({vol.Optional("on", default=True): cv.boolean})
    )
    hass.services.async_register(DOMAIN, SERVICE_RESUME_ALL, resume_all, vol.Schema({}))
