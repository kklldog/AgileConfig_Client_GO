package agileconfig

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	configModule          = "c"
	registerModule        = "r"
	publishTimelineHeader = "publish-time-line-id"
	maxResponseSize       = 64 << 20
)

type configValue struct {
	key   string
	value string
}

type Client struct {
	options    Options
	nodes      []string
	httpClient *http.Client
	dialer     *websocket.Dialer
	logger     Logger

	mu                   sync.RWMutex
	status               ConnectStatus
	values               map[string]configValue
	configs              []ConfigItem
	currentTimelineID    string
	lastLoadedFromServer time.Time
	loadedFromCache      bool
	websocket            *websocket.Conn
	started              bool
	lifecycleContext     context.Context
	cancel               context.CancelFunc

	loadMu  sync.Mutex
	writeMu sync.Mutex
	wg      sync.WaitGroup

	callbackMu      sync.RWMutex
	reloadCallbacks map[uint64]func(ReloadEvent)
	nextCallbackID  uint64

	discoveryMu sync.RWMutex
	discovery   *Discovery

	registerMu sync.RWMutex
	registered bool
	uniqueID   string
}

func New(options Options) (*Client, error) {
	normalized, nodes, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	return &Client{
		options:         normalized,
		nodes:           nodes,
		httpClient:      normalized.HTTPClient,
		dialer:          normalized.WebSocketDialer,
		logger:          normalized.Logger,
		status:          StatusDisconnected,
		values:          make(map[string]configValue),
		reloadCallbacks: make(map[uint64]func(ReloadEvent)),
	}, nil
}

func NewFromFile(path string) (*Client, error) {
	options, err := LoadOptions(path)
	if err != nil {
		return nil, err
	}
	return New(options)
}

func (c *Client) Options() Options {
	options := c.options
	if options.ServiceRegister != nil {
		register := *options.ServiceRegister
		register.Metadata = append([]string(nil), register.Metadata...)
		options.ServiceRegister = &register
	}
	return options
}

func (c *Client) Status() ConnectStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

func (c *Client) LoadedFromCache() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.loadedFromCache
}

func (c *Client) LastLoadedFromServer() (time.Time, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastLoadedFromServer, !c.lastLoadedFromServer.IsZero()
}

func (c *Client) CurrentPublishTimelineID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentTimelineID
}

func (c *Client) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value, ok := c.values[strings.ToLower(key)]
	return value.value, ok
}

func (c *Client) Data() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return copyValues(c.values)
}

func (c *Client) GetGroup(group string) []ConfigItem {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]ConfigItem, 0)
	for _, item := range c.configs {
		if item.Group == group {
			result = append(result, item)
		}
	}
	return result
}

func (c *Client) SubscribeReload(callback func(ReloadEvent)) func() {
	if callback == nil {
		return func() {}
	}
	c.callbackMu.Lock()
	id := c.nextCallbackID
	c.nextCallbackID++
	c.reloadCallbacks[id] = callback
	c.callbackMu.Unlock()
	return func() {
		c.callbackMu.Lock()
		delete(c.reloadCallbacks, id)
		c.callbackMu.Unlock()
	}
}

func (c *Client) LoadConfigs(configs []ConfigItem) {
	newValues := make(map[string]configValue, len(configs))
	copiedConfigs := append([]ConfigItem(nil), configs...)
	for _, item := range copiedConfigs {
		key := item.Key
		if item.Group != "" {
			key = item.Group + ":" + item.Key
		}
		canonical := strings.ToLower(key)
		if _, exists := newValues[canonical]; !exists {
			newValues[canonical] = configValue{key: key, value: item.Value}
		}
	}

	c.mu.Lock()
	oldData := copyValues(c.values)
	c.values = newValues
	c.configs = copiedConfigs
	newData := copyValues(c.values)
	c.mu.Unlock()

	event := ReloadEvent{Old: oldData, New: newData}
	c.callbackMu.RLock()
	callbacks := make([]func(ReloadEvent), 0, len(c.reloadCallbacks))
	for _, callback := range c.reloadCallbacks {
		callbacks = append(callbacks, callback)
	}
	c.callbackMu.RUnlock()
	for _, callback := range callbacks {
		callback(event)
	}
}

func (c *Client) DataMD5Version() string {
	data := c.Data()
	keys := make([]string, 0, len(data))
	values := make([]string, 0, len(data))
	for key, value := range data {
		keys = append(keys, key)
		values = append(values, value)
	}
	sort.Strings(keys)
	sort.Strings(values)
	return md5Upper(strings.Join(keys, "&") + "&" + strings.Join(values, "&"))
}

