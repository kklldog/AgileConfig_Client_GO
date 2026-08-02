package agileconfig

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Discovery struct {
	client *Client

	mu              sync.RWMutex
	services        []ServiceInfo
	dataVersion     string
	loadedFromCache bool

	refreshMu      sync.Mutex
	callbackMu     sync.RWMutex
	callbacks      map[uint64]func()
	nextCallbackID uint64
}

func NewDiscovery(ctx context.Context, client *Client) (*Discovery, error) {
	if ctx == nil {
		return nil, errors.New("discovery context is required")
	}
	if client == nil {
		return nil, errors.New("client is required")
	}
	discovery := &Discovery{
		client:    client,
		callbacks: make(map[uint64]func()),
	}
	client.discoveryMu.Lock()
	client.discovery = discovery
	client.discoveryMu.Unlock()
	if err := discovery.Refresh(ctx); err != nil {
		return discovery, err
	}
	return discovery, nil
}

func (d *Discovery) DataVersion() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.dataVersion
}

func (d *Discovery) LoadedFromCache() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.loadedFromCache
}

func (d *Discovery) Services() []ServiceInfo {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return copyServices(d.services)
}

func (d *Discovery) HealthyServices() []ServiceInfo {
	return filterServices(d.Services(), func(service ServiceInfo) bool {
		return service.Status == ServiceHealthy
	})
}

func (d *Discovery) UnhealthyServices() []ServiceInfo {
	return filterServices(d.Services(), func(service ServiceInfo) bool {
		return service.Status == ServiceUnhealthy
	})
}

func (d *Discovery) GetByServiceName(name string) []ServiceInfo {
	return filterServices(d.Services(), func(service ServiceInfo) bool {
		return service.ServiceName == name
	})
}

func (d *Discovery) GetByServiceID(id string) (ServiceInfo, bool) {
	for _, service := range d.Services() {
		if service.ServiceID == id {
			return service, true
		}
	}
	return ServiceInfo{}, false
}

func (d *Discovery) RandomOne(serviceName string) (ServiceInfo, bool) {
	services := d.Services()
	if serviceName != "" {
		services = filterServices(services, func(service ServiceInfo) bool {
			return service.ServiceName == serviceName
		})
	}
	if len(services) == 0 {
		return ServiceInfo{}, false
	}
	index, err := rand.Int(rand.Reader, big.NewInt(int64(len(services))))
	if err != nil {
		return services[0], true
	}
	return services[index.Int64()], true
}

func (d *Discovery) SubscribeReload(callback func()) func() {
	if callback == nil {
		return func() {}
	}
	d.callbackMu.Lock()
	id := d.nextCallbackID
	d.nextCallbackID++
	d.callbacks[id] = callback
	d.callbackMu.Unlock()
	return func() {
		d.callbackMu.Lock()
		delete(d.callbacks, id)
		d.callbackMu.Unlock()
	}
}

func (d *Discovery) Refresh(ctx context.Context) error {
	if ctx == nil {
		return errors.New("refresh context is required")
	}
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()

	var refreshErrors []error
	for _, node := range randomNodeOrder(d.client.nodes) {
		if err := d.refreshFromNode(ctx, node); err != nil {
			refreshErrors = append(refreshErrors, err)
			continue
		}
		return nil
	}
	if err := d.loadCache(); err == nil {
		d.client.logger.Printf("agileconfig: all server nodes failed; service discovery loaded from local cache")
		return nil
	} else {
		refreshErrors = append(refreshErrors, err)
	}
	return combineErrors("refresh service discovery", refreshErrors)
}

