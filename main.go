package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const usageText = `reader - terminal client for bchr.xyz rooms (end-to-end encrypted)

usage:
  reader [flags] <room>                  chat in a room and receive files
  reader [flags] send <room> <file>...   share files and exit

<room> is an invite link (https://bchr.xyz/#/abc123...) or just the part after #/.
A link to another server (e.g. http://localhost:8080/#/abc) also selects that server.
When stdin is not a terminal, lines read from it are sent (commands included) and
received messages are printed, which makes it usable from scripts and as a service.

flags:
`

const helpText = `commands:
  <text>              send a message
  /me <action>        say what you're doing: * ben waves
  /who                see who's in the room
  /away, /back        show as away (or here) in "Who's here"
  /poll <q> | <a> | <b>   ask the room; /vote <n> answers the latest poll
  /reveal             show the latest spoiler
  /verify             safety words: compare them to be sure you're in the same room
  /nuke, /keep        vote to wipe the room or keep it (the server counts; a majority must agree)
  /report <reason>    report the room to the site's admins (they don't get the link)
  /flag <reason>      ask an admin to join (sends them the link; the room is told)
  /feature <idea>     suggest a feature to the site's admins
forums (links starting with f-):
  /threads            list the threads
  /read <n>           show a thread and its replies
  /post <n> <text>    reply to thread n
  /thread <title> | <text>   start a thread
  /send <files...>    encrypt and share files ('quotes' or \ for spaces, globs ok)
  /get <n...> | all   download shared files by number
  /files              list files shared since you joined
  /name               show your name (sign in with -login to use your username)
  /link               show the invite link
  /quit               leave (Ctrl-C and Ctrl-D work too)`

var roomPassword *string

func main() {
	server := flag.String("server", "https://bchr.xyz", "server to use when <room> is not a full link")
	dir := flag.String("dir", "files", "directory to save received files to")
	login := flag.String("login", "", "sign in as this user, so your username is your name (password from BCHR_PASSWORD or a prompt); otherwise you're anon-NNNN")
	invisible := flag.Bool("invisible", false, "don't show up as here or count in /nuke votes (needs -login), e.g. to keep a history room going")
	autosave := flag.Bool("autosave", true, "download files shared by others automatically")
	page := flag.String("page", "", "room link or code (alternative to the <room> argument)")
	roomPassword = flag.String("password", "", "the room's password, for rooms that have one (or BCHR_ROOM_PASSWORD; asked for otherwise)")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usageText)
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	send := len(args) > 0 && args[0] == "send"
	if send {
		args = args[1:]
	}
	roomArg := *page
	if roomArg == "" {
		if len(args) == 0 {
			flag.Usage()
			os.Exit(2)
		}
		roomArg, args = args[0], args[1:]
	}
	if !send {
		// allow flags after the room too: reader <room> -login ben
		flag.CommandLine.Parse(args)
		args = flag.Args()
		if len(args) > 0 {
			fatalf("unexpected arguments: %s", strings.Join(args, " "))
		}
	} else if len(args) == 0 {
		fatalf("send: no files given")
	}
	srv, secret, err := ParseRoom(roomArg, *server)
	if err != nil {
		fatalf("%v", err)
	}
	room, err := NewRoom(srv, secret)
	if err != nil {
		fatalf("%v", err)
	}
	if *roomPassword == "" {
		*roomPassword = os.Getenv("BCHR_ROOM_PASSWORD")
	}
	if *roomPassword != "" {
		room.SetPassword(*roomPassword)
	}
	// only signed-in people have names; everyone else is a guest (anon-NNNN).
	// Either way the server vouches for the name, so others see it as verified.
	name := fmt.Sprintf("anon-%04d", 1000+rand.Intn(9000))
	if *login != "" {
		var token string
		if name, token, err = signIn(srv, *login); err != nil {
			fatalf("%v", err)
		}
		room.SignedIn(token)
		room.Invisible = *invisible
	} else if *invisible {
		fatalf("-invisible needs -login (only accounts can be invisible)")
	} else if guest, token, err := guestName(srv); err == nil {
		name = guest
		room.AsGuest(token)
	} else {
		fmt.Fprintf(os.Stderr, "warning: %v; others will see %s as unverified\n", err, name)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	// password rooms: ask (up to three times) before going in
	for tries := 0; ; tries++ {
		err := room.Initialize(ctx)
		if !errors.Is(err, ErrPassword) {
			break
		}
		if tries == 3 || (tries == 0 && *roomPassword != "" && !isTerminal(int(os.Stdin.Fd()))) {
			fatalf("wrong password")
		}
		pw, perr := askSecret("Room password: ", "this room has a password: use -password or BCHR_ROOM_PASSWORD")
		if perr != nil {
			fatalf("%v", perr)
		}
		room.SetPassword(pw)
	}

	if send {
		os.Exit(runSend(ctx, room, name, args))
	}
	app := &App{
		room:      room,
		name:      name,
		dir:       *dir,
		autosave:  *autosave,
		uploads:   make(chan string, 1024),
		downloads: make(chan Chat, 1024),
		outbox:    make(chan Chat, 256),
		joined:    make(chan struct{}),
		closed:    make(chan struct{}),
		peers:     map[string]peer{},
		seenMIDs:  map[string]bool{},
		polls:     map[string]*poll{},
	}
	app.run(ctx)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "reader: "+format+"\n", args...)
	os.Exit(1)
}

