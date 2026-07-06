# arcmux launchd service (macOS)

On Linux (labs) arcmux is supervised by a systemd `--user` unit. On macOS the
equivalent supervisor is **launchd**: a per-user LaunchAgent that starts the
daemon at login/boot (`RunAtLoad`) and restarts it if it dies (`KeepAlive`).

## Install

```bash
make service-install     # render plist -> ~/Library/LaunchAgents, bootstrap + kickstart
make status              # confirm the daemon is running
```

`service-install` substitutes `@HOME@ / @BIN@ / @LOG_FILE@ / @PATH@` in
`com.blin.arcmux.plist` and writes the result to
`~/Library/LaunchAgents/com.blin.arcmux.plist`, then loads it with
`launchctl bootstrap gui/$UID`. It is idempotent — re-run after a `make install`
to pick up a new binary path or PATH.

Logs go to `~/.config/arcmux/logs/daemon.log` (same file `make logs`/`make tail`
read).

## Uninstall

```bash
make service-uninstall   # bootout + remove the plist
```

## How it cooperates with the Makefile

Once the agent is loaded, the Makefile detects it (`HAS_LAUNCHD_AGENT`) and
routes `start` / `stop` / `restart` through `launchctl`, exactly like it defers
to `systemctl` when a systemd unit exists. This prevents a manual `nohup` daemon
from racing launchd's `KeepAlive` respawn.
