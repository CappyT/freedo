# freedo beta

This add-on runs the pre-releases of the freedo daemon, such as `0.6.0-beta.1`. A pre-release has
less testing on a real plant than a stable release. A stable release also reaches this add-on, so it
is never behind the stable freedo add-on.

Everything that this page does not describe works as in the stable add-on: read its
[documentation](https://github.com/CappyT/freedo/blob/main/addon/DOCS.md) first.

## Rules

- Stop the stable add-on before you start this one, and stop this one before you start the stable
  add-on. One coordinator serves a radio network. An add-on that finds the other one running stops
  its start with an error in the log.
- Turn on Start on boot for the add-on you use, and turn it off for the other one.
- The two add-ons have separate data. A change you make here does not reach the stable add-on, and
  nothing moves by itself. A [backup](https://github.com/CappyT/freedo/blob/main/addon/DOCS.md#backup)
  carries the state from one add-on to the other.

## Move to the beta add-on

The backup is for Home Assistant administrators: sign in as one. No token is needed. Like the stable
add-on, this add-on has the permission `homeassistant_api` to ask Home Assistant whether the user is an
administrator: see
[Backup](https://github.com/CappyT/freedo/blob/main/addon/DOCS.md#backup).

1. Open the web interface of the stable freedo add-on, then Settings. In the section Backup select
   Download backup. The file holds the network key: keep it private.
2. Stop the stable add-on and turn off its Start on boot.
3. Install freedo beta. Select the same serial port and save. Leave Transmit off.
4. Start freedo beta. It starts with an empty installation of its own.
5. Open its web interface, then Settings. In the section Backup choose the file and select Restore
   backup. The page lists what the backup replaces and asks first. The daemon restarts on the backup
   by itself, and the page reconnects after a few seconds.
6. Check the plant in the web interface, then turn on Transmit. The add-on restarts.

The freedo integration needs no change. The add-on that starts tells Home Assistant its address and
its token, and the integration follows it.

The daemon upgrades the schema of a restored backup when the pre-release needs it. The database of
the stable add-on keeps its schema.

Two waits protect the message identifiers. A device keeps the identifiers it saw for 247 seconds, and
each database continues its own sequence.

- With Transmit on, a start less than 250 seconds after the stop of the other add-on waits for the rest
  of that time.
- With Transmit on, a start less than 250 seconds after a restore waits for the rest of that time. The
  web interface does not answer while the daemon waits.

The log says how long each wait is.

## Options

The options `serial_port`, `api_token` and `transmit` work as in the stable add-on. This add-on has
no option of its own.

A configuration saved by a release up to 0.7.0 holds the option `import_stable_state`. The option has
no effect, and the Supervisor logs a warning about it at each start. Open the Configuration tab and
save once to remove it.

The API port 18080 has no host port by default, so that both add-ons can be installed. Set one in
the Network tab when another client needs the API. The integration does not need it.

## Return to the stable add-on

The return is the same move in the other direction.

1. In the web interface of this add-on, download a backup.
2. Stop this add-on and turn off its Start on boot.
3. Start the stable add-on and restore the backup in its web interface.

The stable add-on accepts the backup only when it reads the schema of the file. A pre-release with a
newer schema writes a backup that the stable add-on refuses with a message that names the two schema
versions. Then the stable add-on stays on its own database, in the state of its last stop:

- A room, a device, a program, a preset or an option that you changed here is not in the stable
  database. Make the change again there.
- A device that you paired here is not in the stable installation. Add it there too. The network key
  is the same in both add-ons when this add-on runs on a backup of the stable one.
- A device keeps the program it fetched last until a schedule write changes its document. Check the
  programs after the return, as after a
  [database rollback](https://github.com/CappyT/freedo/blob/main/docs/OPERATION.md#schema-upgrade-and-rollback).

An older state is safe on air. The devices do not check the frame counter, so a lower counter is
accepted. The two waits of 250 seconds cover the message identifiers.

A restore keeps the database that it replaced in `/data/backup/before-import-<time>/`, and the three
newest of these folders stay.

## The folder /share/freedo/stable of older releases

Releases up to 0.7.0 kept a copy of the stable database in `/share/freedo/stable/` and this add-on
imported it. No release writes or reads that folder now, and no add-on deletes it. The copy holds the
network key: delete `/share/freedo/stable` by hand. The log names the folder at every start while it
exists.

## Data and backups

The data folder of this add-on holds its own database, its token and its log. The database holds the
network key, also in the folders under `/data/backup/`. Home Assistant backups include this folder.
