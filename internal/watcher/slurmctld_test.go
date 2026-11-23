package watcher

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readyzServer(t *testing.T, status int) (string, int) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != slurmctldReadyzPath {
			// A followed redirect would land here and look active.
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if status == http.StatusSeeOther {
			w.Header().Set("Location", "/elsewhere")
		}

		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)

	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	return host, port
}

func TestSlurmctldCheck(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		want    bool
		wantErr bool
	}{
		{"active", http.StatusNoContent, true, false},
		{"active 200", http.StatusOK, true, false},
		{"standby", http.StatusInternalServerError, false, false},
		{"no readyz", http.StatusNotFound, true, false},
		{"redirect not followed", http.StatusSeeOther, false, false},
		{"unauthorized", http.StatusUnauthorized, false, true},
		{"bad gateway", http.StatusBadGateway, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip, port := readyzServer(t, tt.status)

			got, err := newSlurmctldCheck()(context.Background(), ip, port)
			if tt.wantErr {
				require.ErrorIs(t, err, errUnexpectedReadyzStatus)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSlurmctldCheck_Unreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	got, err := newSlurmctldCheck()(context.Background(), "127.0.0.1", port)
	assert.Error(t, err)
	assert.False(t, got)
}
