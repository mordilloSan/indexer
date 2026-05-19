package cmd

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRequireRootMiddleware_AllowsSafeMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			called := false
			handler := requireRootMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))

			req := httptest.NewRequest(method, "/status", nil)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if !called {
				t.Fatal("inner handler was not called")
			}
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
			}
		})
	}
}

func TestRequireRootMiddleware_RejectsMutatingFromTCP(t *testing.T) {
	called := false
	handler := requireRootMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/vacuum", nil)
	req = req.WithContext(withConnectionKind(req.Context(), connectionKindTCP))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if called {
		t.Fatal("inner handler was called")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
	if !strings.Contains(rr.Body.String(), "local Unix socket") {
		t.Fatalf("body = %q, want Unix socket message", rr.Body.String())
	}
}

func TestRequireRootMiddleware_RejectsMutatingUnixWithoutPeerCred(t *testing.T) {
	called := false
	handler := requireRootMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/vacuum", nil)
	req = req.WithContext(withConnectionKind(req.Context(), connectionKindUnix))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if called {
		t.Fatal("inner handler was called")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
	if !strings.Contains(rr.Body.String(), "root privileges") {
		t.Fatalf("body = %q, want root privileges message", rr.Body.String())
	}
}

func TestRequireRootMiddleware_RejectsMutatingNonRootUnix(t *testing.T) {
	called := false
	handler := requireRootMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/vacuum", nil)
	ctx := withConnectionKind(req.Context(), connectionKindUnix)
	ctx = withPeerCred(ctx, peerCred{uid: 1000, gid: 1000})
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if called {
		t.Fatal("inner handler was called")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
	if !strings.Contains(rr.Body.String(), "root privileges") {
		t.Fatalf("body = %q, want root privileges message", rr.Body.String())
	}
}

func TestRequireRootMiddleware_AllowsMutatingRootUnix(t *testing.T) {
	called := false
	handler := requireRootMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/vacuum", nil)
	ctx := withConnectionKind(req.Context(), connectionKindUnix)
	ctx = withPeerCred(ctx, peerCred{uid: 0, gid: 0})
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Fatal("inner handler was not called")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestReadUnixPeerCred(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SO_PEERCRED is Linux-specific")
	}

	socketPath := filepath.Join(t.TempDir(), "peer.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer func() {
		if err := ln.Close(); err != nil {
			t.Fatalf("close listener: %v", err)
		}
	}()

	credCh := make(chan peerCred, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}

		uc, ok := conn.(*net.UnixConn)
		if !ok {
			if closeErr := conn.Close(); closeErr != nil {
				errCh <- fmt.Errorf("close accepted connection: %w", closeErr)
				return
			}
			errCh <- fmt.Errorf("accepted connection is %T, want *net.UnixConn", conn)
			return
		}
		cred, err := readUnixPeerCred(uc)
		if err != nil {
			if closeErr := conn.Close(); closeErr != nil {
				errCh <- fmt.Errorf("close accepted connection after peer cred error: %w", closeErr)
				return
			}
			errCh <- err
			return
		}
		if err := conn.Close(); err != nil {
			errCh <- fmt.Errorf("close accepted connection: %w", err)
			return
		}
		credCh <- cred
	}()

	client, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Fatalf("close client connection: %v", err)
		}
	}()

	select {
	case err := <-errCh:
		t.Fatalf("accept/read peer cred: %v", err)
	case cred := <-credCh:
		if cred.uid != uint32(os.Geteuid()) {
			t.Fatalf("uid = %d, want %d", cred.uid, os.Geteuid())
		}
		if cred.gid != uint32(os.Getegid()) {
			t.Fatalf("gid = %d, want %d", cred.gid, os.Getegid())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for peer credentials")
	}
}

func TestGetUnixListenerCreatesWorldWritableSocket(t *testing.T) {
	t.Setenv("LISTEN_PID", "")
	t.Setenv("LISTEN_FDS", "")

	socketPath := filepath.Join(t.TempDir(), "indexer.sock")
	d := &daemon{
		cfg: DaemonConfig{
			SocketPath: socketPath,
		},
	}

	l, err := d.getUnixListener()
	if err != nil {
		t.Fatalf("get unix listener: %v", err)
	}
	defer func() {
		if err := l.Close(); err != nil {
			t.Fatalf("close listener: %v", err)
		}
	}()

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o666 {
		t.Fatalf("socket mode = %o, want 666", mode)
	}
}
