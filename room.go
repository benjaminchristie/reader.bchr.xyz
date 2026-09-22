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

// Chat is the plaintext of every message. Field names match the website.
type Chat struct {
	ID        string   `json:"id"`
	Timestamp string   `json:"timestamp,omitempty"`
	Data      string   `json:"data,omitempty"`
	IsFile    bool     `json:"isFile,omitempty"`
	Filename  string   `json:"filename,omitempty"`
	FileID    string   `json:"fileId,omitempty"`
	Size      *float64 `json:"size,omitempty"`
	Mime      string   `json:"mime,omitempty"`
}

type envelope struct {
	V  int    `json:"v"`
	IV string `json:"iv"`
	CT string `json:"ct"`
}

type Room struct {
	Server string // e.g. https://bchr.xyz
	Secret string // the part of the link after #/
	ID     string // derived room id used in every API path
	aead   cipher.AEAD
	client *http.Client
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
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Room{
		Server: strings.TrimRight(server, "/"),
		Secret: secret,
		ID:     hex.EncodeToString(id),
		aead:   aead,
		client: &http.Client{},
	}, nil
}

// Link is the invite link to open the room in a browser.
func (r *Room) Link() string {
	return r.Server + "/#/" + url.PathEscape(r.Secret)
}

// Initialize makes sure the server knows the room (it forgets rooms on restart).
func (r *Room) Initialize(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "POST", r.Server+"/initialize", strings.NewReader(r.ID))
	if err != nil {
		return err
	}
	return r.do(req, http.StatusOK)
}

// Subscribe streams decrypted messages to handle until the connection drops or
// ctx is cancelled. Messages that do not decrypt with this room's key are ignored.
func (r *Room) Subscribe(ctx context.Context, onOpen func(), handle func(Chat)) error {
	wsURL := "ws" + strings.TrimPrefix(r.Server, "http") + "/" + r.ID + "/subscribe"
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
		if err != nil {
			return err
		}
		if chat, ok := r.decryptChat(msg); ok {
			handle(chat)
		}
	}
}

func (r *Room) Publish(ctx context.Context, chat Chat) error {
	body, err := r.encryptChat(chat)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", r.Server+"/"+r.ID+"/publish", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	return r.do(req, http.StatusAccepted)
}

func (r *Room) SendText(ctx context.Context, name, text string) error {
	return r.Publish(ctx, Chat{ID: name, Timestamp: nowISO(), Data: text})
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
