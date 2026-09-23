package main

// Client side of the bchr.xyz room protocol. This must stay byte-for-byte
// compatible with the website (src/app/room-crypto.ts in the bchr.xyz repo):
//
//   secret  --PBKDF2-SHA256(250k, "bchr.xyz/room/v1")-->  master
//   master  --HKDF-SHA256(info "room-id")-->   16 bytes, hex  = room id (all the server sees)
//   master  --HKDF-SHA256(info "room-key")-->  32 bytes      = AES-256-GCM key
//
// Messages are JSON envelopes {"v":1,"iv":b64,"ct":b64} with AAD "msg". Files
// are uploaded as records of [12 byte iv][ciphertext + 16 byte tag] per 1 MiB
// chunk, with AAD "file" || uint32be(index) || byte(isLast).

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

const (
	protocolVersion  = 1
	pbkdf2Iterations = 250000
	pbkdf2Salt       = "bchr.xyz/room/v1"
	ivBytes          = 12
	tagBytes         = 16
	fileChunkBytes   = 1 << 20
	fileRecordBytes  = ivBytes + fileChunkBytes + tagBytes
	maxFileBytes     = 510 << 20 // same limit as the website (nginx allows 512M)
)

var fileIDRegexp = regexp.MustCompile("^[A-Za-z0-9_-]{1,128}$")

// ErrRoomClosed means the site's admins blocked the room: the server answers
// 410 Gone and closes connections with status 4001.
var ErrRoomClosed = errors.New("this room has been closed by the site's admins")

// ErrBanned means the site's admins banned this address (403, close 4002).
var ErrBanned = errors.New("you have been banned from bchr.xyz")

const (
	statusRoomClosed websocket.StatusCode = 4001
	statusBanned     websocket.StatusCode = 4002
)

// Chat is the plaintext of every message. Field names match the website
// (src/app/chat-item/chat-item.component.ts); everything after Mime is optional
// and was added later.
type Chat struct {
	ID        string   `json:"id"`
	Timestamp string   `json:"timestamp,omitempty"`
	Data      string   `json:"data,omitempty"`
	IsFile    bool     `json:"isFile,omitempty"`
	Filename  string   `json:"filename,omitempty"`
	FileID    string   `json:"fileId,omitempty"`
	Size      *float64 `json:"size,omitempty"`
	Mime      string   `json:"mime,omitempty"`

	// msg (or empty), emote, system, edit, delete, react, typing, presence;
	// "announce" is only used locally for the server's announcements
	Type     string    `json:"type,omitempty"`
	MID      string    `json:"mid,omitempty"` // message id
	SID      string    `json:"sid,omitempty"` // sender's session id
	ReplyTo  *ReplyRef `json:"replyTo,omitempty"`
	Fwd      string    `json:"fwd,omitempty"`      // original author of a forwarded message
	TTL      float64   `json:"ttl,omitempty"`      // seconds until it disappears (web clients delete it)
	Target   string    `json:"target,omitempty"`   // message an edit/delete/react applies to
	Emoji    string    `json:"emoji,omitempty"`    // react
	On       *bool     `json:"on,omitempty"`       // react/pin: added (true) or removed (false)
	Presence string    `json:"presence,omitempty"` // join, here, leave

	// history rooms: a newcomer's join asks for the conversation so far, and
	// someone already in the room sends it as a "history" message
	WantHistory bool   `json:"wantHistory,omitempty"`
	To          string `json:"to,omitempty"`      // history: the session it's for
	History     []Chat `json:"history,omitempty"` // history: oldest first
	Edited      bool   `json:"edited,omitempty"`  // history items that were edited

	Away       bool      `json:"away,omitempty"`       // presence: /away
	Hidden     bool      `json:"hidden,omitempty"`     // presence: an invisible user (don't list them)
	Spoiler    bool      `json:"spoiler,omitempty"`    // hidden until asked for (/reveal)
	Voice      bool      `json:"voice,omitempty"`      // a file that is a recorded voice note
	Options    []string  `json:"options,omitempty"`    // poll
	Nuke       bool      `json:"nuke,omitempty"`       // poll: a vote to wipe the room
	Electorate int       `json:"electorate,omitempty"` // nuke poll: people present when proposed
	Choice     *int      `json:"choice,omitempty"`     // vote: option index, -1 takes it back
	Until      string    `json:"until,omitempty"`      // timer: when it ends
	Votes      []Vote    `json:"votes,omitempty"`      // polls passed on in history
	Pinned     bool      `json:"pinned,omitempty"`     // history: the pinned message
	Title      string    `json:"title,omitempty"`      // forum: the forum's or a thread's title
	Thread     string    `json:"thread,omitempty"`     // forum: the thread a post replies to
	Files      []FileRef `json:"files,omitempty"`      // forum: attachments

	// local only: the name doesn't match who the server says sent it, and who
	// that is (edits, deletes and votes are matched on it)
	Unverified bool   `json:"-"`
	By         string `json:"-"`
}

