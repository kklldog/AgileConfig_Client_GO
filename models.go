package agileconfig

import (
	"fmt"
	"net"
	"strconv"
)

const Version = "1.0.0"

type ConnectStatus int

const (
	StatusDisconnected ConnectStatus = iota
	StatusConnecting
	StatusConnected
)

type ConfigItem struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Group string `json:"group"`
}

type ActionMessage struct {
	Module string `json:"module"`
	Action string `json:"action"`
	Data   string `json:"data"`
}

type ReloadEvent struct {
	Old map[string]string
	New map[string]string
}

type ServiceStatus int

const (
	ServiceUnhealthy ServiceStatus = iota
	ServiceHealthy
)

type ServiceInfo struct {
	ServiceID   string        `json:"serviceId"`
	ServiceName string        `json:"serviceName"`
	IP          string        `json:"ip"`
	Port        *int          `json:"port"`
	Metadata    []string      `json:"metaData"`
	Status      ServiceStatus `json:"status"`
}

func (s ServiceInfo) HTTPHost() string  { return s.host("http") }
func (s ServiceInfo) HTTPSHost() string { return s.host("https") }
func (s ServiceInfo) WSHost() string    { return s.host("ws") }
func (s ServiceInfo) WSSHost() string   { return s.host("wss") }
func (s ServiceInfo) TCPHost() string   { return s.host("tcp") }

func (s ServiceInfo) host(scheme string) string {
	if s.Port == nil {
		return fmt.Sprintf("%s://%s", scheme, s.IP)
	}
	return scheme + "://" + net.JoinHostPort(s.IP, strconv.Itoa(*s.Port))
}

type ServiceRegisterInfo struct {
	ServiceID          string   `json:"serviceId"`
	ServiceName        string   `json:"serviceName"`
	IP                 string   `json:"ip"`
	Port               *int     `json:"port"`
	Metadata           []string `json:"metaData"`
	AlarmURL           string   `json:"alarmUrl"`
	CheckURL           string   `json:"checkUrl"`
	HeartbeatMode      string   `json:"heartBeatMode"`
	HeartbeatInterval  int      `json:"interval"`
	ReregisterInterval int      `json:"-"`
}
