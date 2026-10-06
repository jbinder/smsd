# smsd

A small, native Linux tray daemon that mirrors your Android phone's SMS to a
local SQLite database over `adb` and gives you a searchable history you can
reply from.
Built for Arch Linux + i3wm. No Electron, no Qt, no Python, no root, and no app
installed on the phone.

> [!WARNING]
> **Turn off RCS ("Chat features") in Google Messages, or smsd will silently miss
> messages.** RCS chat messages are not SMS: Google Messages keeps them in its
> own private database, which adb cannot read without root. They never appear in
> smsd — no error, no notification, nothing in the log. With RCS off, everything
> arrives as SMS and is imported normally.
>
> In Google Messages: **Settings → RCS chats → Turn on RCS chats: off**. On a phone
> with several users, do this in the **primary user's** Google Messages — that
> is where SMS land, whichever user is active. Messages that already arrived
> over RCS cannot be recovered by smsd.

## What it does

- Starts the `adb` server automatically and detects phones via `adb devices`.
- Connects automatically whenever a phone is plugged in; disconnects are handled
  gracefully without crashing.
- Imports SMS into SQLite, never duplicating a message.
- Mirrors the phone's contacts — names, numbers, e-mail and postal addresses,
  organisation, websites, dates, nicknames and notes.
- **Never deletes anything.** A message or contact deleted on the phone (or
  removed by a sync from elsewhere) stays in the database and is marked as
  deleted; if it comes back, the mark is cleared.
- Notifies (via libnotify) only for **newly received** messages — the first
  import of a phone's history is silent.
- Mirrors the phone's **call log**, raises an **Incoming call** notification
  while the phone rings and turns it into a **Missed call** alert if nobody
  answers — see [Calls](#calls).
- Viewer in your browser: conversations with search by phone number, contact
  name, or message body, and a Contacts tab with each contact's card.
- Sends SMS from the viewer — replies and new messages — through the phone,
  still with no app installed on it.
- System tray icon — an SMS speech bubble in four states: hollow grey when no
  device is attached, filled green when connected, blue on unread messages or missed calls, and
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

Deletions cannot be seen in that incremental stream, so every
`deleted_check_minutes` smsd also lists the ids of all messages on the phone
(ids only — no bodies, a couple of seconds for ~17k messages) and marks stored
messages that are no longer there (and likewise for the call log). Contacts are small enough to be re-read in
full every `contacts_refresh_minutes`.

An empty answer from the phone is never taken to mean "everything was
deleted" — that sync is skipped and logged instead.

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
4. **Turn off RCS chats in Google Messages** (in the primary user, on a
   multi-user phone) — see the warning at the top. RCS messages are invisible
   to smsd.

No companion app is required. smsd only issues standard, non-root `content query`
commands against `content://sms`, the contacts provider and the call log, and
reads the call state with `dumpsys telephony.registry`.

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
  "deleted_check_minutes": 15,
  "send_enabled": true,
  "notify_enabled": true,
  "notify_timeout_seconds": 30,
  "ui_addr": "127.0.0.1:0",
  "log_max_bytes": 5242880
}
```

- `adb_path` — override the `adb` binary; empty means look it up on `PATH`.
- `ui_addr` — loopback bind address for the viewer; port `0` picks a free port.
- `send_enabled` — allow sending SMS from the viewer.
- `contacts_refresh_minutes` — how often contacts are re-synced.
- `deleted_check_minutes` — how often the phone is checked for deleted messages.
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
- The server binds to loopback only. Its only write is sending an SMS, which
  it accepts from the viewer page alone — see [Sending](#sending).

### Sending

Open a conversation and type in the box under it, or use **✎ New message** and
pick a contact or type a number. A contact card's numbers you have not
texted yet get a **Write SMS →** button. **Ctrl+Enter** or **Send** sends;
plain Enter is a new line, so a message cannot go out by accident. The counter
shows how many SMS the text takes: 160 characters fit one message in the GSM
alphabet (153 per part once split); a single character outside it, such as an
emoji or a typographic dash, makes it 70 (67 per part). Longer text goes out as
one concatenated message.

There is no app on the phone to hand the message to, so smsd calls Android's
telephony service directly as the adb shell user, which is allowed to send SMS
(`adb shell service call isms …`). Android then files the message in the sent
folder itself, and the regular import brings it into the viewer within a few
seconds; until then it shows as *sending…*. A message the network refuses shows
as *not sent*. A reply goes out from the phone the conversation was last on;
formatting is stripped from numbers (`+44 7843 174004` → `+447843174004`) so a
reply lands in the same conversation as the messages it answers.

Caveats:

- The service is internal to Android and its call numbers can change between
  releases. smsd only sends on versions it has been checked against — currently
  **Android 13** — and says so rather than guessing on anything else.
- "Sent" means the phone accepted the message, not that it was delivered.
- Set `"send_enabled": false` to turn sending off.

A loopback server that can send SMS is a target: any web page open in your
browser can make it POST to `127.0.0.1`. The send endpoint therefore requires a
random per-run token that only the viewer page carries, a loopback `Host`
(against DNS rebinding) and, when present, a loopback `Origin`.

### Calls

The **Calls** tab lists the phone's call log — incoming, outgoing, missed
(in red), rejected and blocked calls with their duration — under the same
*Show* window as the conversations. Selecting a call shows every call with that
number, plus **Messages →** or **Write SMS →**. Calls cleared from the phone's
log are kept and marked deleted, like messages.

Alerts, all through libnotify and subject to `notify_enabled`:

- **Incoming call** — shown while the phone rings, with the contact name when
  the number is known, and kept on screen until the ringing stops. Answering
  removes it.
- **Missed call** — an unanswered call replaces the incoming-call notification
  with a missed-call alert, which uses `notify_timeout_seconds` like an SMS.
  Missed calls also turn the tray icon blue until **Mark notifications read**.
  Missed calls that happened while the phone was unplugged are alerted on when
  it reconnects (with their time); the first import of a phone's call log is
  silent. Rejected calls are not alerted on.

How it works, still with no app on the phone: every poll, smsd reads the
phone's call state from `dumpsys telephony.registry` (a tenth of a second,
filtered on the phone to three lines per SIM). When a call ends it reads the
call log (`content://call_log/calls`, newer ids only) for the next few polls;
otherwise the log is re-read once a minute, which catches calls too short to be
seen ringing. Dual-SIM phones are covered. Alerts lag the phone by up to
`sms_poll_seconds`.

Closing the incoming-call notification uses `gdbus` (part of glib2); without
it the notification is replaced by one that expires at once.

### Contacts and deletions

The **Contacts** tab lists every contact smsd has seen, with a filter for all
contacts, those on the phone, or those deleted from it. A contact's card shows
its details grouped by kind; numbers you have exchanged SMS with get a
**Messages →** button that opens that conversation across its whole history.
The same number held by several accounts on the phone (Google, a messenger, …)
is shown once.

Deleted contacts, details and messages are shown with a *deleted* marker. The
date given is when smsd noticed, which can be later than the actual deletion
if the phone was not connected at the time. A deleted contact still names its
old conversations, but a contact still on the phone wins if both have the
number. Edits made on the phone — a renamed contact, a corrected number —
update the stored copy in place.

Databases from earlier versions, which only cached number → name pairs, are
upgraded on the first contact sync of each phone: numbers no longer on the
phone become deleted contacts rather than being dropped.

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
curl "$URL/api/contacts"
curl "$URL/api/contact?id=42"
curl "$URL/api/calls?since=1780000000000&limit=200"
curl "$URL/api/calls?number=%2B15551234567"     # one number's calls
curl "$URL/api/calls?before_date=…&before_id=…" # older page
```

Messages, contacts and contact details carry `deleted_at` (millisecond epoch)
once they have been marked deleted; the field is absent otherwise.

`has_more` in the `/api/messages` response reports whether older messages remain
*inside the requested window*; drop `since` from the next request to read past it.

## Architecture

```
cmd/smsd            entry point, signal handling, wiring
internal/config     XDG paths + JSON config
internal/logging    rotating file logger (log.txt)
internal/sms        SMS model + content-query parser  (unit-tested)
internal/contact    contact model + content-query parser  (unit-tested)
internal/call       call-log model, content-query and call-state parsers  (unit-tested)
internal/database   SQLite schema, dedup import, contacts, deletion marks, search  (unit-tested)
internal/adb        adb client: server, devices, sms/contacts queries
internal/notify     libnotify (notify-send) notifications
internal/tray       StatusNotifierItem tray + generated state icons
internal/ui         embedded web viewer served on loopback, incl. sending
internal/app        device monitor + per-device incremental sync loops
```

The code is deliberately modular: adding **MMS** later means a new query in
`internal/adb` plus storage in `internal/database`, without touching the tray
or notifier. Supporting sending on another Android release means adding its
`ISms` transaction codes to the table in `internal/adb`.

## Limitations

- smsd never deletes messages on the phone or writes to its contacts; its
  only change to the phone is sending the SMS you write.
- Sending needs a supported Android version (currently 13) and a SIM.
- SMS and calls only. MMS is not imported yet, and **RCS chat messages can never be**
  (they are not readable over adb) — keep RCS turned off.
- Requires the phone unlocked and USB debugging authorised for the host.

## License

MIT — see [LICENSE](LICENSE).