// FileRef is a file attached to a forum post.
type FileRef struct {
	FileID   string  `json:"fileId"`
	Filename string  `json:"filename"`
	Size     float64 `json:"size"`
	Mime     string  `json:"mime"`
}

type Vote struct {
	SID    string `json:"sid"`
	Name   string `json:"name"`
	Choice int    `json:"choice"`
}

type ReplyRef struct {
	MID  string `json:"mid"`
	ID   string `json:"id"`
	Text string `json:"text"`
}

const idAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"

type envelope struct {
	V  int    `json:"v"`
	IV string `json:"iv"`
	CT string `json:"ct"`
	// added by the server when it relays a message: who really sent it (an
	// account, or a guest name it handed out). Clients can't set it.
	By string `json:"by,omitempty"`
	// also added by the server: sent as typing or presence (which skip the
	// room owner's rules); anything else marked like this was snuck in
	Sig bool `json:"sig,omitempty"`
}

type Room struct {
	Server string   // e.g. https://bchr.xyz
	Secret string   // the part of the link after #/
	ID     string   // derived room id used in every API path
	SID    string   // this client's session id in the room
	Words  []string // safety words (/verify); the same for everyone with the link
	aead   cipher.AEAD
	client *http.Client

	// who we are (the login or guest token), so the server can vouch for our
	// name, and for password rooms the token that proves the password
	idHeader, idValue string
	access            string
	Invisible         bool // not listed as here or counted (accounts only, like the website)
}

// roomTransport adds our identity and the room's access token to every request.
type roomTransport struct{ r *Room }

