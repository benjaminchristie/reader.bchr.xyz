# reader.bchr.xyz

Terminal client for [bchr.xyz](https://bchr.xyz) rooms: chat, send and receive files. It speaks the same
end-to-end encrypted protocol as the website, so you can talk to people in the browser from your terminal.

```sh
go build -o reader .

./reader https://bchr.xyz/#/k3v9...          # join a room from an invite link and chat
./reader k3v9...                             # same thing, just the room code
./reader send https://bchr.xyz/#/k3v9... *.pdf   # share files and exit
./reader -login ben -dir ~/Downloads/bchr https://bchr.xyz/#/k3v9...
```

Create a room on the website ("New private room" → Invite) and pass its link. Any string works as a room code,
but only random ones (like the website generates) are private. A room called `test` can be guessed.

In a room:

```
  <text>              send a message
  /send <files...>    encrypt and share files ('quotes' or \ for spaces, globs ok)
  /get <n...> | all   download shared files by number
  /files              list files shared since you joined
  /name               show your name (names come from accounts: use -login)
  /link               show the invite link
  /quit               leave (Ctrl-C and Ctrl-D work too)
```

Files shared by others are saved to `-dir` (default `./files`) automatically. Turn that off with `-autosave=false`
and use `/get`. Existing files are never overwritten.

### Scripts and services

When stdin isn't a terminal, the reader prints messages as plain lines and sends each input line, commands included.
When stdin closes, it keeps listening. Examples:

```sh
./reader "$ROOM" < /dev/null >> room.log            # receive files forever (the original use)
echo "build finished" | BCHR_PASSWORD=… ./reader -login ci "$ROOM"   # post as the account "ci" (Ctrl-C / kill to stop)
```

Without `-login` the reader gets a guest name (anon-1234) from the server. Either way the server vouches for your
name, and names it can't vouch for are shown as `name (unverified)`.

The reader understands everything the website sends:
- replies are shown with the message they answer;
- forwarded, disappearing, edited and deleted messages are labelled;
- emotes are shown as actions, alongside joins, leaves and admin announcements.

Reactions and typing indicators are skipped. It also takes part in presence, so web users see it under "Who's here"
and `/who` in the reader lists everyone in the room.

In rooms created with history sharing (their links start with `h-`), the reader shows the conversation from
before it joined, replayed by the server.

It also shows polls (answer with `/vote <n>`), `/nuke` votes (run by the server: `/nuke` to agree, `/keep` to keep the room; everyone wipes together when a majority
agrees), pins, shared timers, spoilers (`/reveal`), voice notes, and notices, freezes and bans from the site's
admins. `/poll`, `/away`, `/back`, `/verify` (the same safety words as the website), `/report`, `/flag` and
`/feature` work from the terminal too.

Forum links (they start with `f-`) open as a forum: the reader lists the threads, `/read <n>` shows one,
`/post <n> <text>` replies and `/thread <title> | <text>` starts a new one. New threads and replies from others
show up as they're posted.

Rooms can have a name, a password and rules set by their owner on the website. The reader shows the name,
topic and welcome note when it joins; for a password room pass `-password` (or `BCHR_ROOM_PASSWORD`), or it asks.

### Keeping a room going

History rooms (links starting with `h-`, or customized rooms with **Share history** on) show newcomers the last 500
messages, but the server only keeps them while someone is in the room, and forgets two minutes after it empties.
For a long-running group chat, `keep-room.sh` keeps the reader in the room and restarts it if it stops:

```sh
KEEPER_LOGIN=archive BCHR_PASSWORD=… KEEPER_INVISIBLE=1 ./keep-room.sh "https://bchr.xyz/#/h-yourroom…"
```

- `KEEPER_LOGIN` / `BCHR_PASSWORD`: a separate account for the keeper (optional; without it the keeper is a guest).
- `KEEPER_INVISIBLE=1`: the keeper doesn't show up as here or count in `/nuke` votes (needs an account).
- `BCHR_ROOM_PASSWORD`: the room's password, for password rooms.
- `KEEPER_LOG=room.log`: also save what's said, as plain text on that machine (guard it).

To run it for good, e.g. with systemd (`~/.config/systemd/user/keep-room.service`, then
`systemctl --user enable --now keep-room`):

```ini
[Service]
Environment=KEEPER_LOGIN=archive BCHR_PASSWORD=… KEEPER_INVISIBLE=1
ExecStart=/path/to/keep-room.sh https://bchr.xyz/#/h-yourroom…
Restart=always

[Install]
WantedBy=default.target
```

Limits: the history lives in the server's memory, so a server restart (or a `/nuke`) still clears it, and only the
last 1000 messages (8 MB) are kept. Files stay as long as the room is in use. For a room that keeps everything through
restarts, ask the site's admins to **keep it alive** (then no keeper is needed).

The reader reconnects by itself when the connection drops. The one exception is when the site's admins block the room:
then it prints "this room has been closed" and exits with status 1.

