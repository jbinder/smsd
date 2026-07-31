# smsd

A small, native Linux tray daemon that mirrors your Android phone's SMS to a
local SQLite database over `adb` and gives you a read-only, searchable history.
Built for Arch Linux + i3wm. No Electron, no Qt, no Python, no root, and no app
installed on the phone.

## What it does

- Starts the `adb` server automatically and detects phones via `adb devices`.
- Connects automatically whenever a phone is plugged in; disconnects are handled
  gracefully without crashing.
- Imports SMS into SQLite, never duplicating a message.
- Notifies (via libnotify) only for **newly received** messages — the first
  import of a phone's history is silent.
- Caches contacts from the Android Contacts Provider so conversations show names.
- Read-only conversation viewer in your browser, with search by phone number,
  contact name, or message body.
- System tray icon — an SMS speech bubble in four states: hollow grey when no
  device is attached, filled green when connected, blue on unread messages, and
  red with an exclamation mark on error. Its menu offers Open SMS History,
  Refresh, Mark notifications read, Reconnect and Quit.

### Efficient polling

smsd does **not** re-read the whole SMS table every couple of seconds. It:

1. Monitors device state with the cheap local `adb devices` call and only polls
   SMS **while a device is connected**.
2. Tracks the highest imported `_id` per device and queries only newer rows
   (`content query ... --where '_id>N'`). Steady-state polling transfers only the
   handful of new messages, so it stays fast and light even with tens of
   thousands of stored messages.

Goroutines are used with per-device `context` cancellation and ticker-based
waits — there is no busy-waiting.

## Requirements

Runtime:

- `adb` (Arch: `android-tools`)
- `libnotify` (`notify-send`) for desktop notifications
- `xdg-utils` (`xdg-open`) to open the viewer
- A **StatusNotifierItem (SNI)** tray host to display the icon — see
  [Tray under i3](#tray-under-i3). Note that **i3bar's built-in tray does not
  work**: it only speaks the older XEmbed protocol, not SNI.

SQLite is compiled in via the pure-Go `modernc.org/sqlite` driver — there is no
`libsqlite3` runtime dependency and the binary needs no cgo.

Build: Go 1.24+.

### Tray under i3

smsd publishes its tray icon over D-Bus using the StatusNotifierItem (SNI)
protocol, which modern trays speak. i3bar's tray is XEmbed-only and cannot render
SNI items, so the icon will be missing until you run a bridge. The simplest fix
on Arch is [`snixembed`], which proxies SNI items into i3bar's XEmbed tray:

```sh
sudo pacman -S snixembed          # official 'extra' repo, no AUR needed
```

Start it with your session by adding this to `~/.config/i3/config` (near your
`bar { }` / system-tray section), then reload i3 with `$mod+Shift+r`:

```
exec --no-startup-id snixembed
```

`snixembed` adopts already-running items, so start order does not matter. Verify
it is active with:

```sh
busctl --user list | grep StatusNotifierWatcher   # owned by snixembed
```

On Wayland/Sway, `waybar`'s `tray` module speaks SNI natively and needs no
bridge.

[`snixembed`]: https://git.sr.ht/~steef/snixembed

## Phone setup

1. Enable **Developer options** and **USB debugging** on the phone.
2. Plug in over USB and accept the "Allow USB debugging" prompt.
3. Confirm the host sees it: `adb devices` should list the phone as `device`.

No companion app is required. smsd only issues standard, non-root `content query`
commands against `content://sms` and the contacts provider.

## Build & install

```sh
make            # -> bin/smsd (static, cgo-free)
make test       # unit tests
sudo make install   # -> /usr/local/bin + systemd user unit
```

Or install the Arch package:

```sh
makepkg -si     # uses the provided PKGBUILD
```

Enable the background service (starts with your graphical session):

```sh
systemctl --user daemon-reload
systemctl --user enable --now smsd.service
journalctl --user -u smsd -f      # follow logs (also written to the log file below)
```

### Autostart under a bare window manager (i3, sway, …)

The unit is `WantedBy=graphical-session.target`, which full desktop environments
activate for you. Plain window managers do **not** — so an enabled smsd silently
never starts at login. You cannot start that target by hand either; systemd
marks it `RefuseManualStart`, so it may only be pulled in as a dependency.

Add a session target that pulls it in, `~/.config/systemd/user/i3-session.target`:

```ini
[Unit]
Description=i3 session
BindsTo=graphical-session.target
Wants=graphical-session-pre.target
After=graphical-session-pre.target
```

and start it from your WM config (`~/.config/i3/config`):

```
exec --no-startup-id "systemctl --user import-environment DISPLAY XAUTHORITY; systemctl --user start i3-session.target"
```

The quotes matter: i3 splits config lines on `;` unless the command is quoted.
`import-environment` is what lets smsd open the viewer in your browser — the user
manager may be started before X (and stays alive across logout if you have
lingering enabled), so it does not otherwise know `DISPLAY`.

For sway, use `exec` with the same two commands and name the target
`sway-session.target`. Verify with:

```sh
systemctl --user is-active graphical-session.target smsd.service   # active, active
```

### Installing without root

You can install to your home directory instead of using `sudo`:

```sh
install -Dm755 bin/smsd ~/.local/bin/smsd
install -Dm644 systemd/smsd.service ~/.config/systemd/user/smsd.service
```

The unit uses `ExecStart=smsd`, resolved from the systemd **user** manager's
`PATH` — which is typically only `/usr/local/bin:/usr/bin` and does **not**
include `~/.local/bin`. If the service fails with "executable not found", point
it at the absolute path:

```sh
sed -i "s|^ExecStart=smsd|ExecStart=%h/.local/bin/smsd|" ~/.config/systemd/user/smsd.service
systemctl --user daemon-reload
```

## Files

| Purpose        | Location                               |
| -------------- | -------------------------------------- |
| Configuration  | `~/.config/smsd/config.json`           |
| SQLite database| `~/.local/share/smsd/smsd.db`          |
| Log file       | `~/.local/state/smsd/log.txt`          |

`XDG_CONFIG_HOME`, `XDG_DATA_HOME` and `XDG_STATE_HOME` are honoured.

### Configuration

`config.json` is written with defaults on first run:

```json
{
  "adb_path": "",
  "device_poll_seconds": 2,
  "sms_poll_seconds": 2,
  "contacts_refresh_minutes": 15,
  "notify_enabled": true,
  "notify_timeout_seconds": 30,
  "ui_addr": "127.0.0.1:0",
  "log_max_bytes": 5242880
}
```

- `adb_path` — override the `adb` binary; empty means look it up on `PATH`.
- `ui_addr` — loopback bind address for the viewer; port `0` picks a free port.
- `notify_timeout_seconds` — how long a notification stays on screen. Use a
  negative value to keep it up until dismissed. Your notification daemon has
  the last word: dunst honours the hint (unless overridden by a rule), GNOME
  Shell ignores it.

## The viewer

Selecting **Open SMS History** from the tray starts a small loopback web server
(if not already running) and opens it in your browser via `xdg-open`, which
honours `$BROWSER` and your XDG default. Notes:

- By default `ui_addr` is `127.0.0.1:0`, so the port is chosen at random each
  run. The URL is written to the log, e.g.
  `viewer available at http://127.0.0.1:36783/` — you can open it manually in any
  browser. Set a fixed port (e.g. `"ui_addr": "127.0.0.1:8730"`) if you want a
  stable URL.
- The first launch after login can be slow if your browser is not already
  running; the browser is launched fire-and-forget, so a failed launch is silent.
- The server binds to loopback only and is read-only.

### Time window

Histories run to tens of thousands of messages over years, which no browser
renders comfortably in one pass. The viewer's main lever is a **time window**,
set by the *Show* selector in the header and defaulting to the **last month**:

- The window applies to everything. A conversation with no traffic inside it does
  not appear in the list at all, and the per-conversation message count describes
  the window rather than all time.
- The list footer reports what is in view (`8 conversations in the last month`)
  and offers **Show everything**, so a short list never reads as missing data.
- Opening a thread loads only its in-window messages. A **↑ Load messages older
  than the last month** button at the top widens that thread to the full history.
- The choice is remembered in `localStorage` across sessions.

Within the window everything still pages, so a busy month cannot flood the page:
the list loads 100 conversations at a time as you scroll, and a thread loads its
newest 200 messages with a **↑ Load 200 older** button. Paging is keyset-based on
`(date, android_id)`, so it stays correct even when messages share a timestamp.

The viewer also polls `/api/state` — a message count and highest row id — every
five seconds and only redraws when that changes, leaving scroll position and the
selected conversation untouched while idle.

The JSON API takes the same parameters, should you want to script against it
(`since` is a millisecond epoch floor; omit or pass `0` for the whole history):

```sh
curl "$URL/api/state"
curl "$URL/api/conversations?since=1780000000000&limit=100&offset=0"
curl "$URL/api/messages?address=%2B15551234567&since=1780000000000&limit=200"
curl "$URL/api/messages?address=%2B15551234567&before_date=…&before_id=…"
```

`has_more` in the `/api/messages` response reports whether older messages remain
*inside the requested window*; drop `since` from the next request to read past it.

## Architecture

```
cmd/smsd            entry point, signal handling, wiring
internal/config     XDG paths + JSON config
internal/logging    rotating file logger (log.txt)
internal/sms        SMS model + content-query parser  (unit-tested)
internal/database   SQLite schema, dedup import, contacts, search  (unit-tested)
internal/adb        adb client: server, devices, sms/contacts queries
internal/notify     libnotify (notify-send) notifications
internal/tray       StatusNotifierItem tray + generated state icons
internal/ui         embedded read-only web viewer served on loopback
internal/app        device monitor + per-device incremental sync loops
```

The code is deliberately modular: adding **MMS** or **sending SMS** later means a
new query/writer in `internal/adb` plus storage in `internal/database`, without
touching the tray, notifier, or viewer.

## Limitations

- Read-only: smsd never sends or deletes messages on the phone.
- SMS only (no MMS/RCS yet).
- Requires the phone unlocked and USB debugging authorised for the host.

## License

MIT — see [LICENSE](LICENSE).
