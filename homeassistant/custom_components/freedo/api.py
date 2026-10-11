"""Async client of the freedo local HTTP API.

This module imports nothing from Home Assistant, so the unit tests run
without it.
"""

from __future__ import annotations

import asyncio
from typing import Any

import aiohttp


class FreedoError(Exception):
    """Base error of the freedo client."""


class FreedoConnectionError(FreedoError):
    """The daemon did not answer."""


class FreedoAuthError(FreedoError):
    """The daemon rejected the API token (HTTP 401)."""


class FreedoApiError(FreedoError):
    """The daemon answered with an error status."""

    def __init__(self, status: int, message: str | None) -> None:
        super().__init__(f"HTTP {status}: {message or 'no error message'}")
        self.status = status
        self.message = message


class FreedoClient:
    """Talks to one freedo daemon."""

    def __init__(
        self,
        session: aiohttp.ClientSession,
        host: str,
        port: int,
        token: str | None = None,
        timeout: float = 10.0,
    ) -> None:
        if ":" in host and not host.startswith("["):
            host = f"[{host}]"
        self._base = f"http://{host}:{port}"
        self._session = session
        self._token = token or None
        self._timeout = timeout

    async def _request(
        self, method: str, path: str, payload: dict[str, Any] | None = None
    ) -> Any:
        headers = {"Accept": "application/json"}
        if self._token:
            headers["Authorization"] = f"Bearer {self._token}"
        try:
            async with self._session.request(
                method,
                self._base + path,
                json=payload,
                headers=headers,
                timeout=aiohttp.ClientTimeout(total=self._timeout),
            ) as resp:
                if resp.status == 401:
                    raise FreedoAuthError("the daemon rejected the API token")
                try:
                    body = await resp.json(content_type=None)
                except ValueError:
                    body = None
                if resp.status >= 400:
                    message = body.get("error") if isinstance(body, dict) else None
                    raise FreedoApiError(resp.status, message)
                return body
        except (aiohttp.ClientError, asyncio.TimeoutError) as err:
            raise FreedoConnectionError(
                f"{method} {path}: {err or type(err).__name__}"
            ) from err

    # --- reads ---

    async def health(self) -> dict[str, Any]:
        return await self._request("GET", "/api/health")

    async def zones(self) -> dict[str, Any]:
        return await self._request("GET", "/api/zones")

    async def nodes(self) -> list[dict[str, Any]]:
        return await self._request("GET", "/api/nodes")

    async def boiler(self) -> dict[str, Any]:
        return await self._request("GET", "/api/boiler")

    async def home(self) -> dict[str, Any]:
        return await self._request("GET", "/api/home")

    async def install(self) -> dict[str, Any]:
        return await self._request("GET", "/api/install")

    async def install_zones(self) -> dict[str, Any]:
        return await self._request("GET", "/api/install/zones")

    async def install_circuits(self) -> dict[str, Any]:
        return await self._request("GET", "/api/install/circuits")

    # --- writes ---

    async def set_away(self, away: bool) -> dict[str, Any]:
        return await self._request("PATCH", "/api/home", {"away": away})

    async def set_offline_schedule(self, enabled: bool) -> dict[str, Any]:
        return await self._request(
            "PATCH", "/api/install", {"offline_schedule": enabled}
        )

    async def set_display(
        self,
        node: str,
        orientation: str | None = None,
        notification: bool | None = None,
    ) -> dict[str, Any]:
        """Write a valve's display settings. An omitted field keeps its value."""
        payload: dict[str, Any] = {}
        if orientation is not None:
            payload["orientation"] = orientation
        if notification is not None:
            payload["notification"] = notification
        return await self._request("PUT", f"/api/nodes/{node}/display", payload)

    async def set_child_lock(self, node: str, on: bool) -> dict[str, Any]:
        return await self._request("POST", f"/api/nodes/{node}/childlock", {"on": on})

    async def identify(self, node: str) -> dict[str, Any]:
        return await self._request("POST", f"/api/nodes/{node}/identify")

    async def calibrate(self, node: str) -> dict[str, Any]:
        return await self._request("POST", f"/api/nodes/{node}/calibrate")

    async def set_zone_offset(self, zone: int, offset_c: float) -> dict[str, Any]:
        return await self._request(
            "POST", f"/api/zones/{int(zone)}/offset", {"offsetC": offset_c}
        )

    async def patch_install_zone(self, zone: int, **fields: Any) -> dict[str, Any]:
        """Write fields of a zone row: early_start, open_window_detection,
        open_window_duration_sec, device_overlay_end, device_overlay_end_sec."""
        return await self._request("PATCH", f"/api/install/zones/{int(zone)}", fields)

    async def zone_schedule(self, zone: int) -> dict[str, Any] | None:
        """The weekly schedule of a zone, None when no member holds one. Any
        other error, a 404 for an unknown zone included, is raised."""
        try:
            return await self._request("GET", f"/api/zones/{int(zone)}/schedule")
        except FreedoApiError as err:
            if err.status == 404 and err.message == f"zone {int(zone)} has no schedule":
                return None
            raise

    async def set_zone_schedule(
        self, zone: int, programs: list[dict[str, Any]]
    ) -> dict[str, Any]:
        return await self._request(
            "PUT", f"/api/zones/{int(zone)}/schedule", {"programs": programs}
        )

    async def set_flow_limit(self, circuit: int, temp_c: float, target: str) -> dict[str, Any]:
        """Serve a flow limit to a circuit. target is the wall thermostat that
        drives it: without one the daemon takes the hot-water thermostat."""
        return await self._request(
            "POST", "/api/boiler/flow", {"tempC": temp_c, "id": int(circuit), "target": target}
        )

    async def push_outdoor(self, temp_c: float) -> dict[str, Any]:
        return await self._request("POST", "/api/outdoor", {"tempC": temp_c})

    async def push_window(self, zone: int, is_open: bool) -> dict[str, Any]:
        return await self._request(
            "POST", f"/api/zones/{int(zone)}/window", {"open": is_open}
        )

    # --- zone commands ---
    # The daemon picks the member a zone command goes to.

    async def zone_setpoint(
        self,
        zone: int,
        temp_c: float,
        mode: str = "manual",
        duration_sec: int | None = None,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {"setpointC": temp_c, "mode": mode}
        if duration_sec is not None:
            payload["durationSec"] = int(duration_sec)
        return await self._request("POST", f"/api/zones/{int(zone)}/setpoint", payload)

    async def zone_auto(self, zone: int) -> dict[str, Any]:
        return await self._request("POST", f"/api/zones/{int(zone)}/auto")

    async def zone_off(
        self, zone: int, mode: str = "manual", duration_sec: int | None = None
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {"mode": mode}
        if duration_sec is not None:
            payload["durationSec"] = int(duration_sec)
        return await self._request("POST", f"/api/zones/{int(zone)}/off", payload)

    async def zone_boost(self, zone: int, on: bool) -> dict[str, Any]:
        return await self._request("POST", f"/api/zones/{int(zone)}/boost", {"on": on})
