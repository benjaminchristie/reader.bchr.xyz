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
  /send <files...>    encrypt and share files ('quotes' or \ for spaces, globs ok)
  /get <n...> | all   download shared files by number
  /files              list files shared since you joined
  /name <name>        change your display name
  /link               show the invite link
  /quit               leave (Ctrl-C and Ctrl-D work too)`

func main() {
	server := flag.String("server", "https://bchr.xyz", "server to use when <room> is not a full link")
	dir := flag.String("dir", "files", "directory to save received files to")
	name := flag.String("name", "", "display name (default anon-NNNN)")
	autosave := flag.Bool("autosave", true, "download files shared by others automatically")
	page := flag.String("page", "", "room link or code (alternative to the <room> argument)")
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
		// allow flags after the room too: reader <room> -name ben
		flag.CommandLine.Parse(args)
		args = flag.Args()
		if len(args) > 0 {
			fatalf("unexpected arguments: %s", strings.Join(args, " "))
		}
	} else if len(args) == 0 {
		fatalf("send: no files given")
	}
	if *name == "" {
		*name = fmt.Sprintf("anon-%04d", 1000+rand.Intn(9000))
	}

	srv, secret, err := ParseRoom(roomArg, *server)
	if err != nil {
		fatalf("%v", err)
	}
	room, err := NewRoom(srv, secret)
	if err != nil {
		fatalf("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	if send {
		os.Exit(runSend(ctx, room, *name, args))
	}
	app := &App{
		room:      room,
		name:      *name,
		dir:       *dir,
		autosave:  *autosave,
		uploads:   make(chan string, 1024),
		downloads: make(chan Chat, 1024),
		outbox:    make(chan string, 256),
		joined:    make(chan struct{}),
		closed:    make(chan struct{}),
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
	outbox         chan string
	pendingUploads atomic.Int32
	quitWarned     bool
	joined         chan struct{} // closed on the first successful connection
	joinOnce       sync.Once
	closed         chan struct{} // closed when an admin blocks the room
}

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

	go a.subscribeLoop(ctx)
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
			// stdin closed (pipe finished or running as a service): keep listening
			<-ctx.Done()
			return
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
	if !strings.HasPrefix(line, "/") || strings.HasPrefix(line, "//") {
		a.quitWarned = false
		a.outbox <- strings.TrimPrefix(line, "/")
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
		if len(args) == 0 {
			u.Printf("you are %s", a.getName())
		} else {
			a.mu.Lock()
			a.name = strings.Join(args, " ")
			a.mu.Unlock()
			u.Printf("you are now %s", a.getName())
		}
	case "/link", "/invite":
		u.Print(a.room.Link())
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
			}, a.receive)
		}
		a.ui.SetOnline(false)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrRoomClosed) {
			a.ui.Print(a.ui.style("31", ErrRoomClosed.Error()))
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

func (a *App) receive(c Chat) {
	u := a.ui
	when := "--:--"
	if t, err := time.Parse(time.RFC3339Nano, c.Timestamp); err == nil {
		when = t.Local().Format("15:04")
	}
	own := c.ID == a.getName()
	head := u.style("2", when) + " " + u.nameStyle(clean(c.ID, false), own) + " "
	if c.IsFile {
		a.mu.Lock()
		a.files = append(a.files, c)
		n := len(a.files)
		a.mu.Unlock()
		u.Print(head + fmt.Sprintf("shared %s %s %s", u.style("1", clean(c.Filename, false)),
			u.style("2", "("+sizeOf(c)+")"), u.style("2", fmt.Sprintf("#%d", n))))
		if a.autosave && !own {
			select {
			case a.downloads <- c:
			default:
				u.Printf("too many downloads queued, use /get %d later", n)
			}
		}
		return
	}
	// indent continuation lines under the message
	indent := "\n" + strings.Repeat(" ", 6+len([]rune(clean(c.ID, false)))+1)
	u.Print(head + strings.ReplaceAll(clean(c.Data, true), "\n", indent))
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

func (a *App) sendLoop(ctx context.Context) {
	a.waitJoined(ctx)
	for text := range a.outbox {
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := a.room.SendText(pctx, a.getName(), text); err != nil {
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