func (c *Client) Connect(ctx context.Context) error {
	if ctx == nil {
		return errors.New("connect context is required")
	}

	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return nil
	}
	lifecycleContext, cancel := context.WithCancel(ctx)
	c.lifecycleContext = lifecycleContext
	c.cancel = cancel
	c.started = true
	c.mu.Unlock()

	websocketErr := c.connectWebSocket(lifecycleContext)
	if websocketErr != nil {
		c.logger.Printf("agileconfig: initial WebSocket connection failed; background reconnect enabled: %v", websocketErr)
	}
	loadErr := c.Load(lifecycleContext)

	c.wg.Add(1)
	go c.reconnectLoop(lifecycleContext)
	if c.options.ServiceRegister != nil {
		c.wg.Add(1)
		go c.registrationLoop(lifecycleContext)
	}

	if loadErr != nil {
		if websocketErr != nil {
			return combineErrors("connect", []error{websocketErr, loadErr})
		}
		return loadErr
	}
	return nil
}

func (c *Client) Load(ctx context.Context) error {
	if ctx == nil {
		return errors.New("load context is required")
	}
	c.loadMu.Lock()
	defer c.loadMu.Unlock()

	var remoteErrors []error
	for _, node := range randomNodeOrder(c.nodes) {
		if err := c.loadFromNode(ctx, node); err != nil {
			remoteErrors = append(remoteErrors, err)
			continue
		}
		return nil
	}

	if err := c.loadConfigCache(); err == nil {
		c.logger.Printf("agileconfig: all server nodes failed; configurations loaded from local cache")
		return nil
	} else {
		remoteErrors = append(remoteErrors, err)
	}
	return combineErrors("load configurations", remoteErrors)
}

