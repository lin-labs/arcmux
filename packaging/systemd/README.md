# arcmux systemd user service (Linux)

Install or refresh the service from the runtime checkout:

```bash
make install
make service-install
```

The unit intentionally uses `KillMode=process`. arcmux's tmux server owns the
durable agent panes, so a daemon release or restart must stop only the arcmux
main process. The replacement daemon then restores the surviving sessions from
its inventory. Headless exec agents remain daemon-owned: arcmux drains their
dedicated process groups before exiting, so only tmux-backed conversations
survive a restart.

Confirm the installed policy with:

```bash
systemctl --user show arcmux.service -p KillMode
```
