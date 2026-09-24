package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// Value produced by the website's WebCrypto code (src/app/room-crypto.ts) for
// the secret "abc123"; if this changes, the reader can no longer join rooms
// created in the browser.
const browserRoomID = "8edc0a9188976f958d00bf66717ffa79"

func TestRoomIDMatchesBrowser(t *testing.T) {
	r, err := NewRoom("https://bchr.xyz", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != browserRoomID {
		t.Fatalf("room id %s, browser derives %s", r.ID, browserRoomID)
	}
}

func TestMessageRoundTrip(t *testing.T) {
	a, _ := NewRoom("", "room")
	b, _ := NewRoom("", "other room")
	raw, err := a.encryptChat(Chat{ID: "ben", Data: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("hello")) {
		t.Fatal("plaintext leaked")
	}
	if c, ok := a.decryptChat(raw); !ok || c.Data != "hello" || c.ID != "ben" {
		t.Fatalf("round trip failed: %v %+v", ok, c)
	}
	if _, ok := b.decryptChat(raw); ok {
		t.Fatal("decrypted with the wrong room key")
	}
	if _, ok := a.decryptChat([]byte(`{"v":1,"iv":"AAAA","ct":"AAAA"}`)); ok {
		t.Fatal("accepted garbage")
	}
}

func TestFileRoundTrip(t *testing.T) {
	r, _ := NewRoom("", "room")
	for _, n := range []int{0, 1, fileChunkBytes, 2*fileChunkBytes + 3} {
		plain := bytes.Repeat([]byte{7}, n)
		chunks := max(1, (n+fileChunkBytes-1)/fileChunkBytes)
		var enc bytes.Buffer
		if err := r.encryptFile(&enc, bytes.NewReader(plain), chunks); err != nil {
			t.Fatal(err)
		}
		var dec bytes.Buffer
		if err := r.decryptFile(&dec, bytes.NewReader(enc.Bytes())); err != nil {
			t.Fatalf("size %d: %v", n, err)
		}
		if !bytes.Equal(dec.Bytes(), plain) {
			t.Fatalf("size %d: mismatch", n)
		}
		if n > fileChunkBytes {
			// dropping the last record must be detected
			truncated := enc.Bytes()[:2*fileRecordBytes]
			if err := r.decryptFile(&bytes.Buffer{}, bytes.NewReader(truncated)); err == nil {
				t.Fatal("truncation not detected")
			}
		}
	}
}

func TestParseRoom(t *testing.T) {
	cases := []struct{ in, server, secret string }{
		{"abc123", "https://bchr.xyz", "abc123"},
		{"https://bchr.xyz/#/abc123", "https://bchr.xyz", "abc123"},
		{"http://localhost:8080/#/my%20room/", "http://localhost:8080", "my room"},
		{"bchr.xyz/#/abc", "https://bchr.xyz", "abc"},
	}
	for _, c := range cases {
		server, secret, err := ParseRoom(c.in, "https://bchr.xyz/")
		if err != nil || server != c.server || secret != c.secret {
			t.Errorf("ParseRoom(%q) = %q, %q, %v", c.in, server, secret, err)
		}
	}
	for _, bad := range []string{"", "#/", "a/b"} {
		if _, _, err := ParseRoom(bad, "https://bchr.xyz"); err == nil {
			t.Errorf("ParseRoom(%q) should fail", bad)
		}
	}
}

func TestSplitArgs(t *testing.T) {
	got := splitArgs(`/send 'My File.pdf' "a b.txt" c\ d.png  e`)
	want := []string{"/send", "My File.pdf", "a b.txt", "c d.png", "e"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestSafeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":         "report.pdf",
		"../../etc/passwd":   "passwd",
		"..":                 "file",
		"":                   "file",
		"a\x1b[2Jb.txt":      "a[2Jb.txt",
		".bchr-download-123": "file",
	} {
		if got := safeFilename(in); got != want {
			t.Errorf("safeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClean(t *testing.T) {
	if got := clean("a\x1b[31mb\x07‮c\nd", true); got != "a[31mbc\nd" {
		t.Fatalf("got %q", got)
	}
}

func TestParseServerFrame(t *testing.T) {
	c, ok := parseServerFrame([]byte(`{"announce":{"text":"hi","by":"ben","at":"x","until":"y"}}`))
	if !ok || c == nil || c.Type != "announce" || c.Data != "hi" || c.ID != "ben" {
		t.Fatalf("announce: %v %+v", ok, c)
	}
	if c, ok := parseServerFrame([]byte(`{"announce":null}`)); !ok || c != nil {
		t.Fatalf("clear: %v %+v", ok, c)
	}
	if c, ok := parseServerFrame([]byte(`{"notice":{"text":"be nice","by":"boss"}}`)); !ok || c.Type != "notice" || c.Data != "be nice" {
		t.Fatalf("notice: %v %+v", ok, c)
	}
	if c, ok := parseServerFrame([]byte(`{"state":{"frozenUntil":"2030-01-01T00:00:00Z","cap":0}}`)); !ok || c.Type != "state" || c.Until == "" {
		t.Fatalf("state: %v %+v", ok, c)
	}
	if _, ok := parseServerFrame([]byte(`{"v":1,"iv":"a","ct":"b"}`)); ok {
		t.Fatal("envelope taken for a server frame")
	}
}

// the website derives the same words (src/app/room-crypto.ts, WebCrypto)
func TestSafetyWordsMatchBrowser(t *testing.T) {
	r, _ := NewRoom("", "abc123")
	if got := strings.Join(r.Words, " "); got != "goose cocoa pastel actor" {
		t.Fatalf("got %q", got)
	}
}

func TestNewFieldsRoundTrip(t *testing.T) {
	r, _ := NewRoom("", "room")
	raw, _ := r.encryptChat(Chat{ID: "ben", Data: "yes", MID: "m1", SID: r.SID, ReplyTo: &ReplyRef{MID: "m0", ID: "amy", Text: "ok?"}})
	c, ok := r.decryptChat(raw)
	if !ok || c.ReplyTo == nil || c.ReplyTo.Text != "ok?" || c.MID != "m1" || c.SID == "" {
		t.Fatalf("%v %+v", ok, c)
	}
}

// Tags from the website (room-crypto.ts mentionTag) for the same rooms.
func TestMentionTagsMatchBrowser(t *testing.T) {
	r, err := NewRoom("https://bchr.xyz", "abcdefghijklmnopqrstuvwxyz")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "8f407cc992ec5f29e760538e25f8d29d" || r.mentionTag("Ben") != "f5641010d946b1ecb792b9ae9b473739" {
		t.Fatalf("got %s %s", r.ID, r.mentionTag("Ben"))
	}
	p, _ := NewRoom("https://bchr.xyz", "c-abcdefghijklmnopqrstuvwx")
	if err := p.SetPassword("hunter2"); err != nil {
		t.Fatal(err)
	}
	if p.ID != "rff6143a410be46362c190bfd6ffc55fd" || p.mentionTag("amy") != "df82315e7bdc5b98fd7860aa153ab22e" {
		t.Fatalf("password room: %s %s", p.ID, p.mentionTag("amy"))
	}
	names := mentionedNames(Chat{Data: "hey @Ben. and @amy-2, see @ben", ReplyTo: &ReplyRef{ID: "Cat"}})
	if strings.Join(names, ",") != "ben.,ben,amy-2,cat" {
		t.Fatalf("names: %v", names)
	}
	if q := r.tagQuery(Chat{Data: "no one"}); q != "" {
		t.Fatalf("query: %q", q)
	}
	if q := r.tagQuery(Chat{Data: "@Ben hi"}); q != "?tags=f5641010d946b1ecb792b9ae9b473739" {
		t.Fatalf("query: %q", q)
	}
}
