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
start, so a changed token updates the entry.

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
Entities are created from the zones and devices present at setup: reload the
integration after you add a zone or a device.

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
| number Boiler maximum power, Boiler minimum power | hub | `/api/install` `boiler_max_power_kw`, `boiler_min_power_kw`; writes `PATCH /api/install` |
| number Boiler maximum gas flow, Boiler minimum gas flow | hub | `/api/install` `boiler_max_gas_m3h`, `boiler_min_gas_m3h`; writes `PATCH /api/install` |
| number Flow limit | circuit | `/api/boiler` `flowLimit.circuits[]`, `/api/install/circuits` `heat_source`, `/api/install` `flow_limit_c`; writes `POST /api/boiler/flow`, or `PATCH /api/install` while a curve or the adaptation drives the circuit |
| switch Flow limit default curve, switch Flow limit adaptation | hub | `GET /api/install` `flow_default_curve`, `flow_auto_adapt`; writes `PATCH /api/install` |
| switch Away | hub | `GET` and `PATCH /api/home` |
| switch Offline schedule | hub | `GET /api/install` `offline_schedule`; writes `PATCH /api/install` |
| switch Pairing mode | hub | `GET /api/pair/window` `open`, `until`; writes `POST /api/pair/window` |
| select Display orientation, switch Change notification | valve in a zone | `/api/nodes` `Display`; writes `PUT /api/nodes/{id}/display` |
| switch Child lock | valve in a zone | `/api/nodes` `ChildLock`; writes `POST /api/nodes/{id}/childlock` |
| button Identify | valve in a zone | `POST /api/nodes/{id}/identify` |
| button Calibrate motor | valve | `POST /api/nodes/{id}/calibrate` |
| sensor Calibration | valve | `/api/nodes` `Telemetry` `calib_state` and `calib_error` |
| switch Early start, switch Open window detection, number Open window duration | heating zone | `/api/install/zones`; writes `PATCH /api/install/zones/{n}` |
| switch Automatic correction, number Automatic correction interval | heating zone | `/api/zones` `declared[].autoOffset`, `/api/install/zones` `auto_offset`, `auto_offset_interval_sec`; writes `PATCH /api/install/zones/{n}` |
| select Device change end rule, number Device change timer | heating zone | `/api/install/zones` `device_overlay_end`, `device_overlay_end_sec`; writes `PATCH /api/install/zones/{n}` |
| number Away temperature | heating zone | `/api/install/zones` `away_temperature_c`; writes `PATCH /api/install/zones/{n}` |
| number Temperature offset | heating zone | `/api/install/zones` `setpoint_offset_c`; writes `POST /api/zones/{n}/offset` |
| select Temperature offset mode | heating zone | `/api/install/zones` `offset_mode`; writes `PATCH /api/install/zones/{n}` |
| select Temperature source | heating zone | `/api/install/devices`, `/api/zones` `declared[].measuringDevice`, `/api/install/zones` `offset_mode`; writes `PUT /api/install/zones/{n}/measuring` and the options |
| sensor External room sensor | heating zone | `/api/zones` `declared[].externalTemp`, `/api/install/zones` `offset_mode` |
| select Preset | hub | `/api/presets` `id`, `name`, `active`; writes `POST /api/presets/{id}/activate` |

### Presets

Select Preset lists the program presets of the daemon (`GET /api/presets`) and
shows the active one. Choosing an option activates that preset
(`POST /api/presets/{id}/activate`) and polls the daemon again. The options
follow a rename, a new preset and a delete on the next poll, without a reload.
The daemon has no season calendar, so an automation switches the preset by the
season or the weather. A daemon without the route (a 0.4.0 daemon answers 404)
gets no select: reload the integration after a daemon upgrade.

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
  the sensor Default flow limit is unknown, and so is the sensor Flow limit served
  of a circuit that nothing manages. The daemon serves that circuit no limit.
- A circuit that follows a weather curve, the default curve or the adaptation
  takes no direct value: the daemon answers 409 to `POST /api/boiler/flow`. The
  number then shows the manual limit `flow_limit_c` (unknown while it is unset)
  and writes it with `PATCH /api/install`. The curve and the adaptation start
  from this limit. The computed and the served values stay in the sensors Flow
  limit computed and Flow limit served.
