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

SERVICE_BOOST = "boost"
SERVICE_SET_OVERLAY = "set_overlay"
