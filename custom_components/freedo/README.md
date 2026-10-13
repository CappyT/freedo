# freedo integration for Home Assistant

This custom integration connects Home Assistant to the local HTTP API of the
freedo daemon. It reads zones, devices and boiler values, sends setpoints, and
pushes Home Assistant sensor states to the daemon as environment inputs.

freedo is experimental. See the [project README](../../README.md) for the status and the
limits. The routes behind each entity are in the [API reference](../../docs/API.md).

## Requirements

- Home Assistant 2026.8 or later (`via_device_id` in the device info).
- A freedo daemon that Home Assistant can reach on `http_listen` (default port
  8080).

## Installation

With HACS:

1. Open HACS, then the menu, then Custom repositories.
2. Enter `https://github.com/CappyT/freedo` and select the type Integration.
3. Download freedo from HACS and restart Home Assistant.

Manually: copy `custom_components/freedo` to `<config>/custom_components/freedo`
and restart Home Assistant.

## Configuration

With the freedo add-on, the Supervisor sends the host, the port and the token of
the add-on to Home Assistant. Settings -> Devices & services then lists freedo
as discovered: select it and confirm. The add-on repeats the message at each
start, so a changed token updates the entry. The add-ons freedo and freedo beta
share the entry: it takes the host and the token of the one that started last.

For a daemon that runs elsewhere, add the integration from Settings -> Devices
& services and enter:

- Host and port of the daemon.
- API token. The integration sends it as `Authorization: Bearer <token>`.
  Leave it empty when the daemon has no token.

The config flow reads `GET /api/home` to check the connection. A 401 answer
shows "invalid auth". A 401 during polling starts the reauthentication flow.

The integration polls the daemon every 30 seconds. One poll reads
`/api/health`, `/api/zones`, `/api/nodes`, `/api/boiler`, `/api/home`,
`/api/install`, `/api/install/zones`, `/api/install/circuits`, `/api/pair/window`
and `/api/presets`.
Entities are created from the zones and devices present at setup. The
integration follows the installation: see Changes of the installation.

The integration and the options of the add-on have English, Italian, German and French translations.

## Entities

| Entity | Per | API source |
|---|---|---|
| climate | heating zone | `/api/zones` demand (4060 temperature, 40a0 demand) and `declared[].target`, `/api/nodes` `TempC`, `/api/install/zones` `setpoint_offset_c`; writes the zone routes |
| water_heater | hot-water zone | `/api/zones` `declared[].target`, `/api/boiler` `DHWC` and `DHWSetpointC`; writes the zone routes |
| binary_sensor Window | heating zone | `/api/zones` `declared[].window` |
| binary_sensor Heating | heating zone | `/api/zones` `demand` `DemandPct` above 0 |
| binary_sensor Fallback | zone | `/api/health` `zoneFallback` |
| binary_sensor Low battery, Fault, Communication warning, Connectivity | device | `/api/health` `devices[]` (`commsWarnings`) |
| binary_sensor Valve warning | valve | `/api/health` `devices[]` `deviceWarnings` |
| binary_sensor Boiler fault | hub | `/api/boiler` `FaultCodes` |
| event Alert | hub | `/api/health` `devices[]` `lowBattery`, `fault`, `deviceWarnings`, `offline`, `/api/boiler` `FaultCodes` |
| sensor Battery, Battery voltage | device | `/api/nodes` `BatteryPct`, `/api/health` `batteryMV` |
| sensor Firmware version, Temperature, Humidity | device | `/api/nodes` `Telemetry` `fw_run_version`, `TempC`, `HumidityPct` |
| sensor Heating demand | heating zone | `/api/zones` `demand` `DemandPct` |
| sensor boiler values | hub | `/api/boiler` `FlowC`, `ReturnC`, `DHWC`, `ModulationPct`, `ChPressureBar` |
| sensor outdoor temperature and age, default flow limit | hub | `/api/boiler` `flowLimit` |
| sensor flow limit served and computed | circuit | `/api/boiler` `flowLimit.circuits[]` |
| sensor Heat output energy, Heat output power | hub | `/api/boiler` `energy` `totalKWh`, `powerKW` |
| sensor Energy estimate | hub | `/api/boiler` `energy` `running`, `reason` |
| sensor Gas volume, Gas flow | hub | `/api/boiler` `energy.gas` `totalM3`, `flowM3H` |
| sensor Gas estimate | hub | `/api/boiler` `energy.gas` `running`, `reason` |
| sensor Gas meter | hub | `/api/boiler` `energy.gas.meter` `value`, `lastReading`, `lastReadingAt`, `readings`, `factor`, `factorReadings`; writes `POST` and `DELETE /api/boiler/gas/meter` |
| number Max power, Min power | hub | `/api/install` `boiler_max_power_kw`, `boiler_min_power_kw`; writes `PATCH /api/install` |
| number Max gas, Min gas | hub | `/api/install` `boiler_max_gas_m3h`, `boiler_min_gas_m3h`; writes `PATCH /api/install` |
| number Flow limit | circuit | `/api/boiler` `flowLimit.circuits[]`, `/api/install/circuits` `heat_source`, `/api/install` `flow_limit_c`; writes `POST /api/boiler/flow`, or `PATCH /api/install` while a curve or the adaptation drives the circuit |
| switch Default weather curve, switch Flow limit adaptation | hub | `GET /api/install` `flow_default_curve`, `flow_auto_adapt`; writes `PATCH /api/install` |
| switch Away | hub | `GET` and `PATCH /api/home` |
| switch Offline schedule | hub | `GET /api/install` `offline_schedule`; writes `PATCH /api/install` |
| switch Pairing mode | hub | `GET /api/pair/window` `open`, `until`, `responder`, `joiners`; writes `POST /api/pair/window` |
| select Display orientation, switch Change notification | valve in a zone | `/api/nodes` `Display`; writes `PUT /api/nodes/{id}/display` |
| switch Child lock | valve in a zone | `/api/nodes` `ChildLock`; writes `POST /api/nodes/{id}/childlock` |
| button Identify | valve or wall thermostat in a zone | `POST /api/nodes/{id}/identify` |
| button Calibrate motor | valve | `POST /api/nodes/{id}/calibrate` |
| sensor Calibration | valve | `/api/nodes` `Telemetry` `calib_state` and `calib_error` |
| switch Early start, switch Open window detection, number Window time | heating zone | `/api/install/zones`; writes `PATCH /api/install/zones/{n}` |
| select Room temperature correction | heating zone | `/api/install/zones` `offset_mode`, `auto_offset`; writes `PATCH /api/install/zones/{n}` |
| number Calibration | heating zone | `/api/install/zones` `auto_offset`, `auto_offset_interval_sec`; writes `PATCH /api/install/zones/{n}` |
| switch Valve calibration (diagnostic, disabled by default) | heating zone | `/api/zones` `declared[].autoOffset`, `/api/install/zones` `auto_offset`; writes `PATCH /api/install/zones/{n}` |
| select Device change end rule, number Change timer | heating zone | `/api/install/zones` `device_overlay_end`, `device_overlay_end_sec`; writes `PATCH /api/install/zones/{n}` |
| number Away | heating zone | `/api/install/zones` `away_temperature_c`; writes `PATCH /api/install/zones/{n}` |
| number Offset | heating zone | `/api/install/zones` `setpoint_offset_c`; writes `POST /api/zones/{n}/offset` |
| select Offset mode (diagnostic, disabled by default) | heating zone | `/api/install/zones` `offset_mode`; writes `PATCH /api/install/zones/{n}` |
| select Temperature source | heating zone | the temperature sensors of Home Assistant, `/api/install/devices`, `/api/zones` `declared[].measuringDevice`, `/api/install/zones` `offset_mode`, `auto_offset`; writes `PUT /api/install/zones/{n}/measuring`, `PATCH /api/install/zones/{n}` and the options |
| sensor Temperature source state | heating zone | `/api/zones` `declared[].externalTemp`, `/api/install/zones` `offset_mode` |
| select Profile | hub | `/api/presets` `id`, `name`, `active`; writes `POST /api/presets/{id}/activate` |

### Profiles

Select Profile lists the program profiles of the daemon (`GET /api/presets`) and
shows the active one. A new install names it `select.freedo_profile`, an older
one keeps `select.freedo_preset`. Choosing an option activates that profile
(`POST /api/presets/{id}/activate`) and polls the daemon again. The options
follow a rename, a new profile and a delete on the next poll, without a reload.
The daemon has no season calendar, so an automation switches the profile by the
season or the weather: see Season profile blueprint. A daemon without the route
(a 0.4.0 daemon answers 404) gets no select: reload the integration after a
daemon upgrade.

### Valve display

- Display orientation is `horizontal` or `vertical` (d/config tag `0149`).
- Change notification makes the valve show a notification when the zone
  changes remotely (d/config tag `0158`).
- Each change writes the whole d/config document to the valve. The valve
  re-reads it and homes its motor (a full stroke), as with the commercial
  bridge.
- Only a valve that a zone declares as a member gets these entities, Child lock
  and Identify: the daemon needs the zone to address the write and answers 400
  `id required` for a valve outside any zone.

### Flow limit

