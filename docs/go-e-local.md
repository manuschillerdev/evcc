# go-e Gemini local HTTP API v2

This fork provides `go-e-local`, an independent driver for the go-e Charger Gemini 2.0
and Gemini flex 2.0. It connects directly to the charger's local HTTP API v2 and
does not require a sponsor token, a go-e cloud account, or Home Assistant.

Enable **HTTP API v2** in the go-e app, connect the charger to your LAN, and reserve
its IP address in your router. Enable **simulate unplugging** in the car settings
when using automatic phase switching. Select **Charger Gemini flex 2.0 (local API
v2)** in the configuration UI, or configure:

```yaml
chargers:
  - name: goe
    type: template
    template: go-e-local
    host: 192.168.1.50

loadpoints:
  - title: Garage
    charger: goe
    mode: off
```

Replace the example IP address with the charger's address. This is a fragment to
merge into your existing configuration. Build and run this fork to use the driver;
upstream binaries do not include it. The existing `go-e` and `go-e-v3` templates
still select the upstream integrations.

For a direct configuration, use `type: go-e-local` with
`uri: http://192.168.1.50`. `cache` defaults to `1s`; `cache: 0s` disables caching.
The constructor only reads status. Commands invalidate the cached status, including
when their response fails, and only an explicit `true` acknowledgement counts as
success.

Supported operations:

- Connection and charging state, including charger faults.
- Start and stop via `frc=2` and `frc=1`. These force charging on/off, so let evcc
  control charging and disable competing Home Assistant or charger automations.
- Whole-amp current control via `amp`, bounded by the reported minimum, configured
  maximum and 11 kW / 22 kW hardware rating. Cable, adapter and thermal limits remain
  enforced by the charger. Set the loadpoint maximum to match your installation.
- One or three phases via `psm=1` / `psm=2`; the charger performs the switch.
- Power in watts, total energy in kWh, phase currents in amperes and voltages in volts.
  Missing measurements return an error instead of zero.

Session energy is calculated by evcc from the total energy meter. RFID identification,
cloud access, and the legacy HTTP API v1 are outside this driver's scope. This driver
does not configure a communication watchdog: a loss of connectivity can leave the
last requested charging state/current active.

To build, follow [the repository build instructions](../CONTRIBUTING.md#building-from-source).
With the charger configured, first inspect read-only diagnostics:

```sh
./evcc charger --config /path/to/evcc.yaml goe
```

The driver has automated HTTP simulation tests. Physical Gemini 2.0 validation is
still required, including start/stop, 6 A charging, and switching phases with a
connected vehicle.

Protocol references:

- [go-e HTTP API v2](https://github.com/goecharger/go-eCharger-API-v2/blob/main/http-en.md)
- [go-e API keys](https://github.com/goecharger/go-eCharger-API-v2/blob/main/API_KEYS_FIRMWARE/apikeys-en.md)
- [Home Assistant go-e integration](https://github.com/marq24/ha-goecharger-api2)
  ([API key mappings](https://github.com/marq24/ha-goecharger-api2/blob/main/custom_components/goecharger_api2/pygoecharger_ha/keys.py),
  [sensor units and indexes](https://github.com/marq24/ha-goecharger-api2/blob/main/custom_components/goecharger_api2/const.py)).

The new driver is covered by the repository's MIT license. It does not reuse the
separately licensed upstream go-e implementation.
