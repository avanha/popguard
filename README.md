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

Stdlib only. Linux (uses `flock`).

## Build

```sh
go build -o popguard .          # or: CGO_ENABLED=0 go build -trimpath -ldflags=-s -o popguard .
sudo install -m 755 popguard /usr/local/bin/
go test -race ./...             # fake POP3/SMTP servers, including a stalling one, over TLS too
```

## Config

`~/.config/popguard/yahoo.json` (chmod 600 if it contains a password):

```json
{
  "server": "pop.mail.yahoo.com:995",
  "user": "andrev@yahoo.com",
  "password_file": "~/.config/popguard/yahoo.pass",

  "smtp_server": "localhost:25",
  "deliver_to": ["avanha@localhost"],

  "idle_timeout": "60s",
  "poison_threshold": 2,
  "poison_action": "skip",
  "poison_retry_after": "7d",
  "commit_every": 10
}
```

Put the Yahoo app password alone in the password file (`chmod 600`).

| Key | Default | Meaning |
|---|---|---|
| `server` | — | `host:port`; port defaults to 995 (110 with `"tls": false`) |
| `tls` | `true` | implicit TLS (POP3S); certificates are verified |
| `user`, `password` / `password_file` | — | credentials |
| `smtp_server` + `deliver_to` | — | deliver over SMTP (like fetchmail's default) |
| `mda` | — | instead of SMTP, e.g. `["/usr/lib/dovecot/dovecot-lda", "-d", "avanha", "-f", "%F"]`; `%F` = envelope sender (sanitized, never starts with `-`) |
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
popguard -config ~/.config/popguard/yahoo.json          # fetch
popguard -config ~/.config/popguard/yahoo.json -v       # with protocol trace (password redacted)
popguard -config ~/.config/popguard/yahoo.json -list    # what's on the server + local status, downloads nothing
popguard -config ~/.config/popguard/yahoo.json -status  # local state only: failing/poison messages
popguard -config ~/.config/popguard/yahoo.json -reset <uid>   # or -reset all: retry poison
```

cron, replacing the Yahoo poll in fetchmail (keep fetchmail for Gmail):

```
*/5 * * * * /usr/local/bin/popguard -config $HOME/.config/popguard/yahoo.json >> $HOME/popguard.log 2>&1
```

It's quiet when there's nothing to do. A poison message logs one line per run:

```
POISON skipped: uid AL3x..., 13863 bytes, 2 failures, last 5m0s ago (server stalled: no data from server for 1m0s) From: "Kitten Dispatch" <info@...> | Subject: Gourmet Cat Treats!
```

Use the From/Subject to find it in webmail and trash it, or set `"poison_action": "delete"`.
The bytes that did arrive are in the quarantine directory.

Exit status: 0 = fine (including skipped poison and "another run is going"), 1 = error, 2 = bad config.

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

## Ordering guarantees

1. state ← "retrieving uid" → `RETR`
2. state ← "delivering uid" → SMTP/MDA
3. state ← "delivered uid" → `DELE`
4. every `commit_every` DELEs: `QUIT` (commits), reconnect

If anything dies between 3 and 4, the next session sees a delivered UIDL still
on the server and deletes it without redelivering. If it dies during 2, the
message is fetched again (a possible duplicate, never a loss).
