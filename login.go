package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// signIn logs in to the site so the reader uses your username as your name.
// The password comes from BCHR_PASSWORD, or is asked for without echoing it.
func signIn(server, username string) (name, token string, err error) {
	password := os.Getenv("BCHR_PASSWORD")
	if password == "" {
		if password, err = askPassword(); err != nil {
			return "", "", err
		}
	}
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(strings.TrimRight(server, "/")+"/login", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "", "", fmt.Errorf("sign in failed: %s", strings.TrimSpace(string(msg)))
	}
	var out struct {
		Username string `json:"username"`
		Token    string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Username == "" || out.Token == "" {
		return "", "", fmt.Errorf("sign in failed: unexpected answer from the server")
	}
	return out.Username, out.Token, nil
}

// guestName asks the server for a guest name (anon-1234) and the token that
// proves it's ours, so nobody else can post under it.
func guestName(server string) (name, token string, err error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(strings.TrimRight(server, "/")+"/profile/guest", "application/json", nil)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var out struct {
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil || out.Name == "" || out.Token == "" {
		return "", "", fmt.Errorf("no guest name (%s)", resp.Status)
	}
	return out.Name, out.Token, nil
}

func askPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !isTerminal(fd) {
		return "", fmt.Errorf("-login needs a terminal to ask for the password, or set BCHR_PASSWORD")
	}
	fmt.Fprint(os.Stderr, "Password: ")
	restore, ok := makeRaw(fd)
	if !ok {
		return "", fmt.Errorf("can't read a password here; set BCHR_PASSWORD")
	}
	defer func() {
		restore()
		fmt.Fprintln(os.Stderr)
	}()
	in := bufio.NewReader(os.Stdin)
	var pw []rune
	for {
		r, _, err := in.ReadRune()
		if err != nil {
			return "", err
		}
		switch r {
		case '\r', '\n':
			return string(pw), nil
		case 3: // Ctrl-C
			restore()
			fmt.Fprintln(os.Stderr)
			os.Exit(130)
		case 127, 8: // backspace
			if len(pw) > 0 {
				pw = pw[:len(pw)-1]
			}
		default:
			if r >= 32 {
				pw = append(pw, r)
			}
		}
	}
}
