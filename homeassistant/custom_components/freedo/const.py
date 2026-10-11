"""Constants of the freedo integration."""

from datetime import timedelta

DOMAIN = "freedo"

DEFAULT_PORT = 8080
SCAN_INTERVAL = timedelta(seconds=30)
OUTDOOR_PUSH_INTERVAL = timedelta(minutes=15)
WINDOW_RETRY_INTERVAL = timedelta(seconds=30)

CONF_TOKEN = "token"
CONF_OUTDOOR_ENTITY = "outdoor_entity"
CONF_WINDOW_ENTITIES = "window_entities"
CONF_WINDOWS = "windows"

# A write reaches a device at its next wake-up, so a reported value lags. An entity
# shows the requested value until the daemon reports it, or at most this long.
PENDING_SEC = 15 * 60

SERVICE_BOOST = "boost"
SERVICE_SET_OVERLAY = "set_overlay"
SERVICE_GET_SCHEDULE = "get_schedule"
SERVICE_SET_SCHEDULE = "set_schedule"
SERVICE_COPY_SCHEDULE = "copy_schedule"
