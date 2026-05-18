package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mordilloSan/indexer/internal/configfile"
)

func queryDaemonSetupConfig(socketPath, listenAddr string) (setupConfig, error) {
	body, err := fetchDaemonBody(socketPath, listenAddr, "/config")
	if err != nil {
		return setupConfig{}, err
	}

	var resp struct {
		IndexName            string `json:"index_name"`
		IndexPath            string `json:"index_path"`
		IncludeHidden        bool   `json:"include_hidden"`
		IncludeNetworkMounts bool   `json:"include_network_mounts"`
		FreshIndex           bool   `json:"fresh_index"`
		KeepIndexes          int    `json:"keep_indexes"`
		DBPath               string `json:"db_path"`
		DBBusyTimeout        string `json:"db_busy_timeout"`
		DBJournalMode        string `json:"db_journal_mode"`
		DBSynchronous        string `json:"db_synchronous"`
		DBAutoVacuum         string `json:"db_auto_vacuum"`
		DBMaxOpenConns       int    `json:"db_max_open_conns"`
		DBMaxIdleConns       int    `json:"db_max_idle_conns"`
		DBConnMaxIdleTime    string `json:"db_conn_max_idle_time"`
		SocketPath           string `json:"socket_path"`
		ListenAddr           string `json:"listen_addr"`
		Interval             string `json:"interval"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return setupConfig{}, fmt.Errorf("parse daemon config: %w", err)
	}

	socketPath = resp.SocketPath
	if socketPath == "" {
		socketPath = "-"
	}
	interval := resp.Interval
	if interval == "0s" {
		interval = "0"
	}
	return setupConfig{
		Config: configfile.Config{
			IndexPath:            resp.IndexPath,
			IndexName:            resp.IndexName,
			IncludeHidden:        resp.IncludeHidden,
			IncludeNetworkMounts: resp.IncludeNetworkMounts,
			FreshIndex:           resp.FreshIndex,
			KeepIndexes:          resp.KeepIndexes,
			DBPath:               resp.DBPath,
			DBBusyTimeout:        resp.DBBusyTimeout,
			DBJournalMode:        resp.DBJournalMode,
			DBSynchronous:        resp.DBSynchronous,
			DBAutoVacuum:         resp.DBAutoVacuum,
			DBMaxOpenConns:       resp.DBMaxOpenConns,
			DBMaxIdleConns:       resp.DBMaxIdleConns,
			DBConnMaxIdleTime:    resp.DBConnMaxIdleTime,
			SocketPath:           socketPath,
			Interval:             interval,
			ListenAddr:           resp.ListenAddr,
		},
	}, nil
}

// queryDaemon sends a GET request to the given endpoint on the running daemon and prints the response.
func queryDaemon(socketPath, listenAddr, endpoint string) error {
	body, err := fetchDaemonBody(socketPath, listenAddr, endpoint)
	if err != nil {
		return err
	}
	writelnOrExit(os.Stdout, strings.TrimSpace(string(body)))
	return nil
}

// queryDaemonPretty is like queryDaemon but pretty-prints JSON responses.
func queryDaemonPretty(socketPath, listenAddr, endpoint string) error {
	body, err := fetchDaemonBody(socketPath, listenAddr, endpoint)
	if err != nil {
		return err
	}

	var out any
	if err := json.Unmarshal(body, &out); err != nil {
		writelnOrExit(os.Stdout, strings.TrimSpace(string(body)))
		return nil
	}
	pretty, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		writelnOrExit(os.Stdout, strings.TrimSpace(string(body)))
		return nil
	}
	writelnOrExit(os.Stdout, string(pretty))
	return nil
}

func fetchDaemonBody(socketPath, listenAddr, endpoint string) ([]byte, error) {
	return daemonBodyFetcher(http.MethodGet, socketPath, listenAddr, endpoint)
}

func postDaemonBody(socketPath, listenAddr, endpoint string) ([]byte, error) {
	return daemonBodyFetcher(http.MethodPost, socketPath, listenAddr, endpoint)
}

var daemonBodyFetcher = daemonBody

func daemonBody(method, socketPath, listenAddr, endpoint string) ([]byte, error) {
	client, url, err := buildClient(socketPath, listenAddr, endpoint)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			writefOrExit(os.Stderr, "warning: failed to close response body: %v\n", closeErr)
		}
	}()

	body, err := ioReadAllLimit(resp.Body, 2<<20)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// buildClient returns an HTTP client and URL for reaching the daemon at the given endpoint.
func buildClient(socketPath, listenAddr, endpoint string) (*http.Client, string, error) {
	timeout := 5 * time.Second

	if listenAddr != "" {
		base := listenAddr
		if strings.HasPrefix(base, "http://") || strings.HasPrefix(base, "https://") {
			return &http.Client{Timeout: timeout}, strings.TrimRight(base, "/") + endpoint, nil
		}
		if strings.HasPrefix(base, ":") {
			base = "127.0.0.1" + base
		}
		return &http.Client{Timeout: timeout}, "http://" + strings.TrimRight(base, "/") + endpoint, nil
	}

	if socketPath == "-" {
		return nil, "", fmt.Errorf("requires either --listen or a unix socket (got --socket-path '-')")
	}
	if socketPath == "" {
		socketPath = "/var/run/indexer.sock"
	}

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &http.Client{Transport: tr, Timeout: timeout}, "http://unix" + endpoint, nil
}