func (d *Discovery) refreshFromNode(ctx context.Context, node string) error {
	endpoint, err := buildEndpoint(node, "api/registercenter/services")
	if err != nil {
		return err
	}
	requestContext, cancel := context.WithTimeout(ctx, d.client.options.HTTPTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("create discovery request for %s: %w", node, err)
	}
	response, err := d.client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("refresh services from %s: %w", node, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(ioutil.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("refresh services from %s: unexpected HTTP status %s", node, response.Status)
	}
	content, err := readLimited(response.Body)
	if err != nil {
		return fmt.Errorf("read services from %s: %w", node, err)
	}
	services, err := decodeServices(content)
	if err != nil {
		return fmt.Errorf("decode services from %s: %w", node, err)
	}
	d.apply(services, false)
	if err := d.writeCache(content); err != nil {
		d.client.logger.Printf("agileconfig: cache discovered services: %v", err)
	}
	return nil
}

func (d *Discovery) handleAction(ctx context.Context, action ActionMessage) {
	refresh := action.Action == "reload"
	if action.Action == "ping" && !strings.EqualFold(action.Data, d.DataVersion()) {
		refresh = true
	}
	if !refresh {
		return
	}
	go func() {
		if err := d.Refresh(ctx); err != nil && ctx.Err() == nil {
			d.client.logger.Printf("agileconfig: refresh service discovery: %v", err)
		}
	}()
}

func (d *Discovery) apply(services []ServiceInfo, loadedFromCache bool) {
	copied := copyServices(services)
	d.mu.Lock()
	d.services = copied
	d.dataVersion = serviceDataVersion(copied)
	d.loadedFromCache = loadedFromCache
	d.mu.Unlock()

	d.callbackMu.RLock()
	callbacks := make([]func(), 0, len(d.callbacks))
	for _, callback := range d.callbacks {
		callbacks = append(callbacks, callback)
	}
	d.callbackMu.RUnlock()
	for _, callback := range callbacks {
		callback()
	}
}

func (d *Discovery) cachePath() string {
	return filepath.Join(d.client.options.Cache.Directory, d.client.options.AppID+".agileconfig.client.services.cache")
}

func (d *Discovery) writeCache(content []byte) error {
	if d.client.options.Cache.Disabled || len(content) == 0 {
		return nil
	}
	if err := ensureDirectory(d.client.options.Cache.Directory); err != nil {
		return err
	}
	if err := ioutil.WriteFile(d.cachePath(), content, 0600); err != nil {
		return fmt.Errorf("write service cache: %w", err)
	}
	return nil
}

func (d *Discovery) loadCache() error {
	content, err := ioutil.ReadFile(d.cachePath())
	if err != nil {
		return fmt.Errorf("read service cache: %w", err)
	}
	services, err := decodeServices(content)
	if err != nil {
		return fmt.Errorf("decode service cache: %w", err)
	}
	d.apply(services, true)
	return nil
}

func decodeServices(content []byte) ([]ServiceInfo, error) {
	var services []ServiceInfo
	if err := json.Unmarshal(content, &services); err != nil {
		return nil, err
	}
	return services, nil
}

func serviceDataVersion(services []ServiceInfo) string {
	sorted := copyServices(services)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ServiceID < sorted[j].ServiceID
	})
	var plain strings.Builder
	for _, service := range sorted {
		metadata := append([]string(nil), service.Metadata...)
		sort.Strings(metadata)
		port := ""
		if service.Port != nil {
			port = strconv.Itoa(*service.Port)
		}
		fmt.Fprintf(&plain, "%s&%s&%s&%s&%d&%s&",
			service.ServiceID,
			service.ServiceName,
			service.IP,
			port,
			service.Status,
			strings.Join(metadata, ","),
		)
	}
	return md5Upper(plain.String())
}

func copyServices(services []ServiceInfo) []ServiceInfo {
	result := make([]ServiceInfo, len(services))
	for index, service := range services {
		result[index] = service
		result[index].Metadata = append([]string(nil), service.Metadata...)
		if service.Port != nil {
			port := *service.Port
			result[index].Port = &port
		}
	}
	return result
}

func filterServices(services []ServiceInfo, predicate func(ServiceInfo) bool) []ServiceInfo {
	result := make([]ServiceInfo, 0)
	for _, service := range services {
		if predicate(service) {
			result = append(result, service)
		}
	}
	return result
}
