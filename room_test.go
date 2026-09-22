package main

import (
	"bytes"
	"reflect"
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