// runSend uploads files one after another, printing progress to stderr.
func runSend(ctx context.Context, room *Room, name string, paths []string) int {
	if err := room.Initialize(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "reader: %v\n", err)
		return 1
	}
	failed := 0
	for _, path := range expandPaths(paths) {
		base := filepath.Base(path)
		last := -1
		_, err := room.Upload(ctx, name, path, func(f float64) {
			if pct := int(f * 100); pct != last {
				last = pct
				fmt.Fprintf(os.Stderr, "\r%s %3d%%", base, pct)
			}
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "\r%s failed: %v\n", base, err)
			failed++
		} else {
			fmt.Fprintf(os.Stderr, "\r%s sent    \n", base)
		}
	}
	fmt.Fprintf(os.Stderr, "room: %s\n", room.Link())
	if failed > 0 {
		return 1
	}
	return 0
}

type App struct {
	room     *Room
	name     string // guarded by mu; use getName
	dir      string
	autosave bool
	ui       *UI

	mu    sync.Mutex
	files []Chat

	uploads        chan string
	downloads      chan Chat
	outbox         chan Chat // messages to send, in order
	pendingUploads atomic.Int32
	quitWarned     bool
	joined         chan struct{} // closed on the first successful connection
	joinOnce       sync.Once
	closed         chan struct{}    // closed when an admin blocks the room
	peers          map[string]peer  // by session id; guarded by mu
	log            []Chat           // recent messages, passed on to newcomers in history rooms; guarded by mu
	polls          map[string]*poll // by message id; guarded by mu
	lastPoll       string
	lastSpoiler    string
	forum          *forumState // set for forum links; guarded by mu
	away           atomic.Bool
	seenMIDs       map[string]bool // content already shown (a replay after reconnecting repeats some); guarded by mu
	replayLeft     int             // history rooms: earlier messages still to come from the server; guarded by mu
	replayedOnce   bool
}

// what's passed on to newcomers in history rooms (a message is at most 1 MiB)
const (
	historyMaxItems = 200
)

type peer struct {
	by       string // who the server says it is; only they can change this entry
	name     string
	lastSeen time.Time
	away     bool
}

// forum state, replayed from the stored events
type forumThread struct {
	chat    Chat
	replies []Chat
	deleted bool
}

type forumState struct {
	title   string
	expires time.Time
	threads []*forumThread
	byMID   map[string]*forumThread
}

type poll struct {
	chat  Chat
	votes map[string]Vote // by session id
}

// presence: say "here" this often, forget people not heard from for longer
const (
	heartbeatEvery = time.Minute
	peerTimeout    = 150 * time.Second
)

func (a *App) run(ctx context.Context) {
	interactive := isTerminal(int(os.Stdin.Fd())) && isTerminal(int(os.Stdout.Fd()))
	a.ui = NewUI(interactive)

	var restore func()
	if interactive {
		var ok bool
		restore, ok = makeRaw(int(os.Stdin.Fd()))
		if !ok {
			interactive = false
			a.ui = NewUI(false)
		}
	}
	shutdown := func(code int) {
		a.sayLeave()
		if restore != nil {
			restore()
		}
		if interactive {
			fmt.Println()
		}
		os.Exit(code)
	}
	go func() {
		select {
		case <-ctx.Done(): // SIGTERM / SIGHUP (Ctrl-C arrives as a key in raw mode)
			shutdown(130)
		case <-a.closed:
			shutdown(1)
		}
	}()

	u := a.ui
	u.Print(u.style("1", "bchr.xyz") + u.style("2", " · end-to-end encrypted room"))
	u.Print(u.style("2", "invite: ") + a.room.Link())
	saving := "files are not downloaded automatically"
	if a.autosave {
		saving = "files save to " + a.dir
	}
	u.Print(u.style("2", fmt.Sprintf("you are %s · %s · /help for commands", a.getName(), saving)))

	if a.room.IsForum() {
		if err := a.loadForum(ctx); err != nil {
			u.Print(u.style("31", err.Error()))
			shutdown(1)
		}
	}
	go a.subscribeLoop(ctx)
	go a.heartbeat(ctx)
	go a.sendLoop(ctx)
	go a.uploadLoop(ctx)
	go a.downloadLoop(ctx)

	in := bufio.NewReader(os.Stdin)
	for {
		var line string
		var err error
		if interactive {
			line, err = u.ReadLine(in)
		} else {
			line, err = in.ReadString('\n')
			if err == io.EOF && line != "" {
				err = nil
			}
			line = strings.TrimRight(line, "\r\n")
		}
		if err == errInterrupt || (err == io.EOF && interactive) {
			if a.confirmQuit() {
				shutdown(0)
			}
			continue
		}
		if err != nil {
			// stdin closed (pipe finished or running as a service): keep
			// listening; the goroutine above exits (and says goodbye) on a signal
			select {}
		}
		if a.command(strings.TrimSpace(line)) {
			shutdown(0)
		}
	}
}

