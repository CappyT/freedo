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
# Zones whose external room temperature pushes are paused, because the user chose
# a measuring device of the room in the temperature source select.
CONF_ROOM_TEMP_PAUSED = "room_temperature_paused"

# A write reaches a device at its next wake-up, so a reported value lags. An entity
# shows the requested value until the daemon reports it, or at most this long.
PENDING_SEC = 15 * 60

# The frontend draws the temperature control of a climate or water heater entity
# only while the target is not null. A zone with no target at all shows the
# lowest temperature of its range, and the attribute target_note says so.
NOTE_NO_TARGET = "No target is set. The temperature control shows the lowest temperature as a starting point."
NOTE_REPORTED_TARGET = "The temperature control shows the setpoint that the boiler reports. The daemon sets no target."

SERVICE_BOOST = "boost"
SERVICE_BOOST_ALL = "boost_all"
SERVICE_RESUME_ALL = "resume_all"
SERVICE_SET_OVERLAY = "set_overlay"
SERVICE_SET_WINDOW = "set_window"
SERVICE_GET_SCHEDULE = "get_schedule"
SERVICE_SET_SCHEDULE = "set_schedule"
SERVICE_COPY_SCHEDULE = "copy_schedule"
