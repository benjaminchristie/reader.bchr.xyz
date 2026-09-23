#!/bin/sh
# Keeps a room's conversation around by staying in it with the reader.
#
# History rooms (links starting with h-, or customized rooms with "Share
# history" on) show newcomers the last 500 messages, but the server only keeps
# them while someone is in the room: this script is that someone. It runs the
# reader quietly (no files downloaded) and starts it again if it ever stops.
#
#   ./keep-room.sh https://bchr.xyz/#/h-yourroom...
#
# Optional, through the environment:
#   READER=/path/to/reader           the reader binary (default: ./reader next to this script)
#   KEEPER_LOGIN=name                sign in as this account (use a separate one, e.g. "archive")
#   BCHR_PASSWORD=...                that account's password
#   KEEPER_INVISIBLE=1               don't show up as here (needs KEEPER_LOGIN)
#   BCHR_ROOM_PASSWORD=...           the room's password, for password rooms
#   KEEPER_LOG=/path/to/room.log     keep what's said in a file (it's plain text: guard it)
set -u
room=${1:?usage: keep-room.sh <room link> [more reader flags]}
shift
here=$(cd "$(dirname "$0")" && pwd)
reader=${READER:-$here/reader}
log=${KEEPER_LOG:-/dev/null}

set -- -autosave=false "$@"
if [ -n "${KEEPER_LOGIN:-}" ]; then
	set -- -login "$KEEPER_LOGIN" "$@"
	[ -n "${KEEPER_INVISIBLE:-}" ] && set -- -invisible "$@"
fi

while :; do
	"$reader" "$@" "$room" < /dev/null >> "$log" 2>&1
	echo "keep-room: reader stopped (exit $?), starting again in 30s" >&2
	sleep 30
done