func (c *Client) loadFromNode(ctx context.Context, node string) error {
	appID := url.QueryEscape(c.options.AppID)
	endpoint, err := buildEndpoint(node, "api/config/app")
	if err != nil {
		return err
	}
	endpoint.RawPath = strings.TrimRight(endpoint.EscapedPath(), "/") + "/" + url.PathEscape(c.options.AppID)
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + c.options.AppID
	query := endpoint.Query()
	query.Set("env", c.options.Env)
	endpoint.RawQuery = query.Encode()

	requestContext, cancel := context.WithTimeout(ctx, c.options.HTTPTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("create config request for %s: %w", node, err)
	}
	request.Header.Set("appid", appID)
	request.Header.Set("Authorization", basicAuthorization(c.options.AppID, c.options.Secret))

	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("load configurations from %s: %w", node, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(ioutil.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("load configurations from %s: unexpected HTTP status %s", node, response.Status)
	}

	content, err := readLimited(response.Body)
	if err != nil {
		return fmt.Errorf("read configurations from %s: %w", node, err)
	}
	if err := c.applyConfigContent(content); err != nil {
		return fmt.Errorf("decode configurations from %s: %w", node, err)
	}

	c.mu.Lock()
	c.currentTimelineID = response.Header.Get(publishTimelineHeader)
	c.lastLoadedFromServer = time.Now()
	c.loadedFromCache = false
	c.mu.Unlock()

	if err := c.writeConfigCache(content); err != nil {
		c.logger.Printf("agileconfig: cache configurations: %v", err)
	}
	if err := c.sendCurrentWebSocket("loaded"); err != nil && !errors.Is(err, errWebSocketUnavailable) {
		c.logger.Printf("agileconfig: send loaded notice: %v", err)
	}
	return nil
}

func (c *Client) connectWebSocket(ctx context.Context) error {
	c.mu.Lock()
	if c.websocket != nil {
		c.mu.Unlock()
		return nil
	}
	c.status = StatusConnecting
	c.mu.Unlock()

	var connectionErrors []error
	for _, node := range randomNodeOrder(c.nodes) {
		wsURL, err := c.webSocketURL(node)
		if err != nil {
			connectionErrors = append(connectionErrors, err)
			continue
		}
		headers := http.Header{}
		headers.Set("appid", url.QueryEscape(c.options.AppID))
		headers.Set("env", c.options.Env)
		headers.Set("Authorization", basicAuthorization(c.options.AppID, c.options.Secret))
		headers.Set("client-v", Version)

		connection, response, err := c.dialer.DialContext(ctx, wsURL, headers)
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err != nil {
			connectionErrors = append(connectionErrors, fmt.Errorf("connect WebSocket %s: %w", node, err))
			continue
		}

		connection.SetReadLimit(maxResponseSize)
		c.mu.Lock()
		if ctx.Err() != nil {
			c.status = StatusDisconnected
			c.mu.Unlock()
			connection.Close()
			return ctx.Err()
		}
		c.websocket = connection
		c.status = StatusConnected
		c.mu.Unlock()
		c.startWebSocketLoops(ctx, connection)
		return nil
	}

	c.mu.Lock()
	c.status = StatusDisconnected
	c.mu.Unlock()
	return combineErrors("connect WebSocket", connectionErrors)
}

func (c *Client) webSocketURL(node string) (string, error) {
	endpoint, err := buildEndpoint(node, "ws")
	if err != nil {
		return "", err
	}
	if endpoint.Scheme == "https" {
		endpoint.Scheme = "wss"
	} else {
		endpoint.Scheme = "ws"
	}
	query := endpoint.Query()
	query.Set("client_name", c.options.Name)
	query.Set("client_tag", c.options.Tag)
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

func (c *Client) startWebSocketLoops(ctx context.Context, connection *websocket.Conn) {
	c.wg.Add(2)
	go c.webSocketReadLoop(ctx, connection)
	go c.webSocketHeartbeatLoop(ctx, connection)
}

func (c *Client) webSocketReadLoop(ctx context.Context, connection *websocket.Conn) {
	defer c.wg.Done()
	defer c.clearWebSocket(connection)
	for {
		messageType, message, err := connection.ReadMessage()
		if err != nil {
			if ctx.Err() == nil {
				c.logger.Printf("agileconfig: receive WebSocket message: %v", err)
			}
			return
		}
		if messageType != websocket.TextMessage {
			continue
		}
		c.handleMessage(ctx, string(message))
	}
}

func (c *Client) webSocketHeartbeatLoop(ctx context.Context, connection *websocket.Conn) {
	defer c.wg.Done()
	ticker := time.NewTicker(c.options.WebSocketHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.sendWebSocket(connection, "ping"); err != nil {
				if ctx.Err() == nil && !errors.Is(err, errWebSocketUnavailable) {
					c.logger.Printf("agileconfig: send WebSocket heartbeat: %v", err)
				}
				return
			}
		}
	}
}

func (c *Client) handleMessage(ctx context.Context, message string) {
	if message == "" || message == "0" {
		return
	}
	if strings.HasPrefix(message, "V:") {
		if strings.TrimPrefix(message, "V:") != c.DataMD5Version() {
			c.loadInBackground(ctx)
		}
		return
	}

	var action ActionMessage
	if err := json.Unmarshal([]byte(message), &action); err != nil {
		return
	}
	switch action.Module {
	case "", configModule:
		switch action.Action {
		case "offline":
			go func() {
				if err := c.Close(context.Background()); err != nil {
					c.logger.Printf("agileconfig: process offline command: %v", err)
				}
			}()
		case "reload":
			c.loadInBackground(ctx)
		case "ping":
			if action.Data != c.localVersion() {
				c.loadInBackground(ctx)
			}
		}
	case registerModule:
		c.discoveryMu.RLock()
		discovery := c.discovery
		c.discoveryMu.RUnlock()
		if discovery != nil {
			discovery.handleAction(ctx, action)
		}
	}
}

func (c *Client) loadInBackground(ctx context.Context) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		if err := c.Load(ctx); err != nil && ctx.Err() == nil {
			c.logger.Printf("agileconfig: reload configurations: %v", err)
		}
	}()
}

func (c *Client) localVersion() string {
	c.mu.RLock()
	timelineID := c.currentTimelineID
	c.mu.RUnlock()
	if timelineID != "" {
		return timelineID
	}
	return c.DataMD5Version()
}

func (c *Client) reconnectLoop(ctx context.Context) {
	defer c.wg.Done()
	ticker := time.NewTicker(c.options.ReconnectInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.RLock()
			connected := c.websocket != nil
			c.mu.RUnlock()
			if connected {
				continue
			}
			if err := c.connectWebSocket(ctx); err != nil {
				if ctx.Err() == nil {
					c.logger.Printf("agileconfig: reconnect WebSocket: %v", err)
				}
				continue
			}
			if err := c.Load(ctx); err != nil && ctx.Err() == nil {
				c.logger.Printf("agileconfig: reload after reconnect: %v", err)
			}
		}
	}
}

func (c *Client) clearWebSocket(connection *websocket.Conn) {
	c.mu.Lock()
	if c.websocket == connection {
		c.websocket = nil
		c.status = StatusDisconnected
	}
	c.mu.Unlock()
	connection.Close()
}

var errWebSocketUnavailable = errors.New("WebSocket is not connected")

