package agileconfig

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMD5VersionMatchesDotNetASCIIEncoding(t *testing.T) {
	if got, want := md5Upper("配置"), "EA03FCB8C47822BCE772CF6C07D0EBBB"; got != want {
		t.Errorf("md5Upper() = %q, want %q", got, want)
	}
}

func TestLoadFromNodeUsesCompatibleProtocol(t *testing.T) {
	const (
		appID  = "team/api config"
		secret = "secret"
	)

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if got, want := request.URL.EscapedPath(), "/api/config/app/team%2Fapi%20config"; got != want {
			t.Errorf("request path = %q, want %q", got, want)
		}
		if got, want := request.URL.Query().Get("env"), "DEV"; got != want {
			t.Errorf("env = %q, want %q", got, want)
		}
		if got, want := request.Header.Get("appid"), "team%2Fapi+config"; got != want {
			t.Errorf("appid header = %q, want %q", got, want)
		}
		if got, want := request.Header.Get("Authorization"), basicAuthorization(appID, secret); got != want {
			t.Errorf("Authorization header = %q, want %q", got, want)
		}

		response.Header().Set(publishTimelineHeader, "timeline-42")
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`[{"key":"connection","value":"value","group":"db"}]`))
	}))
	defer server.Close()

	client, err := New(Options{
		AppID:  appID,
		Secret: secret,
		Nodes:  server.URL,
		Env:    "dev",
		Cache:  CacheOptions{Disabled: true},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := client.loadFromNode(context.Background(), server.URL); err != nil {
		t.Fatalf("loadFromNode() error = %v", err)
	}

	if got, ok := client.Get("DB:CONNECTION"); !ok || got != "value" {
		t.Errorf("Get() = %q, %v, want %q, true", got, ok, "value")
	}
	if got, want := client.CurrentPublishTimelineID(), "timeline-42"; got != want {
		t.Errorf("CurrentPublishTimelineID() = %q, want %q", got, want)
	}
	if client.LoadedFromCache() {
		t.Error("LoadedFromCache() = true, want false")
	}
	if _, ok := client.LastLoadedFromServer(); !ok {
		t.Error("LastLoadedFromServer() did not report a server load")
	}
}
