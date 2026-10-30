# Dashboard visual review

These are screenshots of the running collector's disposable **synthetic** preview,
not real installation measurements and not values copied from the design mockup.
The preview explicitly labels its sender as synthetic.

- [Overview, dark](overview-dark.png)
- [Overview, light](overview-light.png)
- [Overview, mobile](overview-mobile.png)
- [HEMS schedule and decisions](hems.png)
- [ESP32 runtime](runtime.png)
- [Devices and provider history](devices.png)

Reproduce from the repository root with `go run ./scripts/preview`, then
`npm ci && npm run test:browser`. Browser artifacts go to
`/tmp/hems-dashboard-artifacts` by default.