// confirmQuit asks for a second /quit while uploads are still running.
func (a *App) confirmQuit() bool {
	if a.pendingUploads.Load() == 0 || a.quitWarned {
		return true
	}
	a.quitWarned = true
	a.ui.Print(a.ui.style("33", "uploads are still in progress; quit again to abort them"))
	return false
}

// command handles one line of input and reports whether to quit.
func (a *App) command(line string) bool {
	if line == "" {
		return false
	}
	if a.room.IsForum() && (!strings.HasPrefix(line, "/") || strings.HasPrefix(line, "//")) {
		a.ui.Print("this is a forum: reply with /post <n> <text>, start a thread with /thread <title> | <text>, list them with /threads")
		return false
	}
	if !strings.HasPrefix(line, "/") || strings.HasPrefix(line, "//") {
		a.quitWarned = false
		a.outbox <- Chat{ID: a.getName(), Data: strings.TrimPrefix(line, "/")}
		return false
	}
	args := splitArgs(line)
	cmd, args := args[0], args[1:]
	u := a.ui
	switch cmd {
	case "/quit", "/exit", "/q":
		return a.confirmQuit()
	case "/help", "/?":
		u.Print(helpText)
	case "/send", "/upload":
		paths := expandPaths(args)
		if len(paths) == 0 {
			u.Print("usage: /send <files...>")
		}
		for _, p := range paths {
			a.pendingUploads.Add(1)
			a.uploads <- p
		}
	case "/get", "/download":
		a.get(args)
	case "/files":
		a.mu.Lock()
		files := append([]Chat(nil), a.files...)
		a.mu.Unlock()
		if len(files) == 0 {
			u.Print("no files shared yet")
		}
		for i, f := range files {
			u.Printf("  #%d  %s  %s  from %s", i+1, clean(f.Filename, false), sizeOf(f), clean(f.ID, false))
		}
	case "/name":
		u.Printf("you are %s (names come from accounts: start the reader with -login <username>)", a.getName())
	case "/link", "/invite":
		u.Print(a.room.Link())
	case "/threads":
		a.printThreads()
	case "/read":
		if t := a.threadArg(args); t != nil {
			a.printThread(t)
		}
	case "/post":
		t := a.threadArg(args)
		text := ""
		if len(args) > 1 {
			text = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, cmd), " "+args[0]))
		}
		if t == nil || text == "" {
			if t != nil {
				u.Print("usage: /post <n> <text>")
			}
			break
		}
		a.forumSend(Chat{ID: a.getName(), Type: "post", Thread: t.chat.MID, Data: text})
	case "/thread":
		parts := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(line, cmd)), "|", 2)
		title := strings.TrimSpace(parts[0])
		if title == "" || !a.room.IsForum() {
			u.Print("usage: /thread <title> | <text> (in a forum)")
			break
		}
		body := ""
		if len(parts) == 2 {
			body = strings.TrimSpace(parts[1])
		}
		a.forumSend(Chat{ID: a.getName(), Type: "thread", Title: title, Data: body})
	case "/who":
		u.Print(a.who())
	case "/away", "/afk", "/brb":
		a.away.Store(true)
		go a.signal(context.Background(), Chat{Type: "presence", Presence: "here", Away: true})
		u.Print("you show as away (/back to show as here)")
	case "/back":
		a.away.Store(false)
		go a.signal(context.Background(), Chat{Type: "presence", Presence: "here"})
		u.Print("you show as here again")
	case "/reveal":
		a.mu.Lock()
		s := a.lastSpoiler
		a.mu.Unlock()
		if s == "" {
			u.Print("no spoilers yet")
		} else {
			u.Print(u.style("2", "spoiler: ") + clean(s, true))
		}
	case "/verify":
		u.Print("safety words: " + u.style("1", strings.Join(a.room.Words, " ")) +
			u.style("2", " (everyone in this room sees the same four; compare them to be sure)"))
	case "/poll":
		var q string
		var opts []string
		for i, part := range strings.Split(strings.TrimSpace(strings.TrimPrefix(line, cmd)), "|") {
			if part = strings.TrimSpace(part); part == "" {
				continue
			} else if i == 0 {
				q = part
			} else {
				opts = append(opts, part)
			}
		}
		if q == "" || len(opts) == 1 || len(opts) > 10 {
			u.Print("usage: /poll question | option | option ... (2-10 options; none means yes/no)")
			break
		}
		if len(opts) == 0 {
			opts = []string{"Yes", "No"}
		}
		a.outbox <- Chat{ID: a.getName(), Type: "poll", Data: q, Options: opts}
	case "/vote":
		n, err := strconv.Atoi(strings.Join(args, ""))
		a.mu.Lock()
		p := a.polls[a.lastPoll]
		a.mu.Unlock()
		if err != nil || p == nil || n < 1 || n > len(p.chat.Options) {
			u.Print("usage: /vote <option number> (answers the latest poll)")
			break
		}
		choice := n - 1
		a.outbox <- Chat{ID: a.getName(), Type: "vote", Target: p.chat.MID, Choice: &choice}
	case "/nuke", "/wipe", "/keep":
		// the server runs the vote: one vote per network address in the room
		wipe := cmd != "/keep"
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := a.room.Nuke(ctx, wipe); err != nil {
				u.Print(u.style("31", "vote not counted: "+err.Error()))
			}
		}()
	case "/report", "/flag", "/feature":
		reason := strings.TrimSpace(strings.TrimPrefix(line, cmd))
		if reason == "" {
			u.Printf("usage: %s <reason>", cmd)
			break
		}
		kind := strings.TrimPrefix(cmd, "/")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := a.room.Report(ctx, kind, reason, a.getName()); err != nil {
				u.Print(u.style("31", "not sent: "+err.Error()))
				return
			}
			switch kind {
			case "flag":
				a.outbox <- Chat{ID: a.getName(), Type: "system", Data: a.getName() + " asked the site's admins to join this room. They now have the link."}
				u.Print("an admin has been asked to join; the room has been told")
			case "feature":
				u.Print("thanks! your idea has been sent to the admins")
			default:
				u.Print("the admins have been told about this room")
			}
		}()
	case "/me":
		if len(args) == 0 {
			u.Print("usage: /me <action>")
			break
		}
		a.outbox <- Chat{ID: a.getName(), Type: "emote", Data: strings.TrimSpace(strings.TrimPrefix(line, cmd))}
	default:
		u.Printf("unknown command %s (try /help, or start with // to send a message beginning with /)", cmd)
	}
	return false
}

