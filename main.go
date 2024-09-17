package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"nhooyr.io/websocket"
)

type Resp struct {
	Id        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Data      []byte `json:"data"`
	Filename  string `json:"filename"`
	IsFile    bool   `json:"isFile"`
}

func savefile(dir string, r *Resp) {
	err := os.WriteFile(fmt.Sprintf("%s/%s", dir, r.Filename), r.Data, 0644)
	if err != nil {
		log.Printf("Error saving %s: %s", r.Filename, err.Error())
	}
}

func loop(page, dir string) {
	body := []byte(page)
	r := bytes.NewReader(body)
	_, err := http.Post("https://bchr.xyz/initialize", "", r)
	if err != nil {
		log.Fatalf("Error in initialization: %s", err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	url := fmt.Sprintf("wss://bchr.xyz/%s/subscribe", page)
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		log.Fatalf("Error in dial: %s", err.Error())
	}
	defer ws.CloseNow()
	ws.SetReadLimit(-1)
	for {
		resp := &Resp{}
		_, b, err := ws.Read(ctx)
		if err != nil {
			return
		}
		json.Unmarshal(b, resp)
		if resp.IsFile {
			log.Printf("Saving file: %s", resp.Filename)
			savefile(dir, resp)
		}
	}
}

func main() {
	dirPtr := flag.String("dir", "files", "dir to save files to")
	pagePtr := flag.String("page", "test", "page to subscribe to")
	flag.Parse()
	dir := *dirPtr
	page := *pagePtr
	os.MkdirAll(dir, 0755)
	for {
		loop(page, dir)
	}
}
