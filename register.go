package agileconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (c *Client) Registered() (bool, string) {
	c.registerMu.RLock()
	defer c.registerMu.RUnlock()
	return c.registered, c.uniqueID
}

func (c *Client) Register(ctx context.Context) error {
	if ctx == nil {
		return errors.New("register context is required")
	}
	if c.options.ServiceRegister == nil {
		return errors.New("service registration is not configured")
	}
	if registered, _ := c.Registered(); registered {
		return nil
	}

	payload, err := json.Marshal(c.options.ServiceRegister)
	if err != nil {
		return fmt.Errorf("encode service registration: %w", err)
	}
	var registerErrors []error
	for _, node := range randomNodeOrder(c.nodes) {
		content, err := c.serviceRequest(ctx, node, http.MethodPost, "api/registercenter", payload)
		if err != nil {
			registerErrors = append(registerErrors, err)
			continue
		}
		var result struct {
			UniqueID string `json:"uniqueId"`
		}
		if err := json.Unmarshal(content, &result); err != nil {
			registerErrors = append(registerErrors, fmt.Errorf("decode registration response from %s: %w", node, err))
			continue
		}
		if result.UniqueID == "" {
			registerErrors = append(registerErrors, fmt.Errorf("registration response from %s did not contain uniqueId", node))
			continue
		}
		c.registerMu.Lock()
		c.registered = true
		c.uniqueID = result.UniqueID
		c.registerMu.Unlock()
		return nil
	}
	return combineErrors("register service", registerErrors)
}

func (c *Client) Unregister(ctx context.Context) error {
	if ctx == nil {
		return errors.New("unregister context is required")
	}
	if c.options.ServiceRegister == nil {
		return nil
	}
	registered, uniqueID := c.Registered()
	if !registered || uniqueID == "" {
		return nil
	}

	payload, err := json.Marshal(struct {
		ServiceID   string `json:"serviceId"`
		ServiceName string `json:"serviceName"`
	}{
		ServiceID:   c.options.ServiceRegister.ServiceID,
		ServiceName: c.options.ServiceRegister.ServiceName,
	})
	if err != nil {
		return fmt.Errorf("encode service unregister request: %w", err)
	}

	var unregisterErrors []error
	for _, node := range randomNodeOrder(c.nodes) {
		_, err := c.serviceRequest(ctx, node, http.MethodDelete, "api/registercenter/"+url.PathEscape(uniqueID), payload)
		if err != nil {
			unregisterErrors = append(unregisterErrors, err)
			continue
		}
		c.registerMu.Lock()
		c.registered = false
		c.uniqueID = ""
		c.registerMu.Unlock()
		return nil
	}
	return combineErrors("unregister service", unregisterErrors)
}

func (c *Client) registrationLoop(ctx context.Context) {
	defer c.wg.Done()
	registerInterval := time.Duration(c.options.ServiceRegister.ReregisterInterval) * time.Second
	heartbeatInterval := time.Duration(c.options.ServiceRegister.HeartbeatInterval) * time.Second

	for {
		registered, uniqueID := c.Registered()
		if !registered {
			if err := c.Register(ctx); err != nil && ctx.Err() == nil {
				c.logger.Printf("agileconfig: register service: %v", err)
			}
			registered, _ = c.Registered()
		} else if err := c.sendServiceHeartbeat(ctx, uniqueID); err != nil && ctx.Err() == nil {
			c.logger.Printf("agileconfig: send service heartbeat: %v", err)
		}

		delay := heartbeatInterval
		if !registered {
			delay = registerInterval
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

func (c *Client) sendServiceHeartbeat(ctx context.Context, uniqueID string) error {
	if err := c.sendCurrentWebSocket("s:ping:" + uniqueID); err == nil {
		return nil
	} else if !errors.Is(err, errWebSocketUnavailable) {
		c.logger.Printf("agileconfig: WebSocket service heartbeat failed, falling back to HTTP: %v", err)
	}

	payload, err := json.Marshal(struct {
		UniqueID string `json:"uniqueId"`
	}{UniqueID: uniqueID})
	if err != nil {
		return err
	}
	var heartbeatErrors []error
	for _, node := range randomNodeOrder(c.nodes) {
		content, err := c.serviceRequest(ctx, node, http.MethodPost, "api/registercenter/heartbeat", payload)
		if err != nil {
			heartbeatErrors = append(heartbeatErrors, err)
			continue
		}
		if len(content) > 0 {
			c.handleMessage(ctx, string(content))
		}
		return nil
	}
	return combineErrors("send service heartbeat", heartbeatErrors)
}

func (c *Client) serviceRequest(ctx context.Context, node, method, path string, payload []byte) ([]byte, error) {
	endpoint, err := buildEndpoint(node, path)
	if err != nil {
		return nil, err
	}
	requestContext, cancel := context.WithTimeout(ctx, c.options.HTTPTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, method, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create %s request for %s: %w", method, node, err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(ioutil.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("%s %s: unexpected HTTP status %s", method, endpoint, response.Status)
	}
	content, err := readLimited(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read response from %s: %w", endpoint, err)
	}
	return content, nil
}

func serviceEndpoint(node string) string {
	return strings.TrimRight(node, "/") + "/api/registercenter"
}