func (a *App) get(args []string) {
	a.mu.Lock()
	files := append([]Chat(nil), a.files...)
	a.mu.Unlock()
	if len(args) == 1 && args[0] == "all" {
		for _, f := range files {
			a.downloads <- f
		}
		return
	}
	if len(args) == 0 {
		a.ui.Print("usage: /get <n...> | all   (see /files)")
	}
	for _, arg := range args {
		n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
		if err != nil || n < 1 || n > len(files) {
			a.ui.Printf("no file #%s (see /files)", arg)
			continue
		}
		a.downloads <- files[n-1]
	}
}

func (a *App) getName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.name
}

func (a *App) subscribeLoop(ctx context.Context) {
	backoff := time.Second
	lastErr := ""
	for ctx.Err() == nil {
		err := a.room.Initialize(ctx)
		if err == nil {
			err = a.room.Subscribe(ctx, func() {
				backoff = time.Second
				lastErr = ""
				a.ui.SetOnline(true)
				a.joinOnce.Do(func() { close(a.joined) })
				go a.signal(ctx, Chat{Type: "presence", Presence: "join"})
			}, a.receive)
		}
		a.ui.SetOnline(false)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrRoomClosed) || errors.Is(err, ErrBanned) {
			a.ui.Print(a.ui.style("31", err.Error()))
			close(a.closed)
			return
		}
		if msg := err.Error(); msg != lastErr {
			a.ui.Print(a.ui.style("33", "connection lost ("+msg+"), reconnecting…"))
			lastErr = msg
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (a *App) isOwn(c Chat) bool {
	return c.SID == a.room.SID || (c.SID == "" && c.ID == a.getName())
}

func (a *App) receive(c Chat) {
	u := a.ui
	switch c.Type {
	case "replay": // history rooms: the server replays what was said before we came
		n, _ := strconv.Atoi(c.Data)
		a.mu.Lock()
		a.replayLeft = n
		first := !a.replayedOnce
		a.replayedOnce = true
		a.mu.Unlock()
		if n > 0 && first {
			u.Print(u.style("2", "── sent before you joined ──"))
		}
		return
	case "settings": // the room's name, look and rules
		card, _ := a.room.OpenCard(c.Data)
		head := strings.TrimSpace(clean(card.Emoji+" "+card.Name, false))
		if head == "" {
			head = "room"
		}
		if card.Topic != "" {
			head += " — " + clean(card.Topic, false)
		}
		u.Print(u.style("1", "── "+head+" ──") + u.style("2", " owner "+clean(c.ID, false)))
		if c.Title != "" {
			u.Print(u.style("2", c.Title))
		}
		if card.Welcome != "" {
			u.Print(clean(card.Welcome, true))
		}
		return
	case "archive": // the admins keep (or stop keeping) this room's messages on the server
		if c.Data == "" {
			u.Print(u.style("2", "🗄 this room is no longer kept alive"))
		} else {
			u.Print(u.style("2", "🗄 this room is kept alive ("+c.Data+"): messages and files stay when everyone leaves"))
		}
		return
	case "cleared": // the site's admins deleted the files shared so far
		until, _ := time.Parse(time.RFC3339Nano, c.Until)
		a.mu.Lock()
		kept := a.files[:0]
		for _, f := range a.files {
			if sent, err := time.Parse(time.RFC3339Nano, f.Timestamp); err == nil && sent.After(until) {
				kept = append(kept, f)
			}
		}
		gone := len(a.files) - len(kept)
		a.files = kept
		a.mu.Unlock()
		if gone > 0 {
			u.Print(u.style("1;36", fmt.Sprintf("🛡 the site's admins deleted the files shared here (%d)", gone)))
		}
		return
	case "here": // who's here, from the server
		me := a.getName() // (takes mu itself)
		a.mu.Lock()
		for _, n := range c.Options {
			if n != me {
				a.peers["srv:"+n] = peer{by: n, name: clean(n, false), lastSeen: time.Now()}
			}
		}
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	replayed := a.replayLeft > 0
	if replayed {
		a.replayLeft--
	}
	content := c.MID != "" && (c.Type == "" || c.Type == "msg" || c.Type == "emote" || c.Type == "system" || c.Type == "poll" || c.Type == "timer")
	dup := content && a.seenMIDs[c.MID]
	if content {
		a.seenMIDs[c.MID] = true
	}
	a.mu.Unlock()
	if dup {
		return
	}
	own := a.isOwn(c)
	when, name := timeOf(c), displayName(c)
	if !own && c.SID != "" && !replayed {
		a.seen(c)
	}
	switch c.Type {
	case "typing", "react":
		return // not shown in the terminal
	case "presence":
		switch {
		case own:
		case c.Presence == "join":
			u.Print(u.style("2", when+" "+name+" joined"))
			// let them know we're here, like the website does
			go func() {
				time.Sleep(time.Duration(200+rand.Intn(2500)) * time.Millisecond)
				a.signal(context.Background(), Chat{Type: "presence", Presence: "here"})
			}()
		case c.Presence == "leave":
			u.Print(u.style("2", when+" "+name+" left"))
		}
		return
	case "history":
		return // members used to pass history on themselves; the server does it now
	case "nukevote":
		if c.Nuke { // passed: the server already deleted the files
			a.mu.Lock()
			a.log = nil
			a.files = nil
			a.mu.Unlock()
			u.Print(u.style("1;31", "💥 the room was wiped: "+c.Data+"; messages and files are gone"))
		} else {
			u.Print(u.style("1;31", "💥 "+displayName(c)+" wants to wipe this room: all messages and files, for everyone. ") +
				u.style("2", c.Data+" (/nuke to agree, /keep to keep it)"))
		}
		return
	case "vote":
		a.applyVote(c)
		return
	case "forum", "thread", "post":
		a.applyForum(c, true)
		return
	case "pin":
		if c.On != nil && !*c.On {
			u.Print(u.style("2", when+" "+name+" unpinned a message"))
		} else if m := a.findLog(c.Target); m != nil {
			u.Print(u.style("2", when+" "+name+" pinned: ") + clean(preview(*m), false))
		} else {
			u.Print(u.style("2", when+" "+name+" pinned a message"))
		}
		return
	case "notice":
		u.Print(u.style("1;36", "🛡 notice from the site's admins: "+clean(c.Data, false)))
		return
	case "state":
		if t, err := time.Parse(time.RFC3339Nano, c.Until); err == nil {
			u.Print(u.style("36", "❄ the admins froze this room until "+t.Local().Format("15:04")+"; nobody can post"))
		} else {
			u.Print(u.style("2", "the room is open again"))
		}
		return
	case "announce":
		u.Print(u.style("1;33", "📣 "+clean(c.Data, false)) + u.style("2", " — "+name))
		return
	case "edit":
		a.editLog(c, false)
		u.Print(u.style("2", when+" "+name+" edited a message: ") + clean(c.Data, false))
		return
	case "delete":
		a.editLog(c, true)
		u.Print(u.style("2", when+" "+name+" deleted a message"))
		return
	}
	a.remember(c)
	a.show(c, replayed)
}

func timeOf(c Chat) string {
	if t, err := time.Parse(time.RFC3339Nano, c.Timestamp); err == nil {
		return t.Local().Format("15:04")
	}
	return "--:--"
}

// show prints a message of the conversation (also ones from history).
func (a *App) show(c Chat, fromHistory bool) {
	u := a.ui
	own := a.isOwn(c)
	when, name := timeOf(c), displayName(c)
	switch c.Type {
	case "system":
		u.Print(u.style("2", when+" -- "+clean(c.Data, false)))
		return
	case "emote":
		u.Print(u.style("2", when) + " * " + u.nameStyle(name, own) + " " + clean(c.Data, false))
		return
	}
	switch c.Type {
	case "poll":
		if c.Nuke {
			return // /nuke votes from older clients: the server runs those now
		}
		a.trackPoll(c)
		var opts []string
		for i, o := range c.Options {
			opts = append(opts, fmt.Sprintf("%d) %s", i+1, clean(o, false)))
		}
		u.Print(u.style("2", when) + " 📊 " + u.nameStyle(name, own) + " asks: " + u.style("1", clean(c.Data, false)) +
			"  " + strings.Join(opts, "  ") + u.style("2", "  (/vote <n>)"))
		return
	case "timer":
		end, err := time.Parse(time.RFC3339Nano, c.Until)
		if err != nil {
			return
		}
		label := clean(c.Data, false)
		left := time.Until(end).Round(time.Second)
		if left <= 0 {
			u.Print(u.style("2", when+" ⏲ "+name+"'s timer "+label+" (already over)"))
			return
		}
		u.Print(u.style("2", when) + " ⏲ " + u.nameStyle(name, own) + " started a " + left.String() + " timer " + u.style("1", label))
		time.AfterFunc(left, func() { u.Print(u.style("1;33", "⏰ time's up "+label)) })
		return
	}
	head := u.style("2", when) + " " + u.nameStyle(name, own) + " "
	if c.Spoiler {
		a.mu.Lock()
		a.lastSpoiler = c.Data
		a.mu.Unlock()
		u.Print(head + u.style("2", "sent a spoiler (/reveal to show it)"))
		return
	}
	if c.ReplyTo != nil {
		u.Print(u.style("2", "      ↪ "+clean(c.ReplyTo.ID, false)+": "+clean(c.ReplyTo.Text, false)))
	}
	if c.Fwd != "" {
		head += u.style("2", "(forwarded from "+clean(c.Fwd, false)+") ")
	}
	if c.TTL > 0 {
		head += u.style("2", fmt.Sprintf("(disappears in %ds) ", int(c.TTL)))
	}
	if c.Edited {
		head += u.style("2", "(edited) ")
	}
	if c.IsFile {
		a.mu.Lock()
		a.files = append(a.files, c)
		n := len(a.files)
		a.mu.Unlock()
		what := "shared"
		if c.Voice {
			what = "sent a voice note"
		}
		u.Print(head + fmt.Sprintf("%s %s %s %s", what, u.style("1", clean(c.Filename, false)),
			u.style("2", "("+sizeOf(c)+")"), u.style("2", fmt.Sprintf("#%d", n))))
		if a.autosave && !own && !fromHistory {
			select {
			case a.downloads <- c:
			default:
				u.Printf("too many downloads queued, use /get %d later", n)
			}
		}
		return
	}
	// indent continuation lines under the message
	indent := "\n" + strings.Repeat(" ", 6+len([]rune(name))+1)
	u.Print(head + strings.ReplaceAll(clean(c.Data, true), "\n", indent))
}

// ------------------------------------------------------------------ history

// remember keeps a message for newcomers (history rooms only).
func (a *App) remember(c Chat) {
	if !a.room.SharesHistory() || c.MID == "" || c.TTL > 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.log = append(a.log, c)
	if len(a.log) > historyMaxItems {
		a.log = a.log[len(a.log)-historyMaxItems:]
	}
}

// editLog applies an edit or delete from the message's sender to the log.
func (a *App) editLog(c Chat, remove bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, m := range a.log {
		// the sender the server vouched for decides (the session id can be copied)
		same := m.SID == c.SID
		if m.By != "" {
			same = m.By == c.By
		}
		if m.MID == c.Target && same {
			if remove {
				a.log = append(a.log[:i], a.log[i+1:]...)
			} else {
				a.log[i].Data, a.log[i].Edited = c.Data, true
			}
			return
		}
	}
}

// waitJoined holds back the first messages until we are subscribed, so that
// lines piped in at startup are also shown back to us (briefly, not forever).
func (a *App) waitJoined(ctx context.Context) {
	select {
	case <-a.joined:
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
	}
}

// ---------------------------------------------------------------- presence

func (a *App) seen(c Chat) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// presence updates say who's here; other messages only refresh people we
	// know (invisible users send no presence, so they stay unlisted)
	known, ok := a.peers[c.SID]
	if c.Hidden || (c.Type != "presence" && !ok) {
		return
	}
	// only whoever the entry belongs to can change it (a "leave" for someone
	// else, say); the server says who sent each message
	if ok && known.by != "" && known.by != c.By {
		return
	}
	if c.By != "" {
		delete(a.peers, "srv:"+c.By)
	}
	if c.Type == "presence" && c.Presence == "leave" {
		delete(a.peers, c.SID)
	} else {
		away := a.peers[c.SID].away
		if c.Type == "presence" {
			away = c.Away
		}
		a.peers[c.SID] = peer{by: c.By, name: displayName(c), lastSeen: time.Now(), away: away}
	}
}

func (a *App) signal(ctx context.Context, c Chat) {
	if a.room.Invisible && c.Type == "presence" {
		return // invisible: nobody hears from us unless we post
	}
	c.ID = a.getName()
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	a.room.Signal(sctx, c)
}

func (a *App) heartbeat(ctx context.Context) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.signal(ctx, Chat{Type: "presence", Presence: "here", Away: a.away.Load()})
			a.mu.Lock()
			for sid, p := range a.peers {
				if time.Since(p.lastSeen) > peerTimeout {
					delete(a.peers, sid)
				}
			}
			a.mu.Unlock()
		}
	}
}

