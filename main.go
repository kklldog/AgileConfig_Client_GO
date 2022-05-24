package main

import (
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"io/ioutil"
	"log"
	"net/http"
)

type config struct {
	Group string `json:"group"`
	Key   string `json:"key"`
	Value string `json:"value"`
	Id    string `json:"id"`
	AppId string `json:"appId"`
}

func main() {
	println("agile config starting ...")
	headers := http.Header{}
	headers.Add("appid", "test_app")
	headers.Add("evn", "DEV")
	headers.Add("Authorization", "Basic dGVzdF9hcHA6dGVzdF9hcHA=")
	c, _, err := websocket.DefaultDialer.Dial("ws://agileconfig_server.xbaby.xyz/ws?tag=goclient", headers)
	if err != nil {
		log.Fatal("dial:", err)
	}

	getconfigApiUrl := "http://agileconfig_server.xbaby.xyz/api/config/app/test_app?env=DEV"
	//get configs
	req, err := http.NewRequest("GET", getconfigApiUrl, nil)
	req.Header.Set("Authorization", "Basic dGVzdF9hcHA6dGVzdF9hcHA=")

	resp, err := http.DefaultClient.Do(req)
	content, err := ioutil.ReadAll(resp.Body)
	stringContent := string(content)
	fmt.Printf("response: %s\n", stringContent)

	configArray := []config{}
	err = json.Unmarshal(content, &configArray)
	if err != nil {
		log.Fatal("Unmarshal err:", err)
	}
	for {
		// 读取客户端的消息
		_, msg, err := c.ReadMessage()
		if err != nil {
			return
		}

		// 把消息打印到标准输出
		fmt.Printf("%s sent: %s\n", c.RemoteAddr(), string(msg))

	}
	defer c.Close()
}