- Number Flow limit sets the heating flow limit of a circuit
  (`POST /api/boiler/flow` with the circuit `id` and its `target`, the wall
  thermostat that drives it). The range is `minC` to `maxC` of the circuit in
  `flowLimit.circuits[]`: 30 to 75 C, with the top cut to the highest CH setpoint
  that the boiler reports. A daemon that sends no `minC` and `maxC` gets 30 to
  75 C. The value is `servedC` of the circuit. The thermostat applies a new
  value by opening its OpenTherm session again, at its next poll of the circuit
  configuration.
- The daemon has no default flow limit. While the option `flow_limit_c` is unset,
  the sensor Default flow limit is unknown, and so is the sensor Flow limit active
  of a circuit that nothing manages. The daemon serves that circuit no limit.
- A circuit that follows a weather curve, the default curve or the adaptation
  takes no direct value: the daemon answers 409 to `POST /api/boiler/flow`. The
  number then shows the manual limit `flow_limit_c` (unknown while it is unset)
  and writes it with `PATCH /api/install`. The curve and the adaptation start
  from this limit. The computed and the served values stay in the sensors Flow
  limit computed and Flow limit active.
- Switch Default weather curve is `flow_default_curve`. It gives each circuit
  without a curve of its own a built-in weather curve under `flow_limit_c`: the
  limit is `flow_limit_c` minus a reduction of 0 to 8 C that grows with the
  outdoor temperature. The daemon refuses the switch while `flow_limit_c` is
  unset, so set the number Flow limit first. The curve needs the outdoor
  temperature, so select an outdoor temperature sensor in the options of the
  integration. The curve is a fit on one plant and one season of monthly means
  and is off by default. The details are in the
  [API reference](../../docs/API.md#flow-limit).
- Switch Flow limit adaptation is `flow_auto_adapt`. It trims the limit of each
  circuit from the room feedback, below the manual limit or the curve.
- The attributes `default_curve` and `reduction_c` of the sensors Flow limit
  computed and Flow limit active show that a circuit follows the default curve
  and the reduction in force now.
- The number exists for a circuit that the boiler view lists and the
  installation gives a heat source.

### Boiler energy

The daemon estimates the heat that the boiler delivers from the burner flag, the modulation and the
boiler power of the installation. The counter is heat output, not gas energy, because the daemon
does not model the efficiency. The model and its error sources are in the
[API reference](../../docs/API.md#energy-and-gas-estimate).

- Sensor Heat output energy is the counter in kWh (`totalKWh`). It has device
  class energy and state class total increasing, which the Energy dashboard
  needs. The counter never resets.
- Sensor Heat output power is the output now in kW (`powerKW`), with device
  class power. It is unknown while the estimate does not run.
- Both sensors need a maximum power. They do not exist while the installation
  holds none, and they appear on the poll after you set it.
- Sensor Energy estimate is diagnostic. Its state is `running`, or the reason
  the estimate does not run: `no_max_power` (no maximum power set), `no_report`
  (the thermostat sent no report since the daemon started), `stale` (the last
  report is older than the hold time of the daemon, 5 minutes), `no_burner_flag`
  and `no_modulation` (the boiler did not report that value). A reason of a
  newer daemon shows as unknown.
- Numbers Max power (0.1 to 500 kW) and Min power (0 to
  500 kW) are the output of the boiler at 100 % and at 0 % modulation. Each
  change is a `PATCH /api/install` with `boiler_max_power_kw` or
  `boiler_min_power_kw`. The daemon needs the minimum below the maximum and
  answers 400 otherwise, so raise the maximum first. The numbers send no null:
  to clear a value, send `{"boiler_max_power_kw": null}` to `PATCH /api/install`.
  The numbers exist while the daemon holds an installation.
- A daemon older than the estimate sends no `energy` object and gets none of
  these sensors. Reload the integration after a daemon upgrade.

### Boiler gas

The daemon counts the fuel the boiler burns from the same burner flag and modulation, and from the
fuel flow of the boiler at its maximum and minimum. The counter is a fuel quantity, so it needs no
efficiency.

The install option `fuel_unit` sets the unit: `m3` (natural gas, the default), `l` (heating oil) or
`kg` (LPG or pellets). Write it with `PATCH /api/install`. The integration reads it from
`/api/install`, else from `energy.gas.unit`, else it takes `m3`. The names of the API fields keep
`m3` (`totalM3`, `flowM3H`, `boiler_max_gas_m3h`), and they hold the chosen unit. The unit sets the
class and the unit of the sensors and numbers:

| `fuel_unit` | Gas volume | Gas flow, Max gas and Min gas |
|---|---|---|
| `m3` | device class gas, m³ | device class volume flow rate, m³/h |
| `l` | device class volume, L | device class volume flow rate, L/h |
| `kg` | device class weight, kg | no device class, kg/h |

The Energy dashboard takes a gas source with the device class gas or energy only, so only the unit
`m3` fits it. The daemon does not convert a stored counter when the unit changes: set the unit before
the daemon counts.

- Sensor Gas volume is the counter in the unit of `fuel_unit` (`totalM3`), with
  state class total increasing. The counter never resets.
- Sensor Gas flow is the flow now in the unit of `fuel_unit` per hour (`flowM3H`),
  with state class measurement. It is unknown while the estimate does not run.
- Both sensors need both flows. They do not exist while the installation lacks
  one, and they appear on the poll after you set the second.
- Sensor Gas estimate is diagnostic. Its state is `running`, or the reason the
  estimate does not run: `no_gas_flow` (a gas flow is not set), `no_report`,
  `stale`, `no_burner_flag` and `no_modulation`, as for the Energy estimate. It
  exists from the first poll that carries the gas count, so it shows
  `no_gas_flow` before you set the flows. A reason of a newer daemon shows as
  unknown.
- Numbers Max gas (0.01 to 60) and Min gas
  (0 to 60), in the unit of `fuel_unit` per hour, are the fuel flow at 100 % and
  at 0 % modulation. Each change is a
  `PATCH /api/install` with `boiler_max_gas_m3h` or `boiler_min_gas_m3h`. The
  daemon needs the minimum below the maximum and answers 400 otherwise, so set
  the maximum first. The gas minimum has no default: the estimate needs both
  numbers. To clear a value, send `{"boiler_max_gas_m3h": null}` to
  `PATCH /api/install`. The numbers exist while the daemon holds an installation
  and serves the gas options.
- A daemon older than the gas count sends no `energy.gas` and gets none of these
  entities. Reload the integration after a daemon upgrade.

Fill the two flows from the data sheet of the boiler:

- Find the fuel flow at the burner at the maximum and at the minimum heat input,
  in the unit of `fuel_unit` per hour. Enter the maximum in Max gas
  and the minimum in Min gas.
- Example, a 24 kW condensing combi boiler on natural gas: maximum 2.63 m³/h and
  minimum 0.30 m³/h. These are the numbers of one data sheet, not a default for
  your boiler.

Add the gas to the Energy dashboard so that it shows the cost. This needs the unit
`m3`:

1. Open Settings -> Dashboards -> Energy.
2. In Gas consumption, add a gas source and select the sensor Gas volume.
3. Enter the gas price per m³: a fixed price, or an entity that holds the price.
   The dashboard then shows the cost.

Error sources: a hot water burn counts on the heating scale and reads low. Check
the count with a calibration: read the gas meter at two moments and compare its
difference with the difference of Gas volume over the same period.

### Gas meter

The user reads the gas meter now and then. Record a reading with the service
`freedo.calibrate_gas_meter`. The field `value` is the meter reading, `estimate`
the fuel counter when the reading was taken (for a reading taken earlier, else
the current counter) and `at` the time it was taken (else now). The field
`replaced` says the physical meter was replaced: the daemon drops the stored
readings and starts the new meter from this reading. The daemon refuses a
reading below the previous one and an estimate outside its range with `400`, and
answers `503` while the installation holds no gas flows. The service
`freedo.undo_gas_meter_reading` removes the last reading, to undo a typo. With
several hubs loaded, both services need the field `config_entry_id` to name the
hub, so both do not record the same reading.

- Sensor Gas meter is the meter value the daemon estimates between the readings,
  in the unit of `fuel_unit`. It is unavailable while no reading is stored. The
  attributes are `last_reading`, `last_reading_at`, `readings` (the count kept,
  at most 20), `factor` and `factor_readings`.
- Its state class is total increasing. HA treats a reading below 0.9 of the
  previous value as a meter reset, which is right after a replacement, and a
  small dip after a reading, because `totalM3` is an estimate and the meter also
  counts other users of the same gas such as a hob, only gives a negative delta.
- The daemon corrects the meter value with a factor it learns from the two most
  recent readings whose estimate is at least one unit apart. Until then the
  factor is `1`, so `factor_readings` is 0 and the meter follows the fuel
  counter with the ratio 1. The factor corrects the meter value only: `totalM3`
  and `flowM3H` stay the raw estimate.

### Diagnostics

- Sensor Firmware version is the running firmware, `major.minor`, from the
  `fw/state` the device reports. It is unknown until the device reports one.
- Sensors Temperature and Humidity are the readings of one device. They are
  diagnostic and disabled by default, because the climate entity shows the zone
  values. Temperature is on the wire scale: it does not include the zone
  correction of offset mode `setpoint`. A device gets Humidity once it has
  reported a humidity; the sensor appears on the poll after that report.
- Sensor Heating demand is the heat demand of a heating zone, 0 to 100 %
  (tag `40a0`). It is unknown until the zone reports.
- Binary sensor Heating is on while the demand of a heating zone is above 0. It
  is unknown until the zone reports. The attribute `hvac_action` of the climate
  entity follows the same rule. Reports count the state of this sensor: see
  Reports.
- Sensor Temperature source state shows whether a heating zone regulates on
  the reading of a Home Assistant sensor. The state is `active`, or the cause:
  `no_correction` (select Room temperature correction is None), `no_reading`
  (the daemon holds no reading) or `stale` (the
  last reading is older than `room_temp_max_age_sec`). It is unknown while the
  daemon serves no offset mode. It is an enum sensor with short translated
  states. The attributes are `correction` (`none`, `continuous_shift` or
  `valve_calibration`), `offset_mode`, `source`, `temperature_c` and `age_sec`
  (the reading), `device_temperature_c` (the
  temperature of the valve), `shift_c` (the shift applied to the setpoints) and
  `wanted_shift_c` (the shift it moves toward). The sensor needs a daemon that
  serves `externalTemp` in `GET /api/zones`.
- Binary sensor Communication warning is on while the device shows a radio link
  warning. The attribute `warnings` lists them: `noNeighbour` and
  `noConfigAnswer`. These are the link bits of the fault word. They clear
  when the link recovers and are not a Fault. A daemon that sends no
  `commsWarnings` gets no sensor.
- Binary sensor Fault is on while the device reports a fault. The attribute
  `faults` lists the names the daemon gives, for example `stall`, and
  `fault_word` holds the raw word. `docs/API.md` lists the names.
- Binary sensor Valve warning is on while the valve reports a state that is no
  fault. The attribute `warnings` lists it: `notMounted` (the valve is off its
  radiator), `calibrationNoPinContact`, `shortTravel`, `lowVoltage` or
  `recalibrating`. Such a state leaves Fault off. A daemon that sends no
  `deviceWarnings` gets no sensor.

### Valve controls

- Switch Child lock locks the controls of the valve. The valve takes the write
  at its next wake-up and the daemon reports the new state when the valve
  confirms it. Until then the entity shows the requested state, for at most
  15 minutes. Only a valve has a child lock: the node view reports `ChildLock`
  as null on other devices, and they get no switch. A valve that belongs to no
  zone gets no switch either.
- Button Identify makes the valve or the wall thermostat signal itself; the thermostat shows "Hi!". Button Calibrate motor starts
  the motor calibration, a full stroke of the valve. A 2.xx answer of the valve
  does not guarantee the stroke: the valve drops the request in silence in some
  states.
- Sensor Calibration is the result of the last calibration: `none`, `started`,
  `done` or `failed`, from the `d/<serial>/mnt` report. The attribute
  `error_code` is the error code of the valve. The state is unknown until the
  valve reports a calibration.
- Number Offset corrects the temperature of a heating zone, -10 to
  10 C (`POST /api/zones/{n}/offset`). The zone gets it when it has a measuring
  device, or when its offset mode is `setpoint`.
  - Offset mode `setpoint` is applied by the daemon, and the daemon reports the
    value.
  - Offset modes `config` and `dbg2` write the measuring device, and the daemon
    reports no value: no captured device frame carries tag `0140`. The entity
    shows the last value set from Home Assistant and restores it after a
    restart. It is unknown until the first write, and a value set outside Home
    Assistant does not show. In mode `config` each change makes the valve home
    its motor.

### Zone settings

The installation row of each heating zone (`GET /api/install/zones`) gives
these entities. Each change is a `PATCH /api/install/zones/{n}`, which the daemon
stores in its database and applies at once. The entities show the row the
daemon answers. They exist only while the daemon holds an installation.

- Switch Early start lets the daemon start a warmer program block early, from
  the heat-up model it learns for the zone.
- Switch Open window detection turns the open window detection of the valves of
  the zone on or off (`open_window_detection`).
- Number Window time is the time the zone stays open after a
  detection (`open_window_duration_sec`), in minutes, 1 to 1092.
- Select Room temperature correction is how the zone corrects its regulation
  with the room temperature of a Home Assistant sensor. See Room temperature of
  a zone.
  - None (`none`): the zone regulates on its own sensor. The daemon refuses a
    room temperature reading.
  - Continuous shift (`continuous_shift`, offset mode `setpoint`): the daemon
    shifts the setpoints slowly from the reading and writes nothing to the
    device. This is the recommended method.
  - Valve calibration (`valve_calibration`, `auto_offset` on in offset mode
    `config`): the daemon writes the valve offset from the reading, at most once
    per Number Calibration. Each write makes the valve home
    its motor.
  - The select writes `offset_mode` and `auto_offset` in one
    `PATCH /api/install/zones/{n}`. The daemon checks the two fields against the
    values the request leaves, so the move between the shift and the calibration
    passes, while the daemon refuses `setpoint` with `auto_offset` on.
    Leaving the shift sets the offset mode `config`, the default.
- Switch Valve calibration (`auto_offset`) is the calibration alone. It is a
  diagnostic entity, disabled on a new install: the select replaces it. An
  install that had it keeps it enabled for its automations. It applies in the
  offset modes `config` and `dbg2`. The daemon refuses it on the hot water zone
  and in mode `setpoint`. Its attributes
  `last_write_at`, `next_at`, `filtered_diff_c`, `sample_span_sec` and
  `stop_reason` are the `autoOffset` view of `GET /api/zones`. `stop_reason`
  says why the daemon stopped the automatic writes of the zone. Switch the
  correction off and on to start them again. See Room temperature of a zone.
- Number Calibration is that interval
  (`auto_offset_interval_sec`), in hours, 1 to 168. It is unavailable unless
  the correction is Valve calibration.
- Select Device change end rule is how long a change made on a device lasts, as
  the rule `device_overlay_end`: `manual` until the next command, `until_next`
  until the next block of the weekly program, `timer` for a set time. The rule
  does not travel on the air: the daemon applies it and returns the zone to its
  program. Rule `timer` starts at 60 minutes.
- Number Change timer is the time of the rule `timer`
  (`device_overlay_end_sec`), in minutes, 1 to 10080. It is unavailable while
  the rule is not `timer`.
- Number Away is the temperature the zone keeps while switch Away
  is on (`away_temperature_c`), 5 to 25 C in steps of 0.1 C, as the climate
  entity. 5 C means off in away, as "off in away" in the tado app. A zone with
  no value is off in away too, so the number shows 5 C until you set one. The
  entity needs a daemon that serves `away_temperature_c` in
  `GET /api/install/zones`. The hot water zone has no away temperature and
  gets no number.
- Select Offset mode is how the temperature correction of the zone
  reaches it (`offset_mode`). It is a diagnostic entity, disabled on a new
  install: select Room temperature correction replaces it. An install that had
  it keeps it enabled. The hot water zone has no correction and gets no select.
  - Valve configuration (`config`, the default) writes tag `0140` in the whole
    d/config document of the measuring device. The valve rereads its
    configuration and homes its motor, a full stroke, on each change of the
    number Offset. The wall thermostat ignores the document of a
    d/config write, so the daemon serves it a new document and the thermostat
    rereads it.
  - Valve debug record (`dbg2`) writes the single record `0140` with
    `PUT d/dbg2/tlvs`. The valve does not home its motor. No capture shows this
    write on air. The wall thermostat has no such resource: the daemon accepts
    the mode for its zone and refuses the offset write with 400.
  - Setpoint shift (`setpoint`) writes nothing to the device. The daemon keeps
    the offset, subtracts it from each setpoint it writes to the zone, and adds
    it to the temperature and the target it reports. The valve keeps the
    `0140` it holds, and the shift adds to it. This is the correction Continuous
    shift.
  - A change of the mode writes no offset to the device. Leaving `setpoint`
    drops the stored offset, rewrites the zone without the shift (the valves get
    new setpoints) and ends the room temperature reading of the zone. Entering
    `setpoint` starts the offset at 0 and leaves the `0140` on the valve. To
    clear a `0140` written earlier, set the number Offset to 0
    before the change.
  - A zone without a measuring device gets the number Offset only in
    mode `setpoint`: reload the integration after the change.
  - The number Offset is unavailable while the correction is Valve
    calibration, which writes the valve offset itself. In the modes `config`
    and `dbg2` it shows the last value set from Home Assistant, and it is empty
    until the first write: the valve does not report its offset.
  - The daemon answers 400 to a mode other than the three, 404 to an unknown
    zone, 503 while it holds no installation, and 500 when the running daemon
    does not take the saved change. The select shows the answer as an error and
    keeps the mode.

### Temperature source

Select Temperature source chooses where the room temperature of a heating zone
comes from: a device of the room or a Home Assistant sensor. Every heating zone
has it. The page of the room in Configure shows the same list and the same
current source, and both write the same state: see Rooms.

- The options come in this order:
  - The declared members of the zone (`members` of `GET /api/zones`), labelled
    with the device label and the serial from `GET /api/install/devices`. A
    member whose `zone` field names another room is left out. The device the
    zone measures on is always an option.
  - The sensors with the device class temperature in the area of the zone
    device, labelled with their name and reading. A sensor that is the only
    temperature sensor of its device shows the device name, which is shorter
    than the entity name. A name that you gave to the entity stays. A sensor counts in the area
    when the entity or its device is there. The entities of freedo are left out.
    The sensor mapped to the zone is always an option, also without the device
    class.
  - The other temperature sensors, labelled with their name, reading and area.
- The labels are built at each poll and kept until the next one, so the label
  of a sensor shows the reading of the last poll. A label that repeats gets the
  short address or the entity id.
- The current option is the mapped sensor while its pushes are not paused and
  the zone has a correction (Continuous shift or Valve calibration); otherwise
  it is the effective measuring device (`measuringDevice` of `GET /api/zones`).
- Choosing a sensor maps it to the zone in the options, resumes the pushes and
  sends the current reading at once. A zone with the correction None gets
  Continuous shift. A zone with Valve calibration keeps it.
- Choosing a device writes `PUT /api/install/zones/{n}/measuring` with the
  device reference. The measuring device reaches the valves in the next zone
  document, at their next hourly check. When a sensor is mapped to the zone, the
  choice also pauses its pushes and sends `DELETE /api/zones/{n}/temperature`, so
  the daemon returns to the manual correction. The release starts at once. If
  the call fails, the age limit of the reading ends the correction. The
  correction method does not change.
- The mapping and the pause are stored in the options, so they survive a
  restart. Neither choice reloads the integration.

### Pairing mode

- Switch Pairing mode opens and closes the pair window of the bridge, the
  window in which the daemon answers a device that wants to join. On sends
  `POST /api/pair/window` with `{"seconds": 600}`, the longest window the
  daemon allows. Off sends `{"seconds": 0}`, which closes the window at once.
  The window also closes by itself after 600 seconds.
- The state is `open` of `GET /api/pair/window`. The switch follows the poll:
  a window opened or closed outside Home Assistant shows within 30 seconds, and
  so does the end of a window. The attribute `until` is the end time of an open
  window.
- The attribute `joiners` lists the EUI of each device that waits for the pair
  command, and `responder` says that the bridge answers the beacon requests by
  itself.
- The switch is a config entity of the hub device. A daemon without
  `GET /api/pair/window` gets no switch.
- Service `freedo.pair` sends the pair command to a device that freedo holds
  (`POST /api/pair/devices/{ref}`, under the network key of the installation).
  The field `device` takes the EUI or the short address. Without it, the
  service pairs the one device that waits in the pair window, and it refuses
  when none or several wait. A device can miss the command: call the service
  again while the window is open. With several hubs, the field
  `config_entry_id` names the hub.
- The integration does not add a device to the installation. The freedo
  dashboard does the whole pairing on one page: it opens the window, lists the
  waiting devices, adds the device with its room, sends the command and
  reports when the device joined. The hub device links to the dashboard
  (Visit, on the device page): the ingress page of the add-on, or
  `http://<host>:<port>/` for a daemon that runs elsewhere.

### Changes of the installation

The integration builds its entities from the rooms and the devices of the
installation. It compares them at each poll: the rooms with their members, and
the devices with their kind, label and serial.

- When they change, the integration reloads itself. So a room or a device that
  you add, remove, rename or move in the dashboard shows in Home Assistant
  within one poll, 30 seconds, with no reload by hand.
- The revision of the installation (`rev`) does not decide: it also moves at
  each option write, such as the area the integration sends.
- After the reload, the integration removes the Home Assistant devices, with
  their entities, of the rooms and devices that left. A device that stays keeps
  its area, its names and its entity ids. A poll that shows no room or no
  declared device removes nothing.
- An entity of a device that stays, but that the installation no longer gives,
  stays in Home Assistant as unavailable. Delete it on its page.

### Boiler fault and alerts

- Binary sensor Boiler fault is on while the last report of the boiler holds a
  fault code (`FaultCodes` of `GET /api/boiler`), and unknown before a report.
  The attribute `codes` lists the codes. They are the codes of the boiler
  maker: the manual of the boiler gives their meaning, and freedo gives none.
- Event Alert, on the hub device, raises one event when an alert starts:
  `low_battery`, `device_fault`, `valve_warning`, `device_offline` or
  `boiler_fault`. The event attributes are `room`, `device` and `details` (the
  fault names, the warning names or the boiler codes).
- The alerts that hold when the integration starts raise no event, and an alert
  raises no second event while it holds. So a restart or a reload repeats
  nothing. The alerts of one poll go out one second apart, because a trigger on
  an event entity fires when the time of its last event changes.
- The binary sensors show the same alerts as states. The notification blueprint
  sends the events on: see Notification blueprint.

### Offline schedule

- Switch Offline schedule is the installation option `offline_schedule`:
  d/config tag `02b3` of each device without a per-device value.
- On, each device fetches the weekly program of its zone and runs it after
  25 minutes without an answer from the coordinator. After a schedule change
  the daemon makes each member fetch the new program at once.
- Off, no device fetches the program, as with the commercial coordinator.
- A change reaches each device in its d/config document. A per-device value
  (`POST /api/nodes/{id}/config` `offlineScheduleEnabled`) outranks it.
- The state is unknown while the daemon holds no installation.

### Climate

- The target temperature, the mode and the boost state come from
  `declared[].target` of `/api/zones`, which the daemon derives from the zone
  block it holds.
- The current temperature is tag 4060 of the zone report. Until the zone
  reports 4060, it is the 012d reading of the first zone device that has one:
  the measuring device, the demand reporter, then the members. With no reading
  it is unknown.
- A zone with offset mode `setpoint` shows its temperatures and targets on the
  corrected scale, reading + `setpoint_offset_c`, as the daemon reports them.
  The integration adds the offset to a device reading.
- HVAC mode `auto` returns the zone to its program (`POST /api/zones/{n}/auto`).
  HVAC mode `heat` sets a manual overlay at the current target, else at the
  program setpoint. HVAC mode `off` sets a manual off overlay
  (`POST /api/zones/{n}/off`). Turn on is HVAC mode `auto`.
- A zone with no target and no program setpoint, as a zone with no program is
  (`has_program` is false and `program_temperature` is empty), has nothing to
  show as a target.
  The frontend draws the temperature control only for a target that is not
  empty. So the entity shows the lowest temperature of its range, 5 C, as a
  starting position, and the attribute `target_note` says that no target is set.
  That value is a placeholder: the integration never sends it to the daemon.
  HVAC mode `heat` in this state sends nothing and raises an error that tells
  you to choose a temperature. A temperature chosen on the control is written
  as a manual overlay and selects `heat`. A zone that has a program setpoint
  keeps the control hidden while its target is empty (off, open window, away),
  as before.
- A new target temperature sends `POST /api/zones/{n}/setpoint` with
  `{"setpointC": t, "mode": "manual"}`. The daemon writes the zone to its
  demand reporter, as the commercial coordinator does. A target outside
  5..25 C is clamped to that range, which the daemon accepts on the corrected
  scale.
- `climate.set_temperature` with `hvac_mode` `auto` or `off` sets that mode
  and does not apply the temperature: the program sets the target on `auto`,
  and `off` has none. With `hvac_mode` `heat` it sets the manual overlay.
- A device applies a write at its next wake-up. The entity shows the requested
  value until the daemon reports it, for at most 15 minutes. A change to
  `auto` or `off` or a boost drops the requested target.
- Preset `boost` starts the boost (`POST /api/zones/{n}/boost`), preset `none`
  cancels it. The service `freedo.boost` (field `on`, default true) does the
  same.
- Service `freedo.set_overlay` heats the zone to `temperature` (5..25 C) or
  switches it off (`hvac_mode` `off`, no temperature), and sets when the change
  ends with `end_rule`:
  - `manual` (default) lasts until the next command.
  - `timer` lasts for `duration`, 1 second to 7 days. It needs `duration`.
  - `until_next` lasts until the next block of the weekly program. Without a
    program the overlay does not end.
  - `until_auto_change` lasts until the next block of the weekly program or the
    next switch between home and away (switch Away), whichever comes first. It
    is the rule "until the next automatic change" of the tado app. Without a
    program it has no deadline and ends at the next home or away change.
  The service refuses a missing or surplus `temperature` or `duration` before
  it calls the daemon. `duration` is allowed with `timer` only. It sends
  `POST /api/zones/{n}/setpoint` or `POST /api/zones/{n}/off` with `mode` and
  `durationSec`.
- Service `freedo.set_window` (field `open`, default true) activates or resets
  the open window state of the zone by hand, as the tado app does
  (`POST /api/zones/{n}/window` with `{"open": true}`, or
  `{"open": false, "reset": true}` for a reset).
  - An activation is the same hold as the one the window sensors of a room set: the
    daemon keeps one hold from Home Assistant for each zone, and the last write
    wins. See Window and door sensors of a room.
  - An activated state ends at the reset, or when the daemon ends it after
    `external_window_max_sec` (2 hours by default, an option of the
    installation). An activation of an active state restarts that time.
  - A reset ends every hold of the zone: the one from Home Assistant and a
    state the valves detected (`window_source` `device` or `both`), before its
    time ends (number Window time). The room heats again at once. A new
    detection by a valve opens the state again.
- Attributes: the window state (`window_open`, `window_source`,
  `window_since`, `window_deadline`), demand, fallback, overlay, its end rule
  (`overlay_rule`) and its end (`overlay_ends_at`), `target_source`,
  `target_note`, `has_program`, `program_temperature` and members.
  - `overlay` is the kind of the overlay: `auto` (no overlay), `manual`,
    `until_next`, `until_auto_change` or `boost`. The daemon reports a timer as
    `until_next`.
  - `overlay_rule` is the end rule the overlay runs under: `manual`, `timer`,
    `until_next` or `until_auto_change`. It is empty without an overlay. A daemon
    that sends no rule gets the kind of a `manual` or `until_next` overlay here,
    and an empty value for any other kind.
  - `overlay_ends_at` is the instant the daemon ends the overlay. It is empty
    without a deadline: a manual overlay, and `until_auto_change` without a
    program. The remaining time is `overlay_ends_at` minus now.
  - `has_program` is true while a weekly program of the daemon is in force for
    the zone (`declared[].target.hasProgram`): in away, the away program of a
    zone that has one, else the home program. It is false for a zone with no
    program in force,
    and empty on a daemon that does not report it. `target_source` reads
    `program` in both cases. `program_temperature` of a zone with no program is
    the `6200` of the zone cache, empty when the cache holds none.
  - `target_note` is empty while the target is a value of the daemon or the
    program. It holds a sentence when the entity shows a placeholder: see the
    paragraph about a zone with no target.

### Weekly schedule

Three services on a climate entity or on the water heater entity read and write
the weekly schedule of the zone (`GET` and `PUT /api/zones/{n}/schedule`). A
schedule is a list of programs:

```yaml
programs:
  - days: [mon, tue, wed, thu, fri]
    entries:
      - {time: "00:00", temperature: 17}
      - {time: "06:30", temperature: 21}
      - {time: "22:00", temperature: 17, enabled: false}
  - days: [sat, sun]
    entries:
      - {time: "00:00", temperature: 18}
```

- `freedo.get_schedule` returns `{zone, programs}` for each target entity, in the
  form above with `enabled` on every entry. A zone with no schedule has no
  programs: the daemon answers 404 `zone N has no schedule`, which is the only
  404 read as no schedule. Any other error, such as an unknown zone, is raised.
  The service has a response only: call it with `response_variable`.
- `freedo.set_schedule` (field `programs`) replaces the schedule. The daemon
  writes it to each member of a heating zone.
- `freedo.copy_schedule` (field `to`, freedo climate or water heater entities
  of the same zone kind) reads the schedule of the target entity and writes it
  to each entity in `to`. The daemon has no copy route, so the service does
  both calls. A zone the daemon refuses does not stop the others: the service
  writes every zone and raises one error that lists the refused ones.
- The services check the schedule before they call the daemon, with the rules the
  daemon enforces: the programs cover the seven days once, the first entry of a
  program is at `00:00`, the times of a program increase, a time is a whole
  minute `HH:MM`, a temperature is within 5..25 C (30..65 C for the hot water
  zone) with at most two decimals, and a program encodes to at most 255 bytes,
  as the device stores a day. An entry
  takes 9 bytes before 02:08 and 10 bytes after, and the program takes 2 more,
  so a program holds 25 to 28 entries. `enabled: false` switches the zone off
  from that time.
- The same services work on the water heater entity, with temperatures within
  30..65 C. The daemon keeps the hot-water program itself and writes each
  block change to the thermostat. It holds no program until you set one:
  `freedo.get_schedule` returns no programs then. `enabled: false` turns the
  hot water off from that time.
- A call with several targets checks the schedule against each target before it
  writes any. A room zone and the hot water zone differ in range, so a schedule
  for one is refused for the other.
- `freedo.copy_schedule` between a room zone and the hot water zone is refused
  with an error before any call to the daemon, because their temperature
  ranges differ. There is one hot water zone: copying from it to itself is
  refused too.

### Water heater

- Operation `auto` follows the hot-water program (`POST /api/zones/{n}/auto`),
  `heat` sets an overlay at the target (`POST /api/zones/{n}/setpoint`), `off`
  turns the hot water off (`POST /api/zones/{n}/off`).
- The target and the operation come from `declared[].target` of `/api/zones`.
  The current temperature is the boiler DHW temperature.
- The target shown is, in this order: the target of the daemon, the program
  temperature, the hot water setpoint that the wall thermostat reports
  (`DHWSetpointC` of `/api/boiler`, attribute `reported_setpoint`). The reported
  setpoint counts only while the zone is on and when it lies in 30..65 C. The
  daemon does not set it. In this case `target_note` says that the boiler
  reports the value.
- A hot water zone with no weekly program and no override has none of the three
  values (`has_program` is false). The frontend draws the temperature control
  only for a target that is not empty, so the entity shows 30 C, the lowest
  temperature of the range, as a starting position, and `target_note` says that
  no target is set. That value is a placeholder: the integration never sends it
  to the daemon. Operation `heat` in this state sends nothing and raises an error
  that tells you to choose a temperature. A temperature chosen on the control is
  written as a manual overlay and selects `heat`. To keep 30 C itself, move the
  control away and back, or use the action `water_heater.set_temperature`.
- The attributes `overlay`, `overlay_rule` and `overlay_ends_at` are the ones of
  the climate entity, with `has_program` and `target_note`. The attribute
  `program_temperature` is the setpoint of the program block, and
  `reported_setpoint` the one the boiler reports. The water heater has no
  service to set an end rule.
- The hot water follows the home and away state of switch Away, as in the
  commercial cloud. On its program, the hot water turns off while the home is
  away and resumes the current program block when the home returns. The
  operation stays `auto` and `declared[].target.on` is false. A permanent
  manual overlay keeps the hot water on while the home is away.

### Home services

Two services act on the whole home. They belong to the integration, so they
take no target.

- `freedo.boost_all` (field `on`, default true) starts or stops the boost of
  every heating zone, as the service `freedo.boost` does for one zone.
- `freedo.resume_all` returns every heating zone and the hot water to their
  weekly schedule (`POST /api/zones/{n}/auto` for each zone), and drops a
  target or mode that an entity still shows as requested.
- Both act on the zones that have an enabled climate or water heater entity. The
  daemon has no bulk route, so a service sends one command for each zone. A zone
  the daemon refuses does not stop the others: the service raises one error
  that lists them.

## Configure

Configure of the integration opens a menu of two branches.

- Rooms lists the heating rooms and opens the page of one: see Rooms.
- Outdoor temperature maps the outdoor temperature sensor. The integration
  sends `POST /api/outdoor` with `{"tempC": n}` on each state change and every
  15 minutes. The daemon keeps the reading in memory only, so the periodic push
  also covers a daemon restart. A value in degrees Fahrenheit or kelvin is
  converted. A state that is `unavailable`, `unknown` or not a number is not
  sent. A value outside -60..60 C is not sent, because the daemon rejects it.
  Saving a changed sensor reloads the integration.

### Rooms

Each heating zone is a room. The branch Rooms holds everything that ties a room
to Home Assistant: its area, its temperature source, its window and door
sensors and its correction.

- The branch starts with a table of the heating rooms: the room name, its
  area, its temperature source with the reading, and the number of its window
  sensors. Choose a room below it to open its page, or Done to close. The
  branch needs the loaded integration.
- A flow does not get the language of the user who opened it: the frontend
  translates the form. So the texts that the integration builds at run time
  hold names and numbers only. The words are in the translated description,
  and the choice Done is a selector translation.
- The page of a room holds four settings:
  - Area: the Home Assistant area of the room. See Area of a room.
  - Temperature source: a device of the room or a Home Assistant temperature
    sensor. The list and the current source are those of select Temperature
    source: see Temperature source. The field shows the source in use and is
    required when the room has one. The list is in the order of the area the
    room has when the page opens: after a change of the area, submit and open
    the room again to see the sensors of the new area first.
  - Window and door sensors: see Window and door sensors of a room.
  - Correction, in the closed section Advanced: the choice of select Room
    temperature correction. A Home Assistant sensor chosen as the source of a
    room with the correction None sets Continuous shift.
- Submit writes the room and returns to the list, which shows the new values.
  A page writes only what it changes, and only its room. It does not reload the
  integration. When freedo refuses the correction or the source, the page comes
  back with an error and writes nothing else.
- A sensor that is mapped to a room but is not in use, because the room has the
  correction None, still sends readings. A page saved with a device as the
  source pauses it, so a correction set later does not bring it back unseen.

### Area of a room

The Home Assistant area of a zone is the area of its zone device in the device
registry. The page of the room sets it, and so does the device page of the
zone. The integration reads it at each poll.

- A zone with no area starts, on its page, from the area with the zone name,
  without case.
- When the area of a zone changes, its valves and thermostats take the new
  area if they have no area or still have the previous area of the zone. A
  device that you placed in another area keeps it. A device that appears later,
  such as a new pairing after the reload, takes the area of its zone when it has
  none. Otherwise nothing moves, so a device that you clear to no area stays so.
  A restart compares the area with the label the daemon stores. A wall thermostat that serves a heating zone and the hot water
  takes the area of the heating zone. The area of the hot water zone device
  applies to its other devices.
- The integration sets no `suggested_area`: it applies to new devices only and
  creates an area for each zone name.
- A zone renamed in the daemon renames its zone device at the next poll, unless
  you named the device in Home Assistant. The entity ids do not change. The hot
  water zone device keeps its translated name.
- A heating zone with no area works as before. Home Assistant shows one repair
  issue, "Rooms without an area", that lists those rooms. The list follows the
  areas at each poll, and the issue goes when every room has an area. You can
  ignore it, and it stays ignored when the list changes.
- The integration sends the area to the daemon as a display label:
  `PATCH /api/install/zones/{n}` with `ha_area: {"id", "name"}`, also after an
  area rename. The freedo panel shows it, and the daemon never acts on it. The
  stored label is also the previous area of the zone after a restart. A daemon
  that does not show `ha_area` in `GET /api/install/zones` gets no label.

### Window and door sensors of a room

The page of a room maps binary sensors to it. The heating of the room stops
while one of them is open.

- The list shows the binary sensors with the device class window, door, opening
  or garage door, by name, with the rule of the temperature sensors for the
  device name: those in the area of the room first, then the others
  with their area. A sensor that is already mapped stays in the list without
  such a device class. With no such sensor at all, the field is an entity
  selector.
- A sensor serves one room. A sensor of another room shows that room in square
  brackets, and it moves to the room that chooses it.
- On each change of the state of a room, the integration sends
  `POST /api/zones/{n}/window` with `{"open": bool}`, without `reset`: a sensor
  that closes does not end a state the valves detected. A room is open when one
  of its sensors is on. The pushes of one room go one at a time, and each
  sends the sensor states of its send time, so the daemon ends on the last
  state. The integration also sends the state of a room when it starts and when
  the page changes its sensors. A room that loses its sensors while they hold
  it open is closed. A state that is `unavailable` or `unknown` does not count.
- When the daemon does not answer or answers 5xx, the integration sends the
  current state of the room again every 30 seconds until the daemon takes it. A
  4xx answer is not sent again.
- The daemon ends a hold that no close follows at its `deadline`
  (`external_window_max_sec`, 2 hours by default). This guards a room against a
  Home Assistant that stopped. While a sensor keeps the room open, the
  integration compares the room with `declared[].window` of each poll. It sends
  the open again once when the deadline is closer than 5 minutes, and the
  daemon restarts the time without a write to the devices. It also sends the
  open again when the view shows no hold from Home Assistant. So the room stays
  off as long as a sensor is open. The comparison uses the clocks of both
  hosts: they must agree within a few minutes.
- A daemon that does not restart the time on a repeated open ends the hold at
  the deadline. The room then closes, and the next poll opens it again.
- The window sensors and the service `freedo.set_window` write the same hold of
  a room. A reset with the service lasts while every sensor of the room is
  closed: with an open sensor, the next poll opens the room again.

### Room temperature of a zone

A heating zone regulates on the sensor of its valve. With a room temperature
sensor, the daemon corrects the setpoints it writes, so the room reaches the
target on the reading of that sensor.

- Map a sensor to a zone on the page of its room in Configure, or with select
  Temperature source. A sensor can serve several zones. The hot water zone has
  no sensor.
- On each state change and every 15 minutes, the integration sends
  `POST /api/zones/{n}/temperature` with `{"tempC": n, "source": "<sensor name>"}`.
  It also sends the current state when it starts and when the sensor is chosen.
  A daemon that does not show `externalTemp.source` in `GET /api/zones` gets no
  `source`, because it refuses an unknown field. A value in degrees Fahrenheit or kelvin is
  converted. A state that is `unavailable`, `unknown` or not a number is not
  sent. A value outside -10..50 C is not sent, because the daemon rejects it.
  The integration logs the first failed push of a zone and stays silent until a
  push succeeds.
- Select Room temperature correction says how the daemon uses the reading. See
  Zone settings.
  - Continuous shift (offset mode `setpoint`) shifts the setpoints of the zone
    continuously from the reading.
  - Valve calibration (`auto_offset`) calibrates the correction of the zone
    from the reading, at most once per Number Calibration.
    The correction is tag `0140` on the valve, and in mode `config` each write
    makes the valve home its motor, so the interval bounds the battery and the
    noise.
- A new sensor sets Continuous shift on a zone with the correction None, in
  the page of the room and in select Temperature source. With the correction None the
  daemon answers 409 to a reading, because the other modes write tag `0140` to
  the device. The integration keeps the mapping and logs the first refusal of a
  zone. The sensor Temperature source state shows `no_correction`.
- The correction is slow by design, because a valve next to a radiator swings
  by several degrees. The daemon cannot change what the valve measures, so it
  filters the difference with a time constant of 2 hours, shifts the setpoints
  by 0.2 C at a time and waits at least 15 minutes between two shifts. A
  difference of 2 C needs about 4 hours. These values are design estimates. No
  test on air verifies them.
- While the reading is fresh, the shift replaces the number Offset,
  and the climate entity shows the sensor temperature as the current
  temperature.
- The daemon keeps the reading in memory. A reading older than
  `room_temp_max_age_sec` (30 minutes by default) ends the correction, and the
  shift returns to the manual offset at the same slow pace. A sensor that stays
  unavailable ends the correction in this way.
- When you choose a device of the room as the temperature source, the
  integration sends `DELETE /api/zones/{n}/temperature`. The release starts at
  once and runs at the same pace.
- Sensor Temperature source state shows whether the zone uses the reading, and the
  cause when it does not. See Diagnostics.

## Presence blueprint

The automation blueprint `homeassistant/blueprints/automation/freedo/presence_away.yaml`
sets switch Away from the presence that Home Assistant tracks. It is the
equivalent of tado geofencing. The daemon has no geofencing code.

### Import

Copy the file to `<config>/blueprints/automation/freedo/presence_away.yaml` and
reload the automations.

Then open the blueprint "freedo presence away", select Create automation and
fill the inputs. The blueprint needs Home Assistant 2026.8 or later.

### Inputs

| Input | Default | Meaning |
|---|---|---|
| Away switch | none | The freedo switch Away. |
| People | none | The `person` or `device_tracker` entities that decide. |
| Home zone | `zone.home` | The zone that counts as home. |
| Delay before away | 10 minutes | All people stay away for this time before Away turns on. |
| Delay before home | 0 | One person stays home for this time before Away turns off. |
| Approach zone | `zone.freedo_approach_unset` | Optional. A zone around the home, larger than the home zone. The default names a zone that does not exist, so the feature is off. |
| Blocking entities | none | Optional. The automation does nothing while one of them is on, for example a guest mode `input_boolean`. |

### Behavior

- Away turns on when all selected people are outside the home zone and the
  approach zone for the away delay. A change of state of any selected person
  restarts the delay. A change of attributes, such as a GPS update, does not.
- Away turns off when one selected person is in the home zone, after the home
  delay.
- Away turns off at once when one selected person is in the approach zone. The
  home delay does not apply. See Approach zone.
- A person in the state `unavailable` or `unknown` never turns Away on, and the
  delay restarts when the state changes. A person in the home zone still turns
  Away off while another tracker is unavailable.
- Home zone membership comes from the `in_zones` attribute of the entity, so a
  person located by a router or Bluetooth scanner counts.
- When a blocking entity turns on, the automation cancels a pending delay. When
  it turns off, the automation applies the current presence.
- When Home Assistant has started, the automation applies the current presence.
  It does nothing until the next presence change when the Away switch is
  unavailable at that time.
- The automation calls the switch only when its state must change. A manual
  change of the switch holds until the next presence change.

### Approach zone

The tado cloud ends the away state early when a resident heads home, so the
rooms are warm on arrival. Each zone has a preheating level (eco, balance or
comfort) that trades comfort for energy. No radio tag carries this level: the
cloud only ends the away state earlier. The blueprint does the same with the
position of the people, which Home Assistant knows.

Create the approach zone:

1. Open Settings -> Areas, labels & zones, tab Zones, and add a zone.
2. Put it on the position of the home and set a radius, for example 2000 m.
   The radius is the speed of the person times the heat-up time you want. At
   2 km, a car has about 3 minutes and a person on foot about 25 minutes.
3. Select the zone in the input Approach zone.

How the approach zone works:

- Away turns off at once when one selected person is in the approach zone and
  Away is on. The zones return to their program setpoints before the arrival.
- When that person leaves the approach zone again, Away turns on again after
  the away delay, if all selected people are outside the home zone and the
  approach zone. A person who comes back into the zone during the delay
  cancels it.
- The unavailable and blocking rules, the reconcile at start and the restart of
  the delay are the same as without the zone.
- Leaving the home registers later than without the zone: the away delay starts
  when the last person is outside the approach zone, not at the home zone. In
  return, a person who stays inside the approach zone does not make the home
  switch between away and home.
- Keep the places where a person spends the day, such as a workplace or a
  school, outside the approach zone. A person in the zone counts as present, so
  Away does not turn on.

Overlapping zones:

- Home Assistant sets the state of a person to the smallest zone that holds
  the person, for example `home`. It lists every zone that holds the person in
  the attribute `in_zones`, the smallest first. When the approach zone contains
  the home, a person at home has `in_zones` with both zones.
- The blueprint reads `in_zones`, as the Home Assistant zone conditions do, and
  not the state. A person in a third zone, such as `work`, inside the approach
  zone still counts as in the approach zone.
- The blueprint checks the home zone before the approach zone. A person at home
  keeps the home delay. A person in the approach zone but outside the home zone
  gets no delay.
- Away turns on only when all selected people are outside both zones. The zones
  need not contain each other.
- Only a change of the state of a person starts a run. A change of `in_zones`
  with the same state does not start one, for example a person who stays in a
  third zone that crosses the edge of the approach zone. The next state change
  applies it.

### Difference from tado

- No subscription is needed.
- The presence comes from Home Assistant: any `person` or `device_tracker`,
  not only the phones that run the tado app.
- Switch Away applies to the whole home. freedo has no geofencing flag for each
  schedule block.
- Preheat before arrival: tado starts it from a preheating level of each zone
  and a travel time. The approach zone is one distance for the whole home. Every
  zone returns to its program at the same moment, and the blueprint does not
  estimate a travel time. A person who drives and a person who walks get the
  same radius.
- Pair the approach zone with switch Early start of each zone. The approach zone
  covers an unplanned return. Early start covers the program: it starts a warmer
  block early, from the heat-up time the daemon learns for the zone.
- The approach zone is one step. The Distance setback blueprint lowers the
  rooms in several steps with the distance and raises them on the way home.

## Notification blueprint

The automation blueprint `homeassistant/blueprints/automation/freedo/alert_notification.yaml`
sends a notification for each event of the event entity Alert, and optionally
for a window that stays open.

### Import

Copy the file to `<config>/blueprints/automation/freedo/alert_notification.yaml`
and reload the automations. Then open the blueprint "freedo alert notification",
select Create automation and fill the inputs. The blueprint needs Home Assistant
2026.8 or later.

### Inputs

| Input | Default | Meaning |
|---|---|---|
| Alert event | none | The freedo event entity Alert. |
| Send to | none | The `notify` entities that get the notification, for example the one of a phone with the Home Assistant app. |
| Alerts | all | Low battery, Device fault, Valve warning, Device offline, Boiler fault. Keep at least one: with none the automation does not load. |
| Window sensors | none | Optional. The freedo Window sensors of the rooms to watch. |
| Open for | 30 minutes | A window stays open for this time before its notification. |
| Quiet time start, end | 00:00, 00:00 | A notification that is due between the two times waits for the end. Equal times mean no quiet time. |
| Actions | none | Optional. Actions that run for each notification, with the variables `title` and `message`. |

### Behavior

- An alert sends one notification, with the title "freedo" and an English
  text: the alert, the device, the room and the details. For example "Valve
  warning: Sofa (VA0000000001) in Living (not mounted on a radiator)." and
  "Boiler fault: codes 291, 1110.". A blueprint has no translations.
- The text names a valve warning and a device fault as the dashboard does. A
  name the blueprint does not know goes out as freedo gives it.
- An alert sends no other notification while it holds, and none after a restart
  of Home Assistant or a reload of the integration.
- A window notification goes out once when the sensor has been on for the time.
  The sensor is on for a window that a valve detected and for one that Home
  Assistant set.
- During the quiet time a notification waits, and the waiting ones go out in
  their order when the quiet time ends. At most 20 wait: the automation drops
  the others.

## Distance setback blueprint

The automation blueprint `homeassistant/blueprints/automation/freedo/distance_setback.yaml`
lowers the rooms in steps with the distance of the nearest person, and raises
them as that person approaches. It is the equivalent of the tado preheat before
arrival. It sets number Away of each room, the temperature the room keeps while
switch Away is on. It never sets switch Away: use it with the Presence
blueprint, or switch Away by hand.

### Import

Open Settings -> Automations & scenes -> Blueprints, select Import blueprint
and enter
`https://github.com/CappyT/freedo/blob/main/homeassistant/blueprints/automation/freedo/distance_setback.yaml`.
Or copy the file to `<config>/blueprints/automation/freedo/distance_setback.yaml`
and reload the automations.

The distance comes from the Proximity integration of Home Assistant:

1. Open Settings -> Devices & services, add the integration Proximity and
   select the zone Home.
2. Select the people or device trackers to track.
3. The integration creates the sensor Nearest distance. It is the distance of
   the nearest tracked person from the edge of the home zone, and 0 inside the
   zone.

Then open the blueprint "freedo distance setback", select Create automation and
fill the inputs. The blueprint needs Home Assistant 2026.8 or later.

### Inputs

| Input | Default | Meaning |
|---|---|---|
| Rooms | none | The freedo climate entities of the rooms. The automation finds number Away of each room by itself. |
| Distance of the nearest person | none | The sensor Nearest distance of Proximity. Any length unit works. |
| People | none | Optional. The people that Proximity tracks. See Behavior. |
| Lowest temperature | 16 C | No room goes below it. 5 C means off. |
| Comfort distance | 2 km | Inside it the rooms keep their program temperature. |
| Distance step | 10 km | Each full step beyond the comfort distance lowers the rooms by one more temperature step. |
| Temperature step | 1 C | The size of one step. |
| Hysteresis | 2 km | A person comes this much closer than a step boundary before the rooms rise. |
| Minimum interval | 5 minutes | The shortest time between two changes of the rooms. |
| Blocking entities | none | Optional. The rooms keep their program temperature while one of them is on. |

### Behavior

- Number Away of a room is its program temperature (attribute
  `program_temperature` of the climate entity) minus the steps of the distance,
  and not below the lowest temperature. With the defaults and a program at
  21 C: 21 C up to 2 km, 20 C beyond 2 km, 19 C from 12 km, 18 C from 22 km,
  17 C from 32 km and 16 C from 42 km.
- A room never goes above its program temperature. A program block below the
  lowest temperature gives the program temperature.
- Inside the comfort distance a room keeps its program temperature also while
  switch Away is on. The room is warm before the Presence blueprint turns Away
  off.
- A new block of the weekly program moves number Away of the room with it.
- The rooms rise when the person is closer than a step boundary by the
  hysteresis. At the comfort distance the hysteresis is at most half of that
  distance. A position that moves around a boundary changes nothing.
- The rooms keep their program temperature while the distance is unknown or
  unavailable, while one of the people is `unavailable` or `unknown`, and while
  a blocking entity is on. Proximity leaves out a person with no position, so
  the sensor alone then shows the distance of another person: select the people
  in the input People to cover that case.
- The automation applies the distance again when Home Assistant has started.
- The automation owns number Away of the selected rooms: it replaces a value
  set by hand at its next run. Set the lowest temperature in the automation.
- A room with an away schedule follows that schedule and ignores number Away:
  leave it out. A room with no program temperature is left alone.
- A blocking entity can be a schedule helper, for example to keep the rooms on
  their program at night.

### Radio writes

- At home a change of number Away puts nothing on the air. The daemon stores
  it, and the switch to away carries it.
- While switch Away is on, a change writes that room once: one zone write to
  the device that takes the commands of the room. It is the write of an away
  temperature change in the tado app. It is not a configuration write, so no
  valve homes its motor.
- A room changes at most once in the minimum interval. A change that comes
  earlier waits, and only the last one applies.
- A room changes only when the distance crosses a step boundary, or when its
  program block changes. A trip out and back over 3 steps writes each room 6
  times at most.

### Choose the steps

The distance step is the distance a person travels in the time a room needs to
gain one temperature step. A room that gains 1 C in one hour, and a person who
drives at 50 km/h, give 50 km for each step of 1 C. A shorter step saves more
and can leave the room below its program on arrival.

### Difference from tado

- tado estimates the travel time and holds a preheating level for each room.
  The blueprint reads the distance only: a person who drives and a person who
  walks get the same steps, and every room gets the same steps.
- The lowest temperature is one value for all rooms of the automation. Create
  a second automation for rooms that need another one.
- The steps need Home Assistant. When Home Assistant stops while the home is
  away, the rooms keep the last value until it runs again.

## Auxiliary heat blueprint

The automation blueprint `homeassistant/blueprints/automation/freedo/auxiliary_heat.yaml`
runs another heat source of a room while the room is far below its target, for
example an air conditioner or a heat pump that Home Assistant controls as a
`climate` entity. The radiators keep heating. The automation reads the freedo
climate entity of the room and never writes to freedo. The tado app has no such
function.

### Import

Open Settings -> Automations & scenes -> Blueprints, select Import blueprint
and enter
`https://github.com/CappyT/freedo/blob/main/homeassistant/blueprints/automation/freedo/auxiliary_heat.yaml`.
Or copy the file to `<config>/blueprints/automation/freedo/auxiliary_heat.yaml`
and reload the automations.

Then open the blueprint "freedo auxiliary heat", select Create automation and
fill the inputs. Create one automation for each room. The blueprint needs Home
Assistant 2026.8 or later.

### Inputs

| Input | Default | Meaning |
|---|---|---|
| Room | none | The freedo climate entity of the room. |
| Appliance | none | The `climate` entity of the other heat source. |
| Start gap | 1.5 C | The appliance starts when the room is at least this much below its target. |
| Stop gap | 0.3 C | The appliance stops when the room is at most this much below its target. |
| Appliance mode | Heat | The HVAC mode of a run: `heat`, `heat_cool` or `auto`. |
| Appliance temperature | 22 C | The target temperature of a run. |
| State after the run | The state before the run | Or Off. |
| Maximum run time | 2 hours | The appliance stops after this time. |
| Minimum off time | 10 minutes | The appliance does not start again for this time after a run. |
| Start and end of the time window | 00:00 | The appliance starts only between the two times. Equal times mean the whole day. |

### Behavior

- The gap is the target of the room minus its temperature. The appliance starts
  when the gap is at least the start gap, inside the time window. A room that
  goes from 18 C to a target of 20.5 C in the morning has a gap of 2.5 C.
- The appliance does not start while the room is off, in away (attribute
  `target_source` is `away`), has an open window, or has no target or no
  temperature. A run stops when one of these begins.
- A run stops when the gap is at most the stop gap, or after the maximum run
  time. After the maximum run time the appliance starts again only after the
  gap has been at most the stop gap.
- At the stop the appliance returns to the state it had before the run: its
  mode, its target temperature and its other settings. The automation keeps
  that state in a scene, which a restart of Home Assistant drops. Without the
  scene, and with State after the run at Off, the appliance turns off.
- A person who changes the mode or the target temperature of the appliance
  during a run keeps it. The automation restores nothing and leaves the
  appliance alone until the gap is at most the stop gap. A new run can start
  after that.
- The appliance rests for the minimum off time after each run, also after a
  run that a person ended.
- A run that started inside the time window can end after it.
- An appliance that is unavailable does not start. An appliance that becomes
  unavailable during a run, as a cloud device can, keeps the run: the
  automation decides again when it is back.
- An appliance that does not take the mode within 2 minutes returns to its
  state, and the automation tries again after the minimum off time.
- A restart of Home Assistant ends a run and leaves the appliance on. At the
  start the automation takes back an appliance that it finds in the mode and at
  the temperature of a run, and turns it off at the stop.
- The appliance heats to its own target temperature with its own sensor. Set
  the appliance temperature above the target of the room, or the appliance
  stops by itself before the room is near its target.

## Season profile blueprint

The automation blueprint `homeassistant/blueprints/automation/freedo/season_profile.yaml`
sets select Profile for the season. The daemon has no season calendar.

### Import

Copy the file to `<config>/blueprints/automation/freedo/season_profile.yaml` and
reload the automations. Then open the blueprint "freedo season profile", select
Create automation and fill the inputs. The blueprint needs Home Assistant 2026.8
or later.

### Inputs

| Input | Default | Meaning |
|---|---|---|
| Profile select | none | The freedo select Profile. The selector lists the selects of freedo, so the entity id does not matter. |
| Winter profile | `Winter` | The profile name for the heating season, as the select lists it. |
| Summer profile | `Summer` | The profile name for the rest of the year. |
| Method | Dates | Dates or Outdoor temperature. |
| Winter start, Summer start | 15 October, 15 April | Method Dates. The first day of each profile. The year does not count. |
| Outdoor temperature sensor | `sensor.freedo_outdoor_unset` | Method Outdoor temperature. The default names a sensor that does not exist. |
| Winter threshold, Summer threshold | 14 C, 18 C | Method Outdoor temperature. Keep the summer threshold above the winter one. |
| Delay | 12 hours | Method Outdoor temperature. The time beyond a threshold before the change. |

### Behavior

- Method Dates: the winter profile is active from the winter start to the
  summer start, the summer profile for the rest of the year. The automation
  checks the date every day at 00:00:30 and when Home Assistant starts.
- Method Outdoor temperature: the winter profile becomes active when the
  outdoor temperature stays below the winter threshold for the delay, the
  summer profile when it stays above the summer threshold for the delay. A
  state that is not a number counts as neither. Between the thresholds, and at
  a start, the profile does not change. The daemon keeps the active profile
  across restarts.
- The automation calls the select only when another profile is active and the
  target profile exists. A manual change holds until the next change of date or
  season.

## Reports

tado shows the heating activity of each room and a daily climate report. freedo
keeps no time series. The recorder of Home Assistant stores the history of every
entity, and native features of Home Assistant derive the reports from it. The
integration adds only the binary sensor Heating, the one entity that these
features cannot build from the others.

### Source entities

| Report | Entity | Note |
|---|---|---|
| Heating time of a room | binary sensor Heating, `binary_sensor.<zone>_heating` | On while the demand of the zone is above 0. `history_stats` counts states: the attribute `hvac_action` of the climate entity is not a state. |
| Heating intensity of a room | sensor Heating demand, `sensor.<zone>_heating_demand` | 0 to 100 %, state class measurement. |
| Gas | sensor Gas volume, `sensor.freedo_gas_volume` | Unit of `fuel_unit` (m³ by default), state class total increasing. |
| Heat output | sensor Heat output energy, `sensor.freedo_heat_output_energy` | kWh, state class total increasing. |
| Temperature | sensor Temperature of a device, `sensor.<device>_temperature` | Diagnostic and disabled by default. |
| Humidity | sensor Humidity of a device, `sensor.<device>_humidity` | Diagnostic and disabled by default. It exists after the device reported a humidity. |

- `<zone>` is the name of the zone as an entity id: the zone `Living` gives
  `living`.
- `<device>` is the name of the device as an entity id: the label and the serial
  number, for example `living_va0000000001`, or the serial number alone for a
  device without a label.
- A second freedo hub gets a suffix on the hub entities. Check the entity ids in
  Settings -> Devices & services -> Entities.
- The gas and heat values are estimates from the modulation and the data sheet
  of the boiler. See Boiler energy and Boiler gas.

### Heating time of each room

A `history_stats` sensor counts the hours in which the sensor Heating is on. Add
one entry for each zone to `configuration.yaml`, then reload History Stats in
Developer tools -> YAML, or restart Home Assistant:

```yaml
sensor:
  - platform: history_stats
    name: Heating today <zone>
    unique_id: freedo_heating_today_<zone>
    entity_id: binary_sensor.<zone>_heating
    state: "on"
    type: time
    start: "{{ today_at() }}"
    end: "{{ now() }}"
    state_class: total_increasing
```

- The value is in hours and starts again at 0 at midnight.
- `history_stats` reads the history of the recorder. The default retention is 10
  days: a window longer than the retention needs `purge_keep_days` in the
  `recorder` configuration.
- State class `total_increasing` makes the recorder keep the long-term
  statistics of the sensor, which the purge does not remove.
- `history_stats` is also a helper in Settings -> Devices & services -> Helpers.
  Use the fields of the example. `type: ratio` gives the share of the day and
  `type: count` gives the number of heating periods.

### Gas and heat energy for each day and month

A `utility_meter` follows a counter that never resets and restarts at the begin
of each cycle. Add it to `configuration.yaml` and restart Home Assistant:

```yaml
utility_meter:
  gas_daily:
    source: sensor.freedo_gas_volume
    name: Gas daily
    unique_id: freedo_gas_daily
    cycle: daily
  gas_monthly:
    source: sensor.freedo_gas_volume
    name: Gas monthly
    unique_id: freedo_gas_monthly
    cycle: monthly
  heat_output_daily:
    source: sensor.freedo_heat_output_energy
    name: Heat output daily
    unique_id: freedo_heat_output_daily
    cycle: daily
  heat_output_monthly:
    source: sensor.freedo_heat_output_energy
    name: Heat output monthly
    unique_id: freedo_heat_output_monthly
    cycle: monthly
```

This gives the sensors `sensor.gas_daily`, `sensor.gas_monthly`,
`sensor.heat_output_daily` and `sensor.heat_output_monthly`. A meter keeps its
value over a restart of Home Assistant. `utility_meter` is also a helper in
Settings -> Devices & services -> Helpers.

### Cost

With `fuel_unit` `m3`, add the sensor Gas volume as a gas source in the Energy dashboard
and enter the price. The steps are in Boiler gas. The dashboard shows consumption and cost for
a day, a month or a year, from the long-term statistics of the sensor. It needs
no utility meter. The sensor Heat output energy has no price: it is heat and not
gas energy.

### Temperature and humidity

The sensors Temperature and Humidity of a device have state class measurement,
so a statistics graph card shows them from the long-term statistics. Enable them
in Settings -> Devices & services -> Entities first.

- The temperature is the reading of the device on the wire scale. It does not
  include the correction of a zone in offset mode `setpoint`.
- The climate entity gives the temperature of the zone as an attribute. The
  long-term statistics do not hold attributes.

Add each card in the edit mode of a dashboard with the code editor:

```yaml
type: statistics-graph
title: Temperature
chart_type: line
period: hour
days_to_show: 7
stat_types:
  - mean
  - min
  - max
entities:
  - sensor.<device>_temperature
```

```yaml
type: statistics-graph
title: Humidity
chart_type: line
period: hour
days_to_show: 7
stat_types:
  - mean
entities:
  - sensor.<device>_humidity
```

## Licence

Apache License 2.0. See the [LICENSE](../../LICENSE) file.
