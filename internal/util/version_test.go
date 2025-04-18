package util

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetLatestRelease(t *testing.T) {
	originalVersion := Version
	defer func() { Version = originalVersion }()

	t.Run("dev version", func(t *testing.T) {
		Version = "dev"
		version, err := GetLatestRelease(context.Background())

		assert.NoError(t, err)
		assert.Equal(t, "dev", version)
	})

	t.Run("github api success", func(t *testing.T) {
		Version = "0.1.0"

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/release/cli", r.URL.Path)
			assert.Contains(t, r.Header.Get("User-Agent"), "Agentuity CLI/")

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"tag_name": "0.2.0"}`))
		}))
		defer server.Close()

		originalClient := http.DefaultClient
		http.DefaultClient = &http.Client{
			Transport: &testTransport{
				originalTransport: http.DefaultTransport,
				server:            server,
			},
		}
		defer func() { http.DefaultClient = originalClient }()

		version, err := GetLatestRelease(context.Background())

		assert.NoError(t, err)
		assert.Equal(t, "0.2.0", version)
	})

	t.Run("github api error", func(t *testing.T) {
		Version = "0.1.0"

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		originalClient := http.DefaultClient
		http.DefaultClient = &http.Client{
			Transport: &testTransport{
				originalTransport: http.DefaultTransport,
				server:            server,
			},
		}
		defer func() { http.DefaultClient = originalClient }()

		_, err := GetLatestRelease(context.Background())

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to check for latest release")
	})
}

type testTransport struct {
	originalTransport http.RoundTripper
	server            *httptest.Server
}

func (t *testTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "agentuity.sh" {
		req.URL.Scheme = "http"
		req.URL.Host = t.server.Listener.Addr().String()
	}
	return t.originalTransport.RoundTrip(req)
}

func TestCheckLatestRelease(t *testing.T) {
	originalVersion := Version
	defer func() { Version = originalVersion }()

	logger := &mockLogger{}

	t.Run("dev version", func(t *testing.T) {
		Version = "dev"
		_, err := CheckLatestRelease(context.Background(), logger, false)
		assert.NoError(t, err)
	})
}