// sayLeave tells the room we're going (best effort, on exit).
func (a *App) sayLeave() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.signal(ctx, Chat{Type: "presence", Presence: "leave"})
}

// displayName is the sender's name, marked when the server didn't vouch for it.
func displayName(c Chat) string {
	if c.Unverified {
		return clean(c.ID, false) + " (unverified)"
	}
	return clean(c.ID, false)
}

func (a *App) who() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	me := a.name + " (you)"
	if a.away.Load() {
		me += " (away)"
	}
	names := []string{me}
	for _, p := range a.peers {
		if p.away {
			names = append(names, p.name+" (away)")
		} else {
			names = append(names, p.name)
		}
	}
	sort.Strings(names[1:])
	return fmt.Sprintf("%d here: %s", len(names), strings.Join(names, ", "))
}

// ------------------------------------------------------------------ forums

func (a *App) loadForum(ctx context.Context) error {
	info, err := a.room.ForumInfo(ctx)
	if err != nil {
		return err
	}
	events, err := a.room.ForumLog(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.forum = &forumState{expires: info.Expires, byMID: map[string]*forumThread{}}
	a.mu.Unlock()
	for _, e := range events {
		a.applyForum(e, false)
	}
	a.mu.Lock()
	title := a.forum.title
	a.mu.Unlock()
	if title == "" {
		title = "Untitled forum"
	}
	left := time.Until(info.Expires)
	when := fmt.Sprintf("%d days", int(left.Hours()/24))
	if left < 48*time.Hour {
		when = fmt.Sprintf("%d hours", int(left.Hours()))
	}
	a.ui.Print(a.ui.style("1", "📋 "+clean(title, false)) + a.ui.style("2", " · forum, kept for "+when+" more"))
	a.printThreads()
	return nil
}

// applyForum replays a forum event; live ones are also printed.
func (a *App) applyForum(c Chat, live bool) {
	u := a.ui
	a.mu.Lock()
	f := a.forum
	if f == nil {
		a.mu.Unlock()
		return
	}
	var msg string
	switch c.Type {
	case "forum":
		if f.title == "" {
			f.title = c.Title
		}
	case "thread":
		if c.MID != "" && f.byMID[c.MID] == nil {
			t := &forumThread{chat: c}
			f.threads = append(f.threads, t)
			f.byMID[c.MID] = t
			msg = fmt.Sprintf("📋 new thread #%d: %s by %s", len(f.threads), clean(c.Title, false), displayName(c))
		}
	case "post":
		if t := f.byMID[c.Thread]; t != nil {
			for _, r := range t.replies {
				if r.MID == c.MID {
					a.mu.Unlock()
					return
				}
			}
			t.replies = append(t.replies, c)
			msg = fmt.Sprintf("↪ %s replied in %q: %s", displayName(c), clean(t.chat.Title, false), clean(preview(c), false))
		}
	}
	a.mu.Unlock()
	if live && msg != "" {
		u.Print(msg)
	}
}

func (a *App) printThreads() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.forum == nil {
		a.ui.Print("this isn't a forum")
		return
	}
	if len(a.forum.threads) == 0 {
		a.ui.Print(a.ui.style("2", "no threads yet; start one with /thread <title> | <text>"))
		return
	}
	for i, t := range a.forum.threads {
		a.ui.Printf("  #%d  %s  %s", i+1, clean(t.chat.Title, false),
			a.ui.style("2", fmt.Sprintf("by %s · %d replies", clean(t.chat.ID, false), len(t.replies))))
	}
	a.ui.Print(a.ui.style("2", "/read <n> to open one"))
}

