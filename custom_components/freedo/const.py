"""Constants of the freedo integration."""

from datetime import timedelta

DOMAIN = "freedo"

DEFAULT_PORT = 8080
SCAN_INTERVAL = timedelta(seconds=30)
OUTDOOR_PUSH_INTERVAL = timedelta(minutes=15)
# Shorter than room_temp_max_age_sec (30 minutes by default), after which the
# daemon drops the reading.
ROOM_TEMP_PUSH_INTERVAL = timedelta(minutes=15)
WINDOW_RETRY_INTERVAL = timedelta(seconds=30)

CONF_TOKEN = "token"
CONF_OUTDOOR_ENTITY = "outdoor_entity"
CONF_WINDOW_ENTITIES = "window_entities"
CONF_WINDOWS = "windows"
CONF_ROOM_TEMPS = "room_temperatures"

# A write reaches a device at its next wake-up, so a reported value lags. An entity
# shows the requested value until the daemon reports it, or at most this long.
PENDING_SEC = 15 * 60

SERVICE_BOOST = "boost"
SERVICE_BOOST_ALL = "boost_all"
SERVICE_RESUME_ALL = "resume_all"
SERVICE_SET_OVERLAY = "set_overlay"
SERVICE_SET_WINDOW = "set_window"
SERVICE_GET_SCHEDULE = "get_schedule"
SERVICE_SET_SCHEDULE = "set_schedule"
SERVICE_COPY_SCHEDULE = "copy_schedule"
