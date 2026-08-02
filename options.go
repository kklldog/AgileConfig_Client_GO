package agileconfig

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type Logger interface {
	Printf(format string, v ...interface{})
}

type CacheOptions struct {
	Disabled  bool
	Directory string
	Encrypt   bool
}

type Options struct {
	AppID                      string
	Secret                     string
	Nodes                      string
	Name                       string
	Tag                        string
	Env                        string
	HTTPTimeout                time.Duration
	ReconnectInterval          time.Duration
	WebSocketHeartbeatInterval time.Duration
	Cache                      CacheOptions
	ServiceRegister            *ServiceRegisterInfo
	HTTPClient                 *http.Client
	WebSocketDialer            *websocket.Dialer
	Logger                     Logger
}

type fileOptions struct {
	AppID             string `json:"appId"`
	Secret            string `json:"secret"`
	Nodes             string `json:"nodes"`
	Name              string `json:"name"`
	Tag               string `json:"tag"`
	Env               string `json:"env"`
	HTTPTimeout       int    `json:"httpTimeout"`
	ReconnectInterval int    `json:"reconnectInterval"`
	Cache             struct {
		Enabled       *bool  `json:"enabled"`
		Directory     string `json:"directory"`
		ConfigEncrypt bool   `json:"config_encrypt"`
	} `json:"cache"`
	ServiceRegister *struct {
		ServiceID   string   `json:"serviceId"`
		ServiceName string   `json:"serviceName"`
		IP          string   `json:"ip"`
		Port        *int     `json:"port"`
		Metadata    []string `json:"metaData"`
		AlarmURL    string   `json:"alarmUrl"`
		Heartbeat   struct {
			Mode     string `json:"mode"`
			Interval int    `json:"interval"`
			URL      string `json:"url"`
		} `json:"heartbeat"`
		ReregisterInterval int `json:"reregisterInterval"`
	} `json:"serviceRegister"`
}

func LoadOptions(path string) (Options, error) {
	content, err := ioutil.ReadFile(path)
	if err != nil {
		return Options{}, fmt.Errorf("read options: %w", err)
	}

	var envelope struct {
		AgileConfig json.RawMessage `json:"AgileConfig"`
	}
	if err := json.Unmarshal(content, &envelope); err != nil {
		return Options{}, fmt.Errorf("decode options: %w", err)
	}

	configJSON := content
	if len(envelope.AgileConfig) > 0 && string(envelope.AgileConfig) != "null" {
		configJSON = envelope.AgileConfig
	}

	var raw fileOptions
	if err := json.Unmarshal(configJSON, &raw); err != nil {
		return Options{}, fmt.Errorf("decode AgileConfig options: %w", err)
	}

	options := Options{
		AppID:             raw.AppID,
		Secret:            raw.Secret,
		Nodes:             raw.Nodes,
		Name:              raw.Name,
		Tag:               raw.Tag,
		Env:               raw.Env,
		HTTPTimeout:       time.Duration(raw.HTTPTimeout) * time.Second,
		ReconnectInterval: time.Duration(raw.ReconnectInterval) * time.Second,
		Cache: CacheOptions{
			Directory: raw.Cache.Directory,
			Encrypt:   raw.Cache.ConfigEncrypt,
		},
	}
	if raw.Cache.Enabled != nil {
		options.Cache.Disabled = !*raw.Cache.Enabled
	}
	if raw.ServiceRegister != nil {
		options.ServiceRegister = &ServiceRegisterInfo{
			ServiceID:          raw.ServiceRegister.ServiceID,
			ServiceName:        raw.ServiceRegister.ServiceName,
			IP:                 raw.ServiceRegister.IP,
			Port:               raw.ServiceRegister.Port,
			Metadata:           append([]string(nil), raw.ServiceRegister.Metadata...),
			AlarmURL:           raw.ServiceRegister.AlarmURL,
			CheckURL:           raw.ServiceRegister.Heartbeat.URL,
			HeartbeatMode:      raw.ServiceRegister.Heartbeat.Mode,
			HeartbeatInterval:  raw.ServiceRegister.Heartbeat.Interval,
			ReregisterInterval: raw.ServiceRegister.ReregisterInterval,
		}
	}
	return options, nil
}

func normalizeOptions(options Options) (Options, []string, error) {
	options.AppID = strings.TrimSpace(options.AppID)
	if options.AppID == "" {
		return Options{}, nil, errors.New("app ID is required")
	}
	if strings.TrimSpace(options.Nodes) == "" {
		return Options{}, nil, errors.New("at least one server node is required")
	}

	var nodes []string
	for _, value := range strings.Split(options.Nodes, ",") {
		node := strings.TrimRight(strings.TrimSpace(value), "/")
		if node == "" {
			continue
		}
		parsed, err := url.Parse(node)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return Options{}, nil, fmt.Errorf("invalid server node %q", value)
		}
		nodes = append(nodes, node)
	}
	if len(nodes) == 0 {
		return Options{}, nil, errors.New("at least one valid server node is required")
	}

	options.Env = strings.ToUpper(strings.TrimSpace(options.Env))
	if options.HTTPTimeout <= 0 {
		options.HTTPTimeout = 100 * time.Second
	}
	if options.ReconnectInterval <= 0 {
		options.ReconnectInterval = 5 * time.Second
	}
	if options.WebSocketHeartbeatInterval <= 0 {
		options.WebSocketHeartbeatInterval = 30 * time.Second
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{}
	}
	if options.WebSocketDialer == nil {
		options.WebSocketDialer = websocket.DefaultDialer
	}
	if options.Logger == nil {
		options.Logger = log.Default()
	}

	if options.ServiceRegister != nil {
		register := *options.ServiceRegister
		register.Metadata = append([]string(nil), register.Metadata...)
		if strings.TrimSpace(register.ServiceName) == "" {
			return Options{}, nil, errors.New("service register name is required")
		}
		if register.HeartbeatMode == "" {
			register.HeartbeatMode = "client"
		}
		if register.HeartbeatInterval <= 0 {
			register.HeartbeatInterval = 30
		}
		if register.ReregisterInterval <= 0 {
			register.ReregisterInterval = 5
		}
		if register.ServiceID == "" {
			id, err := loadOrCreateServiceID(options.Cache.Directory, options.AppID)
			if err != nil {
				return Options{}, nil, err
			}
			register.ServiceID = id
		}
		options.ServiceRegister = &register
	}
	return options, nodes, nil
}

func loadOrCreateServiceID(directory, appID string) (string, error) {
	path := filepath.Join(directory, appID+".agileconfig.client.serviceid")
	if content, err := ioutil.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(content)); id != "" {
			return id, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read service ID cache: %w", err)
	}

	random := make([]byte, 16)
	if _, err := cryptoRead(random); err != nil {
		return "", fmt.Errorf("generate service ID: %w", err)
	}
	id := hex.EncodeToString(random)
	if directory != "" {
		if err := os.MkdirAll(directory, 0755); err != nil {
			return "", fmt.Errorf("create cache directory: %w", err)
		}
	}
	if err := ioutil.WriteFile(path, []byte(id), 0600); err != nil {
		return "", fmt.Errorf("write service ID cache: %w", err)
	}
	return id, nil
}
