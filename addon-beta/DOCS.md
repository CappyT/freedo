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
- The two add-ons have separate data. A change you make here does not reach the stable add-on.

## Move to the beta add-on

1. Update the stable freedo add-on and start it once. Its documentation must have the section State
   export: an older version writes no export, and this add-on then has nothing to import.
2. Stop the stable add-on and turn off its Start on boot.
3. Install freedo beta. Select the same serial port and save.
4. Start freedo beta. At the first start it imports the state export, and the log says so:
   `imported the state of the stable add-on: freedo <version>, schema <n>, exported at <time>`.
5. Check the plant in the web interface, then turn on Transmit and restart. The options of the two
   add-ons are separate, so this add-on starts in dry-run.

The freedo integration needs no change. The add-on that starts tells Home Assistant its address and
its token, and the integration follows it.

With Transmit on, a start less than 250 seconds after the stop of the other add-on waits for the rest
of that time. The log says how long.

## Options

The options `serial_port`, `api_token` and `transmit` work as in the stable add-on. This add-on adds
one option.

| Option | Default | Meaning |
|---|---|---|
| `import_stable_state` | off | At the next start, replace the state of this add-on with the state export of the stable add-on. |

The API port 18080 has no host port by default, so that both add-ons can be installed. Set one in
the Network tab when another client needs the API. The integration does not need it.

## Import

The stable add-on writes a copy of its database to `/share/freedo/stable/` at every start and after
every stop. This add-on reads that copy and never writes to it.

- With no database of its own, this add-on imports the copy at start. When the folder holds no copy,
  the add-on starts with an empty installation and the log says so. When this daemon cannot read the
  copy, the start stops with an error.
- With a database of its own, this add-on imports only when `import_stable_state` is on. It first
  moves its own database to `/data/backup/before-import-<time>/` and keeps the three newest of these
  folders. Turn the option off after the import. While the option is on, the add-on imports each new
  export once: a copy that it already imported is not imported again.

The daemon upgrades the schema of the imported database when the pre-release needs it. The copy in
`/share/freedo/stable/` and the database of the stable add-on keep their schema.

## Return to the stable add-on

Stop this add-on, then start the stable add-on. The stable add-on runs on its own database, in the
state of its last stop. Nothing made here goes back by itself:

- A room, a device, a program, a preset or an option that you changed here is not in the stable
  database. Make the change again there.
- A device that you paired here is not in the stable installation. Add it there too. The network key
  is the same in both add-ons.
- A device keeps the program it fetched last until a schedule write changes its document. Check the
  programs after the return, as after a
  [database rollback](https://github.com/CappyT/freedo/blob/main/docs/OPERATION.md#schema-upgrade-and-rollback).

The older state is safe on air. The devices do not check the frame counter, so the lower counter of
the stable database is accepted. The wait of 250 seconds at the start covers the message identifiers.

To start this add-on again later from the current stable state, turn on `import_stable_state` for
one start.

## Data and backups

The data folder of this add-on holds its own database, its token and its log. The database holds the
network key, also in the folders under `/data/backup/`. Home Assistant backups include this folder.