func (t roomTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if t.r.idHeader != "" {
		req.Header.Set(t.r.idHeader, t.r.idValue)
	}
	if t.r.access != "" {
		req.Header.Set("X-Bchr-Access", t.r.access)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// SignedIn makes requests as the account the token belongs to.
func (r *Room) SignedIn(token string) {
	r.idHeader, r.idValue = "Authorization", "Bearer "+token
}

// AsGuest makes requests as the guest the token was issued to.
func (r *Room) AsGuest(token string) {
	r.idHeader, r.idValue = "X-Bchr-Guest", token
}

// ErrPassword: the room has a password and we don't have the right one.
var ErrPassword = errors.New("this room has a password")

// SetPassword switches to a password room's keys (like the website's
// deriveRoom): the id stays, the key, safety words and access token come from
// link + password.
func (r *Room) SetPassword(password string) error {
	master := pbkdf2SHA256([]byte(r.Secret+"\x00"+password), []byte(pbkdf2Salt+"/password"), pbkdf2Iterations, 32)
	block, err := aes.NewCipher(hkdfSHA256(master, nil, []byte("room-key"), 32))
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	var words []string
	for _, b := range hkdfSHA256(master, nil, []byte("verify"), 4) {
		words = append(words, safetyWords[b])
	}
	r.aead, r.Words = aead, words
	r.access = base64.RawURLEncoding.EncodeToString(hkdfSHA256(master, nil, []byte("access"), 32))
	return nil
}

// Card is a room's name and look, set by its owner (encrypted with the room key).
type Card struct {
	Name    string `json:"name"`
	Emoji   string `json:"emoji"`
	Topic   string `json:"topic"`
	Welcome string `json:"welcome"`
}

// OpenCard decrypts a room's card ("" if there's none).
func (r *Room) OpenCard(envelopeJSON string) (Card, bool) {
	var card Card
	var env envelope
	if json.Unmarshal([]byte(envelopeJSON), &env) != nil {
		return card, false
	}
	iv, err1 := base64.StdEncoding.DecodeString(env.IV)
	ct, err2 := base64.StdEncoding.DecodeString(env.CT)
	if err1 != nil || err2 != nil || len(iv) != ivBytes {
		return card, false
	}
	plain, err := r.aead.Open(nil, iv, ct, []byte("msg"))
	if err != nil || json.Unmarshal(plain, &card) != nil {
		return card, false
	}
	return card, true
}

// ParseRoom accepts a full invite link (https://bchr.xyz/#/<secret>) or a bare
// secret. A link overrides the default server so local stacks work too.
func ParseRoom(arg, defaultServer string) (server, secret string, err error) {
	server = strings.TrimRight(defaultServer, "/")
	secret = strings.TrimSpace(arg)
	if i := strings.Index(secret, "#/"); i != -1 {
		if base := strings.TrimRight(secret[:i], "/"); strings.HasPrefix(base, "http://") || strings.HasPrefix(base, "https://") {
			server = base
		}
		secret = secret[i+2:]
	}
	secret = strings.Trim(secret, "/")
	if unescaped, uerr := url.PathUnescape(secret); uerr == nil {
		secret = unescaped
	}
	if secret == "" || strings.Contains(secret, "/") {
		return "", "", fmt.Errorf("%q is not a room link or code", arg)
	}
	return server, secret, nil
}

func NewRoom(server, secret string) (*Room, error) {
	master := pbkdf2SHA256([]byte(secret), []byte(pbkdf2Salt), pbkdf2Iterations, 32)
	id := hkdfSHA256(master, nil, []byte("room-id"), 16)
	key := hkdfSHA256(master, nil, []byte("room-key"), 32)
	var words []string
	for _, b := range hkdfSHA256(master, nil, []byte("verify"), 4) {
		words = append(words, safetyWords[b])
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	room := &Room{
		Server: strings.TrimRight(server, "/"),
		Secret: secret,
		ID:     roomIDPrefix(secret) + hex.EncodeToString(id),
		SID:    randomString(12, idAlphabet),
		Words:  words,
		aead:   aead,
		client: &http.Client{},
	}
	room.client.Transport = roomTransport{room}
	return room, nil
}

// roomIDPrefix: customized rooms (links starting with "c-") have ids starting
// with "r", which the server keeps apart from plain rooms (like the website).
func roomIDPrefix(secret string) string {
	if strings.HasPrefix(secret, "c-") {
		return "r"
	}
	return ""
}

// SharesHistory reports whether the room passes its conversation on to people
// who join later. It's chosen when the room is created: those links start with "h-".
func (r *Room) SharesHistory() bool {
	return strings.HasPrefix(r.Secret, "h-")
}

// Link is the invite link to open the room in a browser.
func (r *Room) Link() string {
	return r.Server + "/#/" + url.PathEscape(r.Secret)
}

// Initialize makes sure the server knows the room (it forgets rooms on restart).
func (r *Room) Initialize(ctx context.Context) error {
	// history rooms ask the server to keep recent messages for newcomers
	path := "/initialize"
	if r.SharesHistory() {
		path += "?history=1"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", r.Server+path, strings.NewReader(r.ID))
	if err != nil {
		return err
	}
	return r.do(req, http.StatusOK)
}

// Subscribe streams decrypted messages to handle until the connection drops or
// ctx is cancelled. Messages that do not decrypt with this room's key are ignored.
func (r *Room) Subscribe(ctx context.Context, onOpen func(), handle func(Chat)) error {
	wsURL := "ws" + strings.TrimPrefix(r.Server, "http") + "/" + r.ID + "/subscribe"
	if t := r.ticket(ctx); t != "" {
		wsURL += "?ticket=" + url.QueryEscape(t)
	}
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	defer c.CloseNow()
	c.SetReadLimit(4 << 20)
	if onOpen != nil {
		onOpen()
	}
	// keep idle connections alive through proxies (nginx drops them after an hour)
	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(pingCtx, 10*time.Second)
				err := c.Ping(pctx)
				cancel()
				if err != nil {
					c.CloseNow()
					return
				}
			}
		}
	}()
	for {
		_, msg, err := c.Read(ctx)
		if websocket.CloseStatus(err) == statusRoomClosed {
			return ErrRoomClosed
		}
		if websocket.CloseStatus(err) == statusBanned {
			return ErrBanned
		}
		if err != nil {
			return err
		}
		if frame, ok := parseServerFrame(msg); ok {
			if frame != nil {
				handle(*frame)
			}
			continue
		}
		if chat, ok := r.decryptChat(msg); ok {
			handle(chat)
		}
	}
}

// ticket asks for a one-time ticket that tells the server who's behind the
// websocket (it counts /nuke votes per person). "" without a name or on error.
func (r *Room) ticket(ctx context.Context) string {
	if r.idHeader == "" {
		return ""
	}
	body := `{"hidden":false}`
	if r.Invisible {
		body = `{"hidden":true}`
	}
	req, err := http.NewRequestWithContext(ctx, "POST", r.Server+"/profile/room/"+r.ID+"/ticket", strings.NewReader(body))
	if err != nil {
		return ""
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Ticket string `json:"ticket"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil {
		return ""
	}
	return out.Ticket
}

// Publish sends a message to the room, filling in the session and message ids.
func (r *Room) Publish(ctx context.Context, chat Chat) error {
	return r.publish(ctx, chat, false)
}

// Signal sends a presence or typing update (not counted as a message).
func (r *Room) Signal(ctx context.Context, chat Chat) error {
	return r.publish(ctx, chat, true)
}

func (r *Room) publish(ctx context.Context, chat Chat, signal bool) error {
	chat.SID = r.SID
	if chat.Timestamp == "" {
		chat.Timestamp = nowISO()
	}
	// everything shown in the conversation needs an id (replies, votes, edits)
	if chat.MID == "" && (chat.Type == "" || chat.Type == "msg" || chat.Type == "emote" || chat.Type == "system" ||
		chat.Type == "poll" || chat.Type == "timer") {
		chat.MID = randomString(16, idAlphabet)
	}
	body, err := r.encryptChat(chat)
	if err != nil {
		return err
	}
	url := r.Server + "/" + r.ID + "/publish"
	if signal {
		url += "?kind=signal"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	return r.do(req, http.StatusAccepted)
}

// parseServerFrame recognises the server's own plaintext frames and turns them
// into local messages: {"announce": ...} (type "announce"), {"notice": ...}
// ("notice") and {"state": ...} ("state"; Until is set while the room is
// frozen). The server only relays encrypted envelopes from clients, so these
// can't be forged. A nil Chat with ok means "nothing to show".
func parseServerFrame(msg []byte) (*Chat, bool) {
	s := string(msg)
	if !strings.HasPrefix(s, `{"announce"`) && !strings.HasPrefix(s, `{"notice"`) && !strings.HasPrefix(s, `{"state"`) &&
		!strings.HasPrefix(s, `{"nuke"`) && !strings.HasPrefix(s, `{"replay"`) && !strings.HasPrefix(s, `{"here"`) &&
		!strings.HasPrefix(s, `{"cleared"`) && !strings.HasPrefix(s, `{"settings"`) && !strings.HasPrefix(s, `{"archive"`) {
		return nil, false
	}
	var frame struct {
		Announce *struct {
			Text, By, Until string
			Countdown       bool
		} `json:"announce"`
		Notice *struct{ Text, By string } `json:"notice"`
		State  *struct {
			FrozenUntil *string `json:"frozenUntil"`
		} `json:"state"`
		Settings *struct {
			Owner, Card string
			Protected   bool
			Slow, Cap   int
			OwnerOnly   bool `json:"ownerOnly"`
		} `json:"settings"` // the room's name, look and rules from its owner
		Cleared *struct {
			At string `json:"at"`
		} `json:"cleared"` // the site's admins deleted the files shared before At
		Archive *struct {
			Max   int    `json:"max"`
			Until string `json:"until"`
		} `json:"archive"` // kept alive: messages (the last Max; 0 = all) and files stay after everyone leaves
		Replay *int      `json:"replay"` // history rooms: this many earlier messages follow
		Here   *[]string `json:"here"`   // who's here (names the server vouches for)
		Nuke   *struct {
			By                          string
			Until                       string
			Electorate, Needed, Yes, No int
			Passed                      bool
		} `json:"nuke"`
	}
	if json.Unmarshal(msg, &frame) != nil {
		return nil, false
	}
	switch {
	case frame.Announce != nil:
		text := frame.Announce.Text
		if t, err := time.Parse(time.RFC3339Nano, frame.Announce.Until); err == nil && frame.Announce.Countdown {
			text += " (at " + t.Local().Format("15:04") + ")"
		}
		return &Chat{Type: "announce", ID: frame.Announce.By, Data: text}, true
	case frame.Notice != nil:
		return &Chat{Type: "notice", ID: frame.Notice.By, Data: frame.Notice.Text}, true
	case frame.Settings != nil:
		st := frame.Settings
		var rules []string
		if st.Protected {
			rules = append(rules, "password")
		}
		if st.OwnerOnly {
			rules = append(rules, "only "+st.Owner+" posts")
		}
		if st.Slow > 0 {
			rules = append(rules, fmt.Sprintf("slow mode %ds", st.Slow))
		}
		if st.Cap > 0 {
			rules = append(rules, fmt.Sprintf("up to %d people", st.Cap))
		}
		return &Chat{Type: "settings", ID: st.Owner, Data: st.Card, Title: strings.Join(rules, " · ")}, true
	case frame.Cleared != nil:
		return &Chat{Type: "cleared", Until: frame.Cleared.At}, true
	case frame.Archive != nil:
		kept := "indefinitely"
		if t, err := time.Parse(time.RFC3339Nano, frame.Archive.Until); err == nil {
			kept = "until " + t.Local().Format("Jan 2")
		}
		if frame.Archive.Max > 0 {
			kept += fmt.Sprintf(", last %d messages", frame.Archive.Max)
		}
		return &Chat{Type: "archive", Data: kept}, true
	case strings.HasPrefix(s, `{"archive":null`):
		return &Chat{Type: "archive"}, true
	case frame.Replay != nil:
		return &Chat{Type: "replay", Data: strconv.Itoa(*frame.Replay)}, true
	case frame.Here != nil:
		return &Chat{Type: "here", Options: *frame.Here}, true
	case frame.Nuke != nil:
		n := frame.Nuke
		c := &Chat{Type: "nukevote", ID: n.By, Nuke: n.Passed}
		if n.Passed {
			c.Data = fmt.Sprintf("%d of %d agreed", n.Yes, n.Electorate)
		} else {
			left := ""
			if t, err := time.Parse(time.RFC3339Nano, n.Until); err == nil {
				left = fmt.Sprintf(", until %s", t.Local().Format("15:04"))
			}
			c.Data = fmt.Sprintf("%d of %d needed%s", n.Yes, n.Needed, left)
		}
		return c, true
	case frame.State != nil:
		c := &Chat{Type: "state"}
		if frame.State.FrozenUntil != nil {
			c.Until = *frame.State.FrozenUntil
		}
		return c, true
	}
	return nil, true // {"announce": null}: nothing to show
}

// IsForum reports whether the link is a forum's (they start with "f-"): its
// posts are stored, encrypted, until the forum expires.
func (r *Room) IsForum() bool {
	return strings.HasPrefix(r.Secret, "f-")
}

type ForumInfo struct {
	Expires time.Time `json:"expires"`
	Posts   int       `json:"posts"`
}

func (r *Room) ForumInfo(ctx context.Context) (ForumInfo, error) {
	var info ForumInfo
	req, err := http.NewRequestWithContext(ctx, "GET", r.Server+"/profile/forum/"+r.ID, nil)
	if err != nil {
		return info, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return info, errors.New("this forum doesn't exist any more (forums are deleted when they expire)")
	}
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("forum: %s", resp.Status)
	}
	return info, json.NewDecoder(resp.Body).Decode(&info)
}

// ForumLog returns every event stored in the forum, decrypted, oldest first.
func (r *Room) ForumLog(ctx context.Context) ([]Chat, error) {
	var out []Chat
	after := int64(0)
	for {
		url := fmt.Sprintf("%s/profile/forum/%s/log?after=%d&limit=500", r.Server, r.ID, after)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, err
		}
		var page []struct {
			Seq  int64  `json:"seq"`
			Body string `json:"body"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, p := range page {
			if c, ok := r.decryptChat([]byte(p.Body)); ok {
				out = append(out, c)
			}
			after = p.Seq
		}
		if len(page) < 500 {
			return out, nil
		}
	}
}

// ForumPost stores an event in the forum; the server also relays it to the room.
func (r *Room) ForumPost(ctx context.Context, chat Chat) error {
	chat.SID = r.SID
	chat.Timestamp = nowISO()
	if chat.MID == "" {
		chat.MID = randomString(16, idAlphabet)
	}
	body, err := r.encryptChat(chat)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", r.Server+"/profile/forum/"+r.ID+"/log", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	return r.do(req, http.StatusCreated)
}

// Nuke votes to wipe the room (or to keep it). The server counts the votes
// and deletes the files; everyone hears about it in a {"nuke": ...} frame.
func (r *Room) Nuke(ctx context.Context, wipe bool) error {
	body := `{"wipe":false}`
	if wipe {
		body = `{"wipe":true}`
	}
	req, err := http.NewRequestWithContext(ctx, "POST", r.Server+"/profile/room/"+r.ID+"/nuke", strings.NewReader(body))
	if err != nil {
		return err
	}
	return r.do(req, http.StatusOK)
}

// Report tells the site's admins about the room: "report" (room id only),
// "flag" (hands them the link so they can join) or "feature" (an idea).
func (r *Room) Report(ctx context.Context, kind, reason, reporter string) error {
	body := map[string]string{"kind": kind, "reason": reason, "reporter": reporter}
	if kind == "flag" {
		body["secret"] = r.Secret
	} else {
		body["room"] = r.ID
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", r.Server+"/profile/report", strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	return r.do(req, http.StatusCreated)
}

// Upload encrypts the file while streaming it to the server, then announces it
// to the room with an encrypted message carrying the real filename.
func (r *Room) Upload(ctx context.Context, name, path string, progress func(float64)) (Chat, error) {
	f, err := os.Open(path)
	if err != nil {
		return Chat{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Chat{}, err
	}
	if info.IsDir() {
		return Chat{}, fmt.Errorf("%s is a directory", path)
	}
	size := info.Size()
	if size > maxFileBytes {
		return Chat{}, fmt.Errorf("%s is larger than 510 MB", filepath.Base(path))
	}
	chunks := (size + fileChunkBytes - 1) / fileChunkBytes
	if chunks == 0 {
		chunks = 1
	}
	fileID := randomString(32, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(r.encryptFile(pw, f, int(chunks)))
	}()
	total := size + chunks*int64(ivBytes+tagBytes)
	req, err := http.NewRequestWithContext(ctx, "POST", r.Server+"/"+r.ID+"/upload_file/"+fileID,
		&progressReader{r: pr, total: total, fn: progress})
	if err != nil {
		pr.Close()
		return Chat{}, err
	}
	req.ContentLength = total
	req.Header.Set("Content-Type", "application/octet-stream")
	if err := r.do(req, http.StatusCreated); err != nil {
		pr.Close()
		return Chat{}, err
	}
	fsize := float64(size)
	chat := Chat{
		ID:        name,
		Timestamp: nowISO(),
		IsFile:    true,
		Filename:  filepath.Base(path),
		FileID:    fileID,
		Size:      &fsize,
		Mime:      mime.TypeByExtension(filepath.Ext(path)),
	}
	return chat, r.Publish(ctx, chat)
}

// Download fetches and decrypts a shared file into dir without overwriting
// existing files, returning the path it was saved to.
func (r *Room) Download(ctx context.Context, chat Chat, dir string, progress func(float64)) (string, error) {
	if !fileIDRegexp.MatchString(chat.FileID) {
		return "", errors.New("invalid file id")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", r.Server+"/"+r.ID+"/download_file/"+chat.FileID, nil)
	if err != nil {
		return "", err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".bchr-download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	tmp.Chmod(0644)
	body := &progressReader{r: resp.Body, total: resp.ContentLength, fn: progress}
	err = r.decryptFile(tmp, body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	dest, err := reserveName(dir, safeFilename(chat.Filename))
	if err != nil {
		return "", err
	}
	return dest, os.Rename(tmp.Name(), dest)
}

func (r *Room) do(req *http.Request, want int) error {
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusGone {
		return ErrRoomClosed
	}
	if resp.StatusCode == http.StatusForbidden && strings.HasSuffix(req.URL.Path, "/initialize") {
		return ErrBanned
	}
	if resp.StatusCode == http.StatusUnauthorized && strings.HasSuffix(req.URL.Path, "/initialize") {
		return ErrPassword
	}
	if resp.StatusCode != want {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("%s %s: %s %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(msg)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// ------------------------------------------------------------------ crypto

func (r *Room) encryptChat(chat Chat) ([]byte, error) {
	plain, err := json.Marshal(chat)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, ivBytes)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	ct := r.aead.Seal(nil, iv, plain, []byte("msg"))
	return json.Marshal(envelope{
		V:  protocolVersion,
		IV: base64.StdEncoding.EncodeToString(iv),
		CT: base64.StdEncoding.EncodeToString(ct),
	})
}

func (r *Room) decryptChat(raw []byte) (Chat, bool) {
	var env envelope
	if json.Unmarshal(raw, &env) != nil || env.V != protocolVersion {
		return Chat{}, false
	}
	iv, err1 := base64.StdEncoding.DecodeString(env.IV)
	ct, err2 := base64.StdEncoding.DecodeString(env.CT)
	if err1 != nil || err2 != nil || len(iv) != ivBytes {
		return Chat{}, false
	}
	plain, err := r.aead.Open(nil, iv, ct, []byte("msg"))
	if err != nil {
		return Chat{}, false
	}
	var chat Chat
	if json.Unmarshal(plain, &chat) != nil {
		return Chat{}, false
	}
	if chat.IsFile && !fileIDRegexp.MatchString(chat.FileID) {
		return Chat{}, false
	}
	if chat.ID == "" {
		chat.ID = "unknown"
	}
	if env.Sig && chat.Type != "presence" && chat.Type != "typing" {
		return Chat{}, false
	}
	chat.Unverified = env.By == "" || env.By != chat.ID
	chat.By = env.By
	return chat, true
}

func (r *Room) encryptFile(w io.Writer, src io.Reader, chunks int) error {
	buf := make([]byte, fileChunkBytes)
	iv := make([]byte, ivBytes)
	for i := 0; i < chunks; i++ {
		n, err := io.ReadFull(src, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return err
		}
		if _, err := rand.Read(iv); err != nil {
			return err
		}
		if _, err := w.Write(iv); err != nil {
			return err
		}
		if _, err := w.Write(r.aead.Seal(nil, iv, buf[:n], chunkAAD(uint32(i), i == chunks-1))); err != nil {
			return err
		}
	}
	return nil
}

func (r *Room) decryptFile(w io.Writer, src io.Reader) error {
	br := bufio.NewReader(src)
	buf := make([]byte, fileRecordBytes)
	for i := uint32(0); ; i++ {
		n, err := io.ReadFull(br, buf)
		last := err == io.ErrUnexpectedEOF
		if err != nil && !last {
			if err == io.EOF {
				return errors.New("file is empty or truncated")
			}
			return err
		}
		if !last {
			if _, perr := br.Peek(1); perr == io.EOF {
				last = true
			} else if perr != nil {
				return perr
			}
		}
		if n < ivBytes+tagBytes {
			return errors.New("file is corrupt")
		}
		plain, err := r.aead.Open(buf[ivBytes:ivBytes], buf[:ivBytes], buf[ivBytes:n], chunkAAD(i, last))
		if err != nil {
			return errors.New("file failed to decrypt (wrong room or tampered)")
		}
		if _, err := w.Write(plain); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

func chunkAAD(index uint32, last bool) []byte {
	aad := make([]byte, 9)
	copy(aad, "file")
	binary.BigEndian.PutUint32(aad[4:], index)
	if last {
		aad[8] = 1
	}
	return aad
}

// PBKDF2 (RFC 8018) with HMAC-SHA256, matching WebCrypto's deriveBits.
func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	var out []byte
	for block := uint32(1); len(out) < keyLen; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.Write(prf, binary.BigEndian, block)
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for n := 1; n < iterations; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for x := 0; x < hashLen; x++ {
				t[x] ^= u[x]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// HKDF (RFC 5869) with SHA-256. An empty salt means HashLen zero bytes, which
// is what WebCrypto does for a zero-length salt.
func hkdfSHA256(ikm, salt, info []byte, length int) []byte {
	if len(salt) == 0 {
		salt = make([]byte, sha256.Size)
	}
	extract := hmac.New(sha256.New, salt)
	extract.Write(ikm)
	prk := extract.Sum(nil)
	var out, prev []byte
	for i := byte(1); len(out) < length; i++ {
		m := hmac.New(sha256.New, prk)
		m.Write(prev)
		m.Write(info)
		m.Write([]byte{i})
		prev = m.Sum(nil)
		out = append(out, prev...)
	}
	return out[:length]
}

// ----------------------------------------------------------------- helpers

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func randomString(n int, alphabet string) string {
	limit := 256 - 256%len(alphabet)
	out := make([]byte, 0, n)
	b := make([]byte, 1)
	for len(out) < n {
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		if int(b[0]) < limit {
			out = append(out, alphabet[int(b[0])%len(alphabet)])
		}
	}
	return string(out)
}

// safeFilename turns an untrusted name from the room into a plain file name.
func safeFilename(name string) string {
	// keep only the last path element, whichever separator the sender used
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".bchr-download-") {
		return "file"
	}
	return name
}

// reserveName creates dir/name, or dir/"name (n).ext" if that exists.
func reserveName(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		path := filepath.Join(dir, candidate)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			f.Close()
			return path, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not find a free name for %s", name)
}

type progressReader struct {
	r     io.Reader
	total int64
	done  int64
	fn    func(float64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.fn != nil && p.total > 0 {
		p.fn(float64(p.done) / float64(p.total))
	}
	return n, err
}

// 256 words for /verify, one per byte. Must match SAFETY_WORDS in the website
// (src/app/room-crypto.ts).
var safetyWords = [256]string{
	"acid", "acorn", "actor", "adobe", "agent", "alarm", "album", "alien", "alley", "amber", "angel",
	"ankle", "apple", "apron", "arena", "arrow", "aspen", "atlas", "attic", "award", "bacon", "badge",
	"bagel", "baker", "bamboo", "banjo", "barn", "basil", "beach", "beard", "beaver", "bench",
	"berry", "bison", "blade", "blaze", "bloom", "blues", "board", "bonus", "boots", "brain", "brass",
	"bread", "brick", "bride", "broom", "brush", "bucket", "buddy", "bugle", "cabin", "cable",
	"cactus", "camel", "candy", "canoe", "canyon", "cargo", "carrot", "castle", "cedar", "chalk",
	"charm", "cheese", "cherry", "chess", "chief", "chili", "cider", "cinema", "circus", "citrus",
	"clam", "cliff", "cloud", "clover", "coast", "cobra", "cocoa", "comet", "coral", "cotton",
	"couch", "cow", "crab", "crane", "crayon", "creek", "crown", "cube", "cupid", "curry", "daisy",
	"dance", "delta", "denim", "desk", "diary", "dingo", "disco", "dock", "dolphin", "donut",
	"dragon", "dream", "drum", "duck", "dune", "eagle", "easel", "echo", "eclipse", "elbow", "elder",
	"elephant", "elk", "ember", "emerald", "engine", "falcon", "fancy", "feather", "fern", "ferry",
	"fiddle", "fig", "finch", "flame", "flask", "flute", "foam", "forest", "fossil", "fox", "frost",
	"fudge", "galaxy", "garden", "garlic", "gecko", "genie", "ginger", "glacier", "globe", "goose",
	"grape", "gravy", "guitar", "gumbo", "hammer", "harbor", "harp", "hazel", "hedge", "helmet",
	"heron", "hippo", "honey", "hornet", "husky", "igloo", "iguana", "island", "ivory", "jacket",
	"jaguar", "jazz", "jelly", "jewel", "jungle", "kayak", "kettle", "kiwi", "koala", "ladder",
	"lagoon", "lemon", "lens", "lilac", "lime", "linen", "lion", "llama", "lobster", "locket",
	"lotus", "lunar", "mango", "maple", "marble", "meadow", "melon", "mint", "mirror", "mocha",
	"monkey", "moose", "mosaic", "muffin", "nectar", "needle", "noodle", "nutmeg", "oasis", "ocean",
	"olive", "onion", "opal", "orbit", "orchid", "otter", "owl", "oyster", "paddle", "panda",
	"papaya", "parrot", "pastel", "peach", "pebble", "pepper", "piano", "pickle", "pilot", "pine",
	"pixel", "planet", "plum", "polar", "pony", "poppy", "prism", "pumpkin", "puzzle", "quartz",
	"quill", "rabbit", "radar", "radish", "raven", "reef", "robin", "rocket", "rose", "ruby",
	"saddle", "salmon", "sapphire", "satin", "scarf", "shadow", "shell", "sierra", "silver", "sketch",
}