func (a *App) threadArg(args []string) *forumThread {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.forum == nil {
		a.ui.Print("this isn't a forum")
		return nil
	}
	n := 0
	if len(args) > 0 {
		n, _ = strconv.Atoi(strings.TrimPrefix(args[0], "#"))
	}
	if n < 1 || n > len(a.forum.threads) {
		a.ui.Print("no such thread (see /threads)")
		return nil
	}
	return a.forum.threads[n-1]
}

func (a *App) printThread(t *forumThread) {
	u := a.ui
	a.mu.Lock()
	posts := append([]Chat{t.chat}, t.replies...)
	a.mu.Unlock()
	u.Print(u.style("1", "── "+clean(t.chat.Title, false)+" ──"))
	for _, p := range posts {
		u.Print(u.style("2", timeOf(p)+" ") + u.nameStyle(displayName(p), a.isOwn(p)) + " " + clean(p.Data, true))
		for _, f := range p.Files {
			u.Print(u.style("2", "      📎 "+clean(f.Filename, false)))
		}
	}
}

func (a *App) forumSend(c Chat) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := a.room.ForumPost(ctx, c); err != nil {
			a.ui.Print(a.ui.style("31", "not posted: "+err.Error()))
		}
	}()
}

// ------------------------------------------------------------------- polls

