# Development scripts

`restart-lumi-app.sh` is the development loop between Lumi.app builds. It gracefully quits the running app, rebuilds and installs it to `~/Applications`, resets the Screen Recording and Accessibility TCC entries invalidated by the new ad-hoc signature, then launches the new build.

It is a local development helper, not a release or user-facing install script. The reset removes grants; re-enable them in System Settings after running it.
