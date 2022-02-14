package main

import (
	"github.com/gorilla/websocket"
	"log"
	"net/http"
)

func main()  {
	println("agile config starting ...")
	headers := http.Header {
	}
	headers.Add("appid", "test_app")
	headers.Add("evn", "DEV")
	headers.Add("Authorization", "Basic dGVzdF9hcHA6dGVzdF9hcHA=")
	c, _, err := websocket.DefaultDialer.Dial("ws://agileconfig_server.xbaby.xyz/ws", headers)
	if err != nil {
		log.Fatal("dial:", err)
	}
	defer c.Close()
}
