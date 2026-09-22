# reader.bchr.xyz

Terminal client for [bchr.xyz](https://bchr.xyz) rooms: chat, send and receive files. It speaks the same
end-to-end encrypted protocol as the website, so you can talk to people in the browser from your terminal.

```sh
go build -o reader .

./reader https://bchr.xyz/#/k3v9...          # join a room from an invite link and chat
./reader k3v9...                             # same thing, just the room code
./reader send https://bchr.xyz/#/k3v9... *.pdf   # share files and exit
./reader -name ben -dir ~/Downloads/bchr https://bchr.xyz/#/k3v9...
```

Create a room on the website ("New private room" → Invite) and pass its link. Any string works as a room code,
but only random ones (like the website generates) are private. A room called `test` can be guessed.

In a room:

```
  <text>              send a message
  /send <files...>    encrypt and share files ('quotes' or \ for spaces, globs ok)
  /get <n...> | all   download shared files by number
  /files              list files shared since you joined
  /name <name>        change your display name
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
echo "build finished" | ./reader -name ci "$ROOM"   # post a message (Ctrl-C / kill to stop)
```

The reader reconnects by itself when the connection drops. The one exception is when the site's admins block the room:
then it prints "this room has been closed" and exits with status 1.

