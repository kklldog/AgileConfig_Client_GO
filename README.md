# AgileConfig Go Client

AgileConfig 的 Go 客户端，支持配置拉取、WebSocket 实时更新、本地缓存、服务注册与服务发现。

## 环境要求

- Go 1.17+
- 可访问的 AgileConfig 服务端

## 安装

```bash
go get github.com/kklldog/AgileConfig_Client_GO
```

## 快速开始

```go
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	agileconfig "github.com/kklldog/AgileConfig_Client_GO"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := agileconfig.New(agileconfig.Options{
		AppID:  "your-app-id",
		Secret: "your-secret",
		Nodes:  "http://127.0.0.1:5000",
		Env:    "DEV",
		Cache: agileconfig.CacheOptions{
			Directory: ".agileconfig",
			Encrypt:   true,
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	if err := client.Connect(ctx); err != nil {
		log.Fatal(err)
	}

	if value, ok := client.Get("database:connection"); ok {
		log.Println(value)
	}

	<-ctx.Done()
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Close(closeCtx); err != nil {
		log.Printf("close AgileConfig client: %v", err)
	}
}
```

`Connect` 会执行首次配置加载，并启动 WebSocket 心跳、配置更新和断线重连。若只需要主动拉取配置，可以调用 `client.Load(ctx)`。

`Get` 不区分键名大小写。存在分组时使用 `group:key` 访问，例如分组 `database` 下的键 `connection` 对应 `database:connection`。

## 监听配置更新

```go
unsubscribe := client.SubscribeReload(func(event agileconfig.ReloadEvent) {
	log.Printf("config changed: before=%v after=%v", event.Old, event.New)
})
defer unsubscribe()
```

还可以使用以下方法读取配置状态：

- `Data()`：返回全部配置的副本
- `GetGroup(group)`：返回指定分组的配置项
- `Status()`：返回 WebSocket 连接状态
- `LoadedFromCache()`：判断当前配置是否来自本地缓存
- `LastLoadedFromServer()`：返回最近一次从服务端加载配置的时间
- `CurrentPublishTimelineID()`：返回当前发布版本标识

## 从文件创建客户端

使用 `NewFromFile` 读取 JSON 配置：

```go
client, err := agileconfig.NewFromFile("appsettings.json")
```

配置文件既可以直接包含配置项，也可以使用 `AgileConfig` 包装：

```json
{
  "AgileConfig": {
    "appId": "your-app-id",
    "secret": "your-secret",
    "nodes": "http://127.0.0.1:5000,http://127.0.0.1:5001",
    "name": "order-service",
    "tag": "v1",
    "env": "DEV",
    "httpTimeout": 10,
    "reconnectInterval": 5,
    "cache": {
      "enabled": true,
      "directory": ".agileconfig",
      "config_encrypt": true
    }
  }
}
```

`httpTimeout` 和 `reconnectInterval` 的单位为秒。`nodes` 支持多个以逗号分隔的 HTTP/HTTPS 地址。

## 本地缓存

本地缓存默认启用。服务端节点全部不可用时，客户端会尝试从缓存恢复配置；可通过 `LoadedFromCache()` 判断数据来源。

```go
Cache: agileconfig.CacheOptions{
	Disabled:  false,
	Directory: ".agileconfig",
	Encrypt:   true,
},
```

生产环境建议设置独立且可写的缓存目录。启用 `Encrypt` 后，缓存内容会使用 `Secret` 派生的密钥加密，因此修改 `Secret` 后旧缓存将无法读取。

## 服务注册

在客户端选项中配置 `ServiceRegister` 后，`Connect` 会启动自动注册、心跳和重注册，`Close` 会注销服务。

```go
port := 8080
client, err := agileconfig.New(agileconfig.Options{
	AppID:  "order-service",
	Secret: "your-secret",
	Nodes:  "http://127.0.0.1:5000",
	Env:    "PROD",
	Cache: agileconfig.CacheOptions{
		Directory: ".agileconfig",
	},
	ServiceRegister: &agileconfig.ServiceRegisterInfo{
		ServiceName:       "order-service",
		IP:                "10.0.0.12",
		Port:              &port,
		Metadata:          []string{"region=cn-east", "version=v1"},
		HeartbeatMode:     "client",
		HeartbeatInterval: 30,
	},
})
```

未指定 `ServiceID` 时，客户端会自动生成并写入缓存目录，以便进程重启后继续使用同一 ID。也可以使用 `Register`、`Unregister` 和 `Registered` 手动管理注册状态。

## 服务发现

```go
discovery, err := agileconfig.NewDiscovery(ctx, client)
if err != nil {
	log.Fatal(err)
}

services := discovery.GetByServiceName("payment-service")
for _, service := range services {
	if service.Status == agileconfig.ServiceHealthy {
		log.Println(service.HTTPHost())
	}
}

unsubscribe := discovery.SubscribeReload(func() {
	log.Printf("services changed: %v", discovery.HealthyServices())
})
defer unsubscribe()
```

服务发现支持 `Services`、`HealthyServices`、`UnhealthyServices`、`GetByServiceName`、`GetByServiceID` 和 `RandomOne`。`ServiceInfo` 可通过 `HTTPHost`、`HTTPSHost`、`WSHost`、`WSSHost` 或 `TCPHost` 生成对应协议的地址。

## 默认值

| 选项 | 默认值 |
| --- | --- |
| `HTTPTimeout` | 100 秒 |
| `ReconnectInterval` | 5 秒 |
| `WebSocketHeartbeatInterval` | 30 秒 |
| `ServiceRegister.HeartbeatMode` | `client` |
| `ServiceRegister.HeartbeatInterval` | 30 秒 |
| `ServiceRegister.ReregisterInterval` | 5 秒 |

`AppID` 和至少一个有效的 `Nodes` 地址为必填项。

## 测试

```bash
go test ./...
```