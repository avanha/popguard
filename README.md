# popguard

A defensive POP3 fetcher for servers that misbehave (looking at you, Yahoo
`jpop-0.1`). It does the fetchmail job — download, hand to your MTA, delete —
but assumes the server can go silent mid-message at any time.

What it does differently from fetchmail:

| Problem | fetchmail | popguard |
|---|---|---|
| Server stops sending mid-message | waits for `timeout`, then retries the same message forever | detects the stall (idle timeout), records the UIDL, moves on to the next message |
| One bad message | blocks the whole mailbox every poll | after `poison_threshold` failures it's skipped (or deleted) on every run |
| Session dies after some DELEs | DELEs are lost → everything re-downloaded → duplicates | every delivered UIDL is remembered; leftovers are deleted without redelivery |
| Long runs | one session, deletes only commit at the end | QUIT/reconnect every `commit_every` deletes |
| Killed by cron/OOM mid-download | no memory of it | that message counts as a failure next run |
| Overlapping cron runs | lock file | `flock`, a second run exits quietly |

Stdlib only. Runs on Linux and macOS (uses `flock`); not tested on other systems.

## Build

```sh
go build -o popguard .          # or: CGO_ENABLED=0 go build -trimpath -ldflags=-s -o popguard .
sudo install -m 755 popguard /usr/local/bin/
```

