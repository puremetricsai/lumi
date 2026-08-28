# Changelog

## [0.3.2](https://github.com/puremetricsai/lumi/compare/v0.3.1...v0.3.2) (2026-08-28)


### Bug Fixes

* **app:** open Settings through the action that still exists in macOS 26 ([519cf06](https://github.com/puremetricsai/lumi/commit/519cf067e364d49d9af154f7f59defd28e80ea4f))

## [0.3.1](https://github.com/puremetricsai/lumi/compare/v0.3.0...v0.3.1) (2026-08-28)


### Bug Fixes

* **release:** stop importing the Developer ID G2 intermediate ([f245e64](https://github.com/puremetricsai/lumi/commit/f245e64d85b836de86232d1d3287999843c9ac40))
* **release:** stop importing the Developer ID G2 intermediate ([164a1bf](https://github.com/puremetricsai/lumi/commit/164a1bf2da183303ef46cbe7228a4c78a6f08da5))

## [0.3.0](https://github.com/puremetricsai/lumi/compare/v0.2.0...v0.3.0) (2026-08-27)


### ⚠ BREAKING CHANGES

* `brew install --cask puremetricsai/lumi/lumi` is retired. Existing cask installs should `brew uninstall --cask lumi` and re-install with install.sh. The cask's `binary` stanza also symlinked `lumi` onto PATH; that command is gone, and any MCP client configured against `/opt/homebrew/bin/lumi` must be re-registered from the app's MCP tab.
* `brew upgrade --cask` removes the `lumi` command from your PATH, and the release no longer publishes a standalone binary. AI agents registered against the old path stop resolving; re-register them from Settings > MCP, which offers Replace for an entry still naming it. A symlink created by the app's own earlier first-launch offer is not managed by Homebrew, survives the upgrade, and keeps working.

### Features

* **app:** offer an available update from the menu bar and About tab ([afd5133](https://github.com/puremetricsai/lumi/commit/afd5133557c30cfb69382066512f0dc056eef1bc))
* **cli:** add `lumi update` to check for and install a new release ([7bf83a9](https://github.com/puremetricsai/lumi/commit/7bf83a99d4a73ccf13a6066e41ff9941b5ede156))
* **cli:** point the app surface at install.sh and accept target names ([1949047](https://github.com/puremetricsai/lumi/commit/1949047bc4e8d5a4cf87f955772c408b84149d90))
* **config:** derive an update log path beside the record log ([b0b7d59](https://github.com/puremetricsai/lumi/commit/b0b7d59e4de5df1ac309acb3da2f3aa8e673a136))
* make Lumi.app the only product ([3018b1d](https://github.com/puremetricsai/lumi/commit/3018b1d62933e1b9965a8c0cdad93c928d28d316))
* make Lumi.app the only product ([477346c](https://github.com/puremetricsai/lumi/commit/477346c5cddc77831d3d37f7c9e8f71bd518cfc8))
* replace the Homebrew cask with install.sh ([b09c518](https://github.com/puremetricsai/lumi/commit/b09c5189ae156ef4e53de66ab0708b66781753cb))
* tell users a new Lumi exists, and install it for them ([9ff010f](https://github.com/puremetricsai/lumi/commit/9ff010f88b908add002843ddcbfa390e73fcd2fa))


### Bug Fixes

* **release:** gate publication on verification and tag at cut ([30fc1e2](https://github.com/puremetricsai/lumi/commit/30fc1e27ab46474f19536089158b6341bfd867a7))
* **release:** import the Developer ID G2 intermediate before signing ([a948ab8](https://github.com/puremetricsai/lumi/commit/a948ab8cbf1d6a93db1dbfa9ed2e34d8bbdcb5a3))
* **release:** import the Developer ID G2 intermediate before signing ([15e2c4c](https://github.com/puremetricsai/lumi/commit/15e2c4cb6f52d7c2917984cc90c1eedd3c52b6d1))

## [0.2.0](https://github.com/puremetricsai/lumi/compare/v0.1.0...v0.2.0) (2026-08-26)


### Features

* **app:** add a Start/Stop Recording item to the menu bar ([4800d1b](https://github.com/puremetricsai/lumi/commit/4800d1b13d94961dc133f3d9cd91c9c9d499e551))
* **app:** add a Start/Stop Recording item to the menu bar ([60980bd](https://github.com/puremetricsai/lumi/commit/60980bdc534983abbce715ff397e0e08b2a542aa))

## 0.1.0 (2026-08-26)


### Bug Fixes

* **app:** grant the microphone entitlement under the Hardened Runtime ([69c9608](https://github.com/puremetricsai/lumi/commit/69c96086a8e6a7b0c0870aca8b7a185048373089))
* **app:** grant the microphone entitlement under the Hardened Runtime ([e9bfe97](https://github.com/puremetricsai/lumi/commit/e9bfe9732deb70c61283a22c69b875727bbb30f6))


### Miscellaneous Chores

* **release:** restart versioning at 0.1.0 ([81c6208](https://github.com/puremetricsai/lumi/commit/81c62084be4f9b0ac6ef606dd83884287a87f1ad))
