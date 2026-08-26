# Development scripts

`restart-lumi-app.sh` is the development loop between Lumi.app builds. It gracefully quits the running app, rebuilds and installs it to `~/Applications`, resets the Screen Recording and Accessibility TCC entries invalidated by the new ad-hoc signature, then launches the new build.

Every step names `~/Applications/Lumi.app` explicitly instead of using `lumi app --quit`, which resolves `/Applications` first and so quits the wrong bundle on a machine that also has the released copy. It sends `lumi://quit` and then *waits* — `open` returns before the confirmation sheet and the recorder's 20s SIGTERM stop are done — and aborts rather than escalating to a kill or reinstalling over a live copy.

It is a local development helper, not a release or user-facing install script. The reset removes grants; re-enable them in System Settings after running it.