To build for another platform, set `GOOS` (operating system) and `GOARCH` (CPU
architecture). For example, to build a Linux binary on a Mac:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags=-s -o popguard-linux-amd64 .
# use GOARCH=arm64 for ARM servers such as a Raspberry Pi 4/5 or AWS Graviton
```

`go tool dist list` shows every supported `GOOS/GOARCH` pair. Tests run on the host
platform only:

```sh
go test -race ./...             # fake POP3/SMTP servers, including a stalling one, over TLS too
```

## Config

popguard handles one account per JSON file. By default it processes every
`*.json` file in `$XDG_CONFIG_HOME/popguard/conf.d/` (`~/.config/popguard/conf.d/`
when `XDG_CONFIG_HOME` is unset), in name order; dotfiles and other extensions are
ignored. `-config` takes either a single file or a different directory.

Each account keeps its own state file (see below), and its log lines are prefixed
with the file name, so `yahoo.json` logs as `popguard: yahoo: ...`. Accounts run one
after another, and a failing account doesn't stop the others.

The example below is `~/.config/popguard/conf.d/yahoo.json` (chmod 600 if it contains a password):

```json
{
  "server": "pop.mail.yahoo.com:995",
  "user": "user@yahoo.com",
  "password_file": "~/.config/popguard/yahoo.pass",

  "smtp_server": "localhost:25",
  "deliver_to": ["user@localhost"],

  "idle_timeout": "60s",
  "poison_threshold": 2,
  "poison_action": "skip",
  "poison_retry_after": "7d",
  "commit_every": 10
}
```

Put the Yahoo app password alone in the password file (`chmod 600`). A relative
`password_file` is read relative to the directory of the config file, so
`"password_file": "yahoo.pass"` sits next to `yahoo.json`. Absolute paths and `~/...`
work as written.

| Key | Default | Meaning |
|---|---|---|
| `server` | — | `host:port`; port defaults to 995 (110 with `"tls": false`) |
| `tls` | `true` | implicit TLS (POP3S); certificates are verified |
| `user`, `password` / `password_file` | — | credentials; relative `password_file` is relative to the config file |
| `smtp_server` + `deliver_to` | — | deliver over SMTP (like fetchmail's default) |
| `mda` | — | instead of SMTP, e.g. `["/usr/lib/dovecot/dovecot-lda", "-d", "user", "-f", "%F"]`; `%F` = envelope sender (sanitized, never starts with `-`) |
| `keep` | `false` | leave mail on the server; UIDLs prevent refetching |
| `idle_timeout` | `60s` | no bytes for this long = stall |
| `message_timeout` | `10m` | hard cap for one message |
| `connect_timeout` | `30s` | dial + TLS + greeting |
| `deliver_timeout` | `2m` | per-message SMTP/MDA limit |
| `poison_threshold` | `2` | failures before a message is poison |
| `poison_action` | `skip` | `skip` leaves it on the server, `delete` removes it |
| `poison_retry_after` | `0` (never) | give poison another try this long after its last failure, e.g. `"7d"` |
| `commit_every` | `10` | QUIT and reconnect after this many DELEs |
| `max_stalls` | `3` | after this many failures in one run, commit and stop until next run |
| `max_message_size` | 50 MiB | larger messages are skipped |
| `max_per_run` | `0` | cap downloads per run (0 = unlimited) |
| `in_use_retries`, `in_use_backoff` | `3`, `30s` | retry login when the server says the maildrop is locked |
| `state_file` | `~/.local/state/popguard/<user@host>.json` | per-account memory |
| `quarantine_dir` | `<state dir>/quarantine` | partial downloads of failing messages; `"none"` disables |

Unknown keys are an error, so typos don't silently do nothing.

## Run

```sh
popguard          # fetch
popguard -v       # with protocol trace (password redacted)
popguard -list    # what's on the server + local status, downloads nothing
popguard -status  # local state only: failing/poison messages
popguard -reset <uid>   # or -reset all: retry poison
```

cron, replacing the Yahoo poll in fetchmail (keep fetchmail for Gmail):

```
*/5 * * * * /usr/local/bin/popguard >> $HOME/popguard.log 2>&1
```

On macOS, use a launchd agent instead of cron. Save this as
`~/Library/LaunchAgents/com.popguard.plist` (adjust the paths and user):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.popguard</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/popguard</string>
  </array>
  <key>StartInterval</key>
  <integer>300</integer>
  <key>StandardOutPath</key>
  <string>/Users/you/popguard.log</string>
  <key>StandardErrorPath</key>
  <string>/Users/you/popguard.log</string>
</dict>
</plist>
```

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.popguard.plist   # start
launchctl bootout gui/$(id -u)/com.popguard                                  # stop
```

launchd sets `HOME`, so the default `~/.config/popguard/conf.d` is found. The agent
runs only while you are logged in. If the config dir isn't in `~/.config`, add
`-config /full/path` to `ProgramArguments`.

It's quiet when there's nothing to do. A poison message logs one line per run:

```
POISON skipped: uid AL3x..., 13863 bytes, 2 failures, last 5m0s ago (server stalled: no data from server for 1m0s) From: "Kitten Dispatch" <info@...> | Subject: Gourmet Cat Treats!
```

Use the From/Subject to find it in webmail and trash it, or set `"poison_action": "delete"`.
The bytes that did arrive are in the quarantine directory.

Exit status: 0 = fine (including skipped poison and "another run is going"), 1 = error,
2 = bad config. With several accounts, every account is attempted and the exit status
is the worst one. `-list`, `-status` and `-reset` apply to every account (with
`-list`/`-status` printing an `== name ==` header); add `-config <file>` to target one.

## How it decides something is poison

A message gets a failure recorded when, after `+OK` to `RETR`:

- no bytes arrive for `idle_timeout`, or
- the whole message isn't done within `message_timeout`, or
- the server sends more than 2× the advertised size + 1 MiB, or
- the connection drops, or
- the process was killed while downloading it (detected next run).

A `-ERR` reply to `RETR` also counts, but keeps the connection.

Delivery failures (MTA down, 4xx/5xx) never count against the message; the run
stops, earlier deletes are committed, and the message stays on the server.

## Where state is kept

popguard is stateless between runs except for a small per-account JSON file.
There is no database and no state on the server.

| Path | Contents |
|---|---|
| `$XDG_STATE_HOME/popguard/<user@host>.json` (default `~/.local/state/popguard/…`; override with `state_file`) | the state file, mode 0600 |
| `<state_file>.lock` | `flock` lock file; empty, only there to stop overlapping runs |
| `<state dir>/quarantine/<uid>.partial` (override with `quarantine_dir`, `"none"` disables) | bytes that did arrive for failing messages |

The state file holds, keyed by the server's UIDL:

- `retrieving` / `delivering`: set just before `RETR` and before handing the message
  to the MTA, cleared afterwards. If either is still set at startup, the previous
  run was killed mid-step; a stuck `retrieving` counts as a failure for that message.
- `delivered`: UIDL → time handed to the MTA. If a delivered message is still on
  the server, its `DELE` never committed, so it is deleted again without
  redelivering. Entries are forgotten 7 days after the message leaves the server.
- `failures`: UIDL → failure count, last try time and error, size, a From/Subject
  hint, and the quarantine file. Reaching `poison_threshold` makes a message poison.
  Entries are dropped once the message is gone from the server.

The file is rewritten atomically (temp file, `fsync`, rename) before and after each
risky step. If it is corrupt, popguard refuses to run and tells you to fix or move
it aside. Deleting it is safe but loses the protection above: delivered messages
still on the server will be fetched again, and poison messages will be retried.

Inspect it with `-status` (local only) or `-list` (server plus local status); clear
failures with `-reset <uid>` or `-reset all`.

## Ordering guarantees

1. state ← "retrieving uid" → `RETR`
2. state ← "delivering uid" → SMTP/MDA
3. state ← "delivered uid" → `DELE`
4. every `commit_every` DELEs: `QUIT` (commits), reconnect

If anything dies between 3 and 4, the next session sees a delivered UIDL still
on the server and deletes it without redelivering. If it dies during 2, the
message is fetched again (a possible duplicate, never a loss).