func (a *App) trackPoll(c Chat) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := &poll{chat: c, votes: map[string]Vote{}}
	for _, v := range c.Votes {
		p.votes[v.SID] = v
	}
	a.polls[c.MID] = p
	a.lastPoll = c.MID
}

// applyVote records a poll vote: one per sender the server vouched for, so
// votes under made-up session ids don't count.
func (a *App) applyVote(c Chat) {
	if c.Choice == nil || c.By == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.polls[c.Target]
	if p == nil {
		return
	}
	delete(p.votes, c.By)
	if *c.Choice >= 0 && *c.Choice < len(p.chat.Options) {
		p.votes[c.By] = Vote{SID: c.By, Name: c.By, Choice: *c.Choice}
	}
}

func (a *App) findLog(mid string) *Chat {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.log {
		if a.log[i].MID == mid {
			c := a.log[i]
			return &c
		}
	}
	return nil
}

func preview(c Chat) string {
	if c.IsFile {
		return c.Filename
	}
	if r := []rune(c.Data); len(r) > 80 {
		return string(r[:79]) + "…"
	}
	return c.Data
}

func (a *App) sendLoop(ctx context.Context) {
	a.waitJoined(ctx)
	for chat := range a.outbox {
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := a.room.Publish(pctx, chat); err != nil {
			a.ui.Print(a.ui.style("31", "message not sent: "+err.Error()))
		}
		cancel()
	}
}