func (c *Client) sendCurrentWebSocket(message string) error {
	c.mu.RLock()
	connection := c.websocket
	c.mu.RUnlock()
	if connection == nil {
		return errWebSocketUnavailable
	}
	return c.sendWebSocket(connection, message)
}

func (c *Client) sendWebSocket(connection *websocket.Conn, message string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.RLock()
	current := c.websocket == connection
	c.mu.RUnlock()
	if !current {
		return errWebSocketUnavailable
	}
	return connection.WriteMessage(websocket.TextMessage, []byte(message))
}

func (c *Client) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("close context is required")
	}

	c.mu.Lock()
	if !c.started {
		c.status = StatusDisconnected
		c.mu.Unlock()
		if c.options.ServiceRegister != nil {
			return c.Unregister(ctx)
		}
		return nil
	}
	cancel := c.cancel
	connection := c.websocket
	c.websocket = nil
	c.status = StatusDisconnected
	c.started = false
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	var unregisterErr error
	if c.options.ServiceRegister != nil {
		unregisterErr = c.Unregister(ctx)
	}
	if connection != nil {
		c.writeMu.Lock()
		_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		c.writeMu.Unlock()
		connection.Close()
	}

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return unregisterErr
	case <-ctx.Done():
		if unregisterErr != nil {
			return combineErrors("close client", []error{unregisterErr, ctx.Err()})
		}
		return ctx.Err()
	}
}

func (c *Client) applyConfigContent(content []byte) error {
	var configs []ConfigItem
	if err := json.Unmarshal(content, &configs); err != nil {
		return err
	}
	c.LoadConfigs(configs)
	return nil
}

func (c *Client) configCachePath() string {
	return filepath.Join(c.options.Cache.Directory, c.options.AppID+".agileconfig.client.configs.cache")
}

func (c *Client) writeConfigCache(content []byte) error {
	if c.options.Cache.Disabled || len(content) == 0 {
		return nil
	}
	if err := ensureDirectory(c.options.Cache.Directory); err != nil {
		return err
	}
	data := content
	if c.options.Cache.Encrypt {
		encrypted, err := encryptCache(c.options.Secret, content)
		if err != nil {
			return err
		}
		data = []byte(encrypted)
	}
	if err := ioutil.WriteFile(c.configCachePath(), data, 0600); err != nil {
		return fmt.Errorf("write config cache: %w", err)
	}
	return nil
}

func (c *Client) loadConfigCache() error {
	content, err := ioutil.ReadFile(c.configCachePath())
	if err != nil {
		return fmt.Errorf("read config cache: %w", err)
	}
	if c.options.Cache.Encrypt {
		content, err = decryptCache(c.options.Secret, string(content))
		if err != nil {
			return fmt.Errorf("decrypt config cache: %w", err)
		}
	}
	if err := c.applyConfigContent(content); err != nil {
		return fmt.Errorf("decode config cache: %w", err)
	}
	c.mu.Lock()
	c.loadedFromCache = true
	c.currentTimelineID = ""
	c.mu.Unlock()
	return nil
}

func copyValues(values map[string]configValue) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		result[value.key] = value.value
	}
	return result
}

func basicAuthorization(appID, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(appID+":"+secret))
}

func buildEndpoint(node, endpointPath string) (*url.URL, error) {
	parsed, err := url.Parse(node)
	if err != nil {
		return nil, err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + strings.TrimLeft(endpointPath, "/")
	return parsed, nil
}

func readLimited(reader io.Reader) ([]byte, error) {
	content, err := ioutil.ReadAll(io.LimitReader(reader, maxResponseSize+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxResponseSize {
		return nil, fmt.Errorf("response exceeds %d bytes", maxResponseSize)
	}
	return content, nil
}

func ensureDirectory(directory string) error {
	if directory == "" {
		return nil
	}
	if err := os.MkdirAll(directory, 0755); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}
	return nil
}

func randomNodeOrder(nodes []string) []string {
	result := append([]string(nil), nodes...)
	if len(result) < 2 {
		return result
	}
	index, err := rand.Int(rand.Reader, big.NewInt(int64(len(result))))
	if err != nil {
		return result
	}
	start := int(index.Int64())
	return append(result[start:], result[:start]...)
}

func combineErrors(operation string, errs []error) error {
	messages := make([]string, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			messages = append(messages, err.Error())
		}
	}
	if len(messages) == 0 {
		return fmt.Errorf("%s failed", operation)
	}
	return fmt.Errorf("%s failed: %s", operation, strings.Join(messages, "; "))
}
