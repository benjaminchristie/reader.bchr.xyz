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

The reader reconnects by itself when the connection drops. The one exception is when the site's admins block the room:
then it prints "this room has been closed" and exits with status 1.

