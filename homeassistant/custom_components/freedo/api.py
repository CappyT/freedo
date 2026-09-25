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