func (a *App) uploadLoop(ctx context.Context) {
	a.waitJoined(ctx)
	for path := range a.uploads {
		base := clean(filepath.Base(path), false)
		last := -1
		_, err := a.room.Upload(ctx, a.getName(), path, func(f float64) {
			if pct := int(f * 100); pct != last {
				last = pct
				a.ui.SetStatus("up", fmt.Sprintf("↑ %s %d%%", base, pct))
			}
		})
		a.ui.SetStatus("up", "")
		a.pendingUploads.Add(-1)
		if err != nil {
			a.ui.Print(a.ui.style("31", fmt.Sprintf("could not send %s: %v", base, err)))
		}
	}
}

func (a *App) downloadLoop(ctx context.Context) {
	for c := range a.downloads {
		base := clean(safeFilename(c.Filename), false)
		last := -1
		path, err := a.room.Download(ctx, c, a.dir, func(f float64) {
			if pct := int(f * 100); pct != last {
				last = pct
				a.ui.SetStatus("down", fmt.Sprintf("↓ %s %d%%", base, pct))
			}
		})
		a.ui.SetStatus("down", "")
		if err != nil {
			a.ui.Print(a.ui.style("31", fmt.Sprintf("could not download %s: %v", base, err)))
		} else {
			a.ui.Print(a.ui.style("2", "      saved "+path))
		}
	}
}

func sizeOf(c Chat) string {
	if c.Size == nil {
		return "? B"
	}
	return formatBytes(*c.Size)
}

// expandPaths expands ~ and glob patterns; unmatched patterns are kept so the
// upload reports a useful "no such file" error.
func expandPaths(args []string) []string {
	var out []string
	for _, p := range args {
		if p == "~" || strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, strings.TrimPrefix(p, "~"))
			}
		}
		if strings.ContainsAny(p, "*?[") {
			if matches, err := filepath.Glob(p); err == nil && len(matches) > 0 {
				out = append(out, matches...)
				continue
			}
		}
		out = append(out, p)
	}
	return out
}
