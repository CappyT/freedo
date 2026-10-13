# freedo beta add-on

Runs the pre-releases of the freedo daemon, a local coordinator for tado 868 MHz heating devices, on
Home Assistant. A pre-release has less testing on a real plant than a stable release.

The add-on has its own data. At its first start it imports a copy of the state of the stable freedo
add-on. Stop the stable add-on before you start this one: the two must not run at the same time. The
add-on documentation describes the import and the return to the stable add-on.