- Switch Flow limit default curve is `flow_default_curve`. It gives each circuit
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
  computed and Flow limit served show that a circuit follows the default curve
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
- Numbers Boiler maximum power (0.1 to 500 kW) and Boiler minimum power (0 to
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

| `fuel_unit` | Gas volume | Gas flow, Boiler maximum and minimum gas flow |
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
- Numbers Boiler maximum gas flow (0.01 to 60) and Boiler minimum gas flow
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
  in the unit of `fuel_unit` per hour. Enter the maximum in Boiler maximum gas flow
  and the minimum in Boiler minimum gas flow.
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
- Sensor External room sensor shows whether a heating zone regulates on the
  reading of its room temperature sensor. The state is `active`, or the cause:
  `wrong_offset_mode` (the offset mode is not `setpoint` and switch Automatic
  correction is off), `no_reading` (the daemon holds no reading) or `stale` (the
  last reading is older than `room_temp_max_age_sec`). It is unknown while the
  daemon serves no offset mode. The attributes are `offset_mode`,
  `temperature_c` and `age_sec` (the reading), `device_temperature_c` (the
  temperature of the valve), `shift_c` (the shift applied to the setpoints) and
  `wanted_shift_c` (the shift it moves toward). The sensor needs a daemon that
  serves `externalTemp` in `GET /api/zones`.
- Binary sensor Communication warning is on while the device shows a radio link
  warning. The attribute `warnings` lists them: `noNeighbour` and
  `noConfigAnswer`. These are the link bits of the fault word. They clear
  when the link recovers and are not a Fault. A daemon that sends no
  `commsWarnings` gets no sensor.

### Valve controls

- Switch Child lock locks the controls of the valve. The valve takes the write
  at its next wake-up and the daemon reports the new state when the valve
  confirms it. Until then the entity shows the requested state, for at most
  15 minutes. Only a valve has a child lock: the node view reports `ChildLock`
  as null on other devices, and they get no switch. A valve that belongs to no
  zone gets no switch either.
- Button Identify makes the valve signal itself. Button Calibrate motor starts
  the motor calibration, a full stroke of the valve. A 2.xx answer of the valve
  does not guarantee the stroke: the valve drops the request in silence in some
  states.
- Sensor Calibration is the result of the last calibration: `none`, `started`,
  `done` or `failed`, from the `d/<serial>/mnt` report. The attribute
  `error_code` is the error code of the valve. The state is unknown until the
  valve reports a calibration.
- Number Temperature offset corrects the temperature of a heating zone, -10 to
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
- Number Open window duration is the time the zone stays open after a
  detection (`open_window_duration_sec`), in minutes, 1 to 1092.
- Switch Automatic correction calibrates the correction of the zone from the
  external room temperature, at most once per Number Automatic correction
  interval. It applies in the offset modes `config` and `dbg2`. The daemon
  refuses it on the hot water zone and in mode `setpoint`. Its attributes
  `last_write_at`, `next_at`, `filtered_diff_c`, `sample_span_sec` and
  `stop_reason` are the `autoOffset` view of `GET /api/zones`. `stop_reason`
  says why the daemon stopped the automatic writes of the zone. Switch the
  correction off and on to start them again. See Room temperature of a zone.
- Number Automatic correction interval is that interval
  (`auto_offset_interval_sec`), in hours, 1 to 168.
- Select Device change end rule is how long a change made on a device lasts, as
  the rule `device_overlay_end`: `manual` until the next command, `until_next`
  until the next block of the weekly program, `timer` for a set time. The rule
  does not travel on the air: the daemon applies it and returns the zone to its
  program. Rule `timer` starts at 60 minutes.
- Number Device change timer is the time of the rule `timer`
  (`device_overlay_end_sec`), in minutes, 1 to 10080. It is unavailable while
  the rule is not `timer`.
- Number Away temperature is the temperature the zone keeps while switch Away
  is on (`away_temperature_c`), 5 to 25 C in steps of 0.1 C, as the climate
  entity. 5 C means off in away, as "off in away" in the tado app. A zone with
  no value is off in away too, so the number shows 5 C until you set one. The
  entity needs a daemon that serves `away_temperature_c` in
  `GET /api/install/zones`. The hot water zone has no away temperature and
  gets no number.
- Select Temperature offset mode is how the temperature correction of the zone
  reaches it (`offset_mode`). The hot water zone has no correction and gets no
  select.
  - Valve configuration (`config`, the default) writes tag `0140` in the whole
    d/config document of the measuring device. The valve rereads its
    configuration and homes its motor, a full stroke, on each change of the
    number Temperature offset. The wall thermostat ignores the document of a
    d/config write, so the daemon serves it a new document and the thermostat
    rereads it.
  - Valve debug record (`dbg2`) writes the single record `0140` with
    `PUT d/dbg2/tlvs`. The valve does not home its motor. No capture shows this
    write on air. The wall thermostat has no such resource: the daemon accepts
    the mode for its zone and refuses the offset write with 400.
  - Setpoint shift (`setpoint`) writes nothing to the device. The daemon keeps
    the offset, subtracts it from each setpoint it writes to the zone, and adds
    it to the temperature and the target it reports. The valve keeps the
    `0140` it holds, and the shift adds to it. The room temperature sensors of
    the options need this mode.
  - A change of the mode writes no offset to the device. Leaving `setpoint`
    drops the stored offset, rewrites the zone without the shift (the valves get
    new setpoints) and ends the room temperature reading of the zone. Entering
    `setpoint` starts the offset at 0 and leaves the `0140` on the valve. To
    clear a `0140` written earlier, set the number Temperature offset to 0
    before the change.
  - A zone without a measuring device gets the number Temperature offset only in
    mode `setpoint`: reload the integration after the change.
  - The daemon answers 400 to a mode other than the three, 404 to an unknown
    zone, 503 while it holds no installation, and 500 when the running daemon
    does not take the saved change. The select shows the answer as an error and
    keeps the mode.

### Temperature source

Select Temperature source chooses where the room temperature of a heating zone
comes from: a device placed in the room, or the external sensor mapped in the
options. The select exists while the room has a device of its own or a mapped
sensor.

- The options are the declared members of the zone (`members` of
  `GET /api/zones`), labelled with the device label and the serial from
  `GET /api/install/devices`. A member whose `zone` field names another room is
  left out. The option `external` appears while the options map a sensor to the
  zone.
- The current option is `external` while a sensor is mapped, its pushes are not
  paused and the daemon uses the reading: the offset mode is `setpoint`, or
  switch Automatic correction is on; otherwise it is the effective measuring
  device (`measuringDevice` of `GET /api/zones`).
- Choosing a device writes `PUT /api/install/zones/{n}/measuring` with the
  device reference. The measuring device reaches the valves in the next zone
  document, at their next hourly check. When a sensor is mapped to the zone, the
  choice also pauses its pushes and sends `DELETE /api/zones/{n}/temperature`, so
  the daemon returns to the manual correction. The offset mode does not change.
- Choosing `external` resumes the pushes and sends the current reading at once.
  When switch Automatic correction is off, the choice also sets the offset mode
  `setpoint` if it is not: the daemon refuses that mode while the automatic
  correction runs. See Room temperature of a zone.
- The pause is stored in the options, so it survives a restart.

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
- The switch is a config entity of the hub device. It does not show the devices
  the window heard, and the integration does not send the pair command
  (`POST /api/pair`). A daemon without `GET /api/pair/window` gets no switch.

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
  (`POST /api/zones/{n}/window` with `{"open": bool}`).
  - It is the same hold as the one the window sensors of the options set: the
    daemon keeps one hold from Home Assistant for each zone, and the last write
    wins. See Environment inputs.
  - An activated state ends at the reset, or when the daemon ends it after
    `external_window_max_sec` (2 hours by default, an option of the
    installation).
  - A reset ends the hold from Home Assistant only. A state the valves detected
    (`window_source` `device`) runs until its time ends (number Open window
    duration), and the daemon has no route to reset it. When both hold the zone,
    `window_source` is `both` and the zone stays open after the reset.
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

## Environment inputs

Open the integration options to map Home Assistant entities to the daemon:

- Outdoor temperature sensor. The integration sends `POST /api/outdoor` with
  `{"tempC": n}` on each state change and every 15 minutes. The daemon keeps
  the reading in memory only, so the periodic push also covers a daemon
  restart. A value in degrees Fahrenheit or kelvin is converted.
- Window and door sensors. The second options step selects the heating zone of
  each sensor. On each change of a zone state, the integration sends
  `POST /api/zones/{n}/window` with `{"open": bool}`. A zone is open when one
  of its sensors is on. The pushes of one zone go one at a time, and each
  sends the sensor states of its send time, so the daemon ends on the last
  state. The integration also sends the current state of each zone when it
  starts. When the daemon does not answer or answers 5xx, the integration
  sends the current state of the zone again every 30 seconds until the daemon
  takes it. A 4xx answer is not sent again.
- Room temperature sensors. The last options step selects an optional
  temperature sensor for each heating zone. See Room temperature of a zone.

The window sensors and the service `freedo.set_window` write the same hold of a
zone. A sensor pushes its zone when its state changes, when the integration
starts and when a push needs a retry. Between those pushes, the last write
stands. For example, after a reset with the service the daemon keeps the zone
closed while a sensor stays open, until a sensor of the zone changes state.

A state that is `unavailable`, `unknown` or not a number is not sent. An outdoor
temperature outside -60..60 C is not sent, because the daemon rejects it.

### Room temperature of a zone

A heating zone regulates on the sensor of its valve. With a room temperature
sensor, the daemon corrects the setpoints it writes, so the room reaches the
target on the reading of that sensor.

- Select a sensor with the device class temperature for each heating zone. A
  sensor can serve several zones. The hot water zone has no field. The step
  lists the zones only while the integration is loaded. Otherwise it keeps the
  saved sensors.
- On each state change and every 15 minutes, the integration sends
  `POST /api/zones/{n}/temperature` with `{"tempC": n}`. It also sends the
  current state when it starts. A value in degrees Fahrenheit or kelvin is
  converted. A state that is `unavailable`, `unknown` or not a number is not
  sent. A value outside -10..50 C is not sent, because the daemon rejects it.
  The integration logs the first failed push of a zone and stays silent until a
  push succeeds.
- The daemon uses the reading in two ways.
  - Offset mode `setpoint` shifts the setpoints of the zone continuously from
    the reading. Set it with the select Temperature offset mode of the zone
    (option Setpoint shift). See Zone settings.
  - Switch Automatic correction (`auto_offset`) calibrates the correction of the
    zone from the reading, at most once per Number Automatic correction
    interval. It applies in the modes `config` and `dbg2`, where the correction
    is tag `0140` on the valve. In mode `config` each write makes the valve home
    its motor, so the interval bounds the battery and the noise. The daemon
    refuses the switch on the hot water zone and in mode `setpoint`, and refuses
    the mode `setpoint` while the switch is on.
- The options do not change the mode. In the modes `config` and `dbg2` without
  automatic correction the daemon answers 409 to a reading, because those modes
  write tag `0140` to the device. The integration keeps the mapping and logs the
  first refusal of a zone. The sensor External room sensor shows
  `wrong_offset_mode`.
- The correction is slow by design, because a valve next to a radiator swings
  by several degrees. The daemon cannot change what the valve measures, so it
  filters the difference with a time constant of 2 hours, shifts the setpoints
  by 0.2 C at a time and waits at least 15 minutes between two shifts. A
  difference of 2 C needs about 4 hours. These values are design estimates. No
  test on air verifies them.
- While the reading is fresh, the shift replaces the number Temperature offset,
  and the climate entity shows the sensor temperature as the current
  temperature.
- The daemon keeps the reading in memory. A reading older than
  `room_temp_max_age_sec` (30 minutes by default) ends the correction, and the
  shift returns to the manual offset at the same slow pace. A sensor that stays
  unavailable ends the correction in this way.
- When you remove the sensor of a zone in the options, the integration sends
  `DELETE /api/zones/{n}/temperature`. The release starts at once and runs at
  the same pace. If the call fails, the age limit ends the correction.
- Sensor External room sensor shows whether the zone uses the reading, and the
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
