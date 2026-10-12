"""Services of the integration: boost or resume every zone in one call, and
the weekly schedule of a heating zone or the hot water zone."""

from __future__ import annotations

import asyncio
from functools import partial

import voluptuous as vol

from homeassistant.components.climate import HVACMode
from homeassistant.core import HomeAssistant, ServiceCall, SupportsResponse, callback
from homeassistant.exceptions import ServiceValidationError
from homeassistant.helpers import config_validation as cv
from homeassistant.helpers.entity_platform import async_get_platforms
from homeassistant.helpers.service import async_extract_entities

from .climate import FreedoZoneClimate
from .const import (
    DOMAIN,
    SERVICE_BOOST_ALL,
    SERVICE_CALIBRATE_GAS_METER,
    SERVICE_COPY_SCHEDULE,
    SERVICE_GET_SCHEDULE,
    SERVICE_RESUME_ALL,
    SERVICE_SET_SCHEDULE,
    SERVICE_UNDO_GAS_METER_READING,
)
from .entity import FreedoScheduleEntity, call_api, each
from .water_heater import STATE_AUTO, FreedoHotWater


def _zones(hass: HomeAssistant, kind: type) -> list:
    """The enabled zone entities of one platform, of every freedo entry."""
    return [
        entity
        for platform in async_get_platforms(hass, DOMAIN)
        for entity in platform.entities.values()
        if isinstance(entity, kind)
    ]


def _runtimes(hass: HomeAssistant) -> list:
    """The runtime of every loaded freedo entry."""
    return [
        entry.runtime_data
        for entry in hass.config_entries.async_entries(DOMAIN)
        if getattr(entry, "runtime_data", None) is not None
    ]


@callback
def async_setup_services(hass: HomeAssistant) -> None:
    async def boost_all(call: ServiceCall) -> None:
        climates = _zones(hass, FreedoZoneClimate)
        if not climates:
            raise ServiceValidationError(translation_domain=DOMAIN, translation_key="no_zones")
        await each([(z.entity_id, partial(z.async_boost, call.data["on"])) for z in climates])

    async def resume_all(call: ServiceCall) -> None:
        climates = _zones(hass, FreedoZoneClimate)
        water = _zones(hass, FreedoHotWater)
        if not climates and not water:
            raise ServiceValidationError(translation_domain=DOMAIN, translation_key="no_zones")
        await each(
            [(z.entity_id, partial(z.async_set_hvac_mode, HVACMode.AUTO)) for z in climates]
            + [(z.entity_id, partial(z.async_set_operation_mode, STATE_AUTO)) for z in water]
        )

    async def schedule_targets(call: ServiceCall) -> list[FreedoScheduleEntity]:
        """The zone entities the call targets, climate and water heater alike:
        one platform registers a service for its own entities only."""
        return await async_extract_entities(_zones(hass, FreedoScheduleEntity), call)

    async def get_schedule(call: ServiceCall) -> dict[str, dict]:
        targets = await schedule_targets(call)
        views = await asyncio.gather(*(t.async_get_schedule() for t in targets))
        return {t.entity_id: view for t, view in zip(targets, views)}

    async def set_schedule(call: ServiceCall) -> None:
        targets = await schedule_targets(call)
        # Every target is checked before any one is written.
        for target in targets:
            target.checked_schedule(call.data["programs"])
        await asyncio.gather(*(t.async_set_schedule(call.data["programs"]) for t in targets))

    async def copy_schedule(call: ServiceCall) -> None:
        targets = await schedule_targets(call)
        await asyncio.gather(*(t.async_copy_schedule(call.data["to"]) for t in targets))

    async def calibrate_gas_meter(call: ServiceCall) -> None:
        runtimes = _runtimes(hass)
        if not runtimes:
            raise ServiceValidationError(translation_domain=DOMAIN, translation_key="no_hub")
        for runtime in runtimes:
            await call_api(
                runtime.client.record_gas_meter(
                    call.data["value"], call.data.get("estimate"), call.data.get("at")
                )
            )
            await runtime.coordinator.async_request_refresh()

    async def undo_gas_meter_reading(call: ServiceCall) -> None:
        runtimes = _runtimes(hass)
        if not runtimes:
            raise ServiceValidationError(translation_domain=DOMAIN, translation_key="no_hub")
        for runtime in runtimes:
            await call_api(runtime.client.undo_gas_meter_reading())
            await runtime.coordinator.async_request_refresh()

    hass.services.async_register(
        DOMAIN,
        SERVICE_GET_SCHEDULE,
        get_schedule,
        cv.make_entity_service_schema({}),
        SupportsResponse.ONLY,
    )
    hass.services.async_register(
        DOMAIN,
        SERVICE_SET_SCHEDULE,
        set_schedule,
        cv.make_entity_service_schema({vol.Required("programs"): vol.All(cv.ensure_list, [dict])}),
    )
    hass.services.async_register(
        DOMAIN,
        SERVICE_COPY_SCHEDULE,
        copy_schedule,
        cv.make_entity_service_schema({vol.Required("to"): cv.entity_ids}),
    )
    hass.services.async_register(
        DOMAIN, SERVICE_BOOST_ALL, boost_all, vol.Schema({vol.Optional("on", default=True): cv.boolean})
    )
    hass.services.async_register(DOMAIN, SERVICE_RESUME_ALL, resume_all, vol.Schema({}))
    hass.services.async_register(
        DOMAIN,
        SERVICE_CALIBRATE_GAS_METER,
        calibrate_gas_meter,
        vol.Schema(
            {
                vol.Required("value"): vol.Coerce(float),
                vol.Optional("estimate"): vol.Coerce(float),
                vol.Optional("at"): cv.datetime,
            }
        ),
    )
    hass.services.async_register(
        DOMAIN, SERVICE_UNDO_GAS_METER_READING, undo_gas_meter_reading, vol.Schema({})
    )
