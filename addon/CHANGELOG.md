# Changelog

## 0.3.1

- A serial reply that the USB link cuts no longer stops the daemon. It discards the reply and
  goes on, counts it in `serialErrors` of `GET /api/health`, and stops only when the link
  answers nothing for 30 s. The radio frame inside a lost reply is lost.
- The daemon talks 230400 baud to the bridge and falls back to 921600 for firmware before
  v2.10. Flash the firmware of this release to stop the lost replies on a Raspberry Pi.
- A restart continues the Router Advertisement interval and sends no burst at every start.
- Each release attaches the bridge firmware image and `SHA256SUMS`.
- A Home Assistant temperature sensor can be mapped to a heating zone, with a select for the
  temperature offset mode of the zone.

## 0.3.0

- Default weather curve for the flow limit, off by default.
- Switches for the flow limit default curve and the flow limit adaptation.
- Italian, German and French translations.

## 0.2.0

- First release of the add-on.
