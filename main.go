package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	DefaultPort     = "2053"
	ConfigFile      = "/app/config.json"
	WSSecretPath    = "/ws-cazar-gate"
	XHTTPSecretPath = "/xh-cazar-gate"
	wsBackendAddr   = "127.0.0.1:8081"
	xhBackendAddr   = "127.0.0.1:8082"
	bufSize         = 128 * 1024
)

var xrayReady atomic.Bool

var bufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, bufSize)
		return &b
	},
}

type pooledBuffer struct{}

func (pooledBuffer) Get() []byte {
	return *bufferPool.Get().(*[]byte)
}

func (pooledBuffer) Put(b []byte) {
	if b == nil || cap(b) != bufSize {
		return
	}
	b = b[:bufSize]
	bufferPool.Put(&b)
}

var proxyBufferPool = pooledBuffer{}

func routeWithinSecretPath(reqPath, secret string) bool {
	return reqPath == secret || strings.HasPrefix(reqPath, secret+"/")
}

func setTCPNoDelay(fd uintptr) {
	_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
}

func newLoopbackTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   3 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(setTCPNoDelay)
		},
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   128,
		MaxConnsPerHost:       512,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0,
		ExpectContinueTimeout: 0,
		TLSHandshakeTimeout:   0,
		ForceAttemptHTTP2:     false,
		DisableCompression:    true,
		DisableKeepAlives:     false,
		WriteBufferSize:       bufSize,
		ReadBufferSize:        bufSize,
	}
}

func attachReverseProxy(backend *url.URL, name string) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(backend)
	proxy.Transport = newLoopbackTransport()
	proxy.BufferPool = proxyBufferPool
	proxy.FlushInterval = -1
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode == http.StatusSwitchingProtocols {
			return nil
		}
		resp.Header.Set("X-Accel-Buffering", "no")
		resp.Header.Set("Cache-Control", "no-store")
		resp.Header.Del("Content-Length")
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[%s Proxy Error] %v", name, err)
		if r.Context().Err() != nil {
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}
	return proxy
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(400 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

func portOpen(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 800*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func markReadyWhenBound(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
			if portOpen(wsBackendAddr) && portOpen(xhBackendAddr) {
				xrayReady.Store(true)
				log.Println("[Health] Xray inbounds 8081/8082 are accepting TCP")
				return
			}
		}
	}
}

func startXraySupervisor(ctx context.Context, configPath string) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			log.Println("[Xray Supervisor] Spawning Xray-core daemon...")
			cmd := exec.Command("/usr/local/bin/xray", "run", "-config", configPath)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Setpgid:   true,
				Pdeathsig: syscall.SIGKILL,
			}
			cmd.Env = append(os.Environ(), "XRAY_LOCATION_ASSET=/usr/local/share/xray")

			if err := cmd.Start(); err != nil {
				xrayReady.Store(false)
				log.Printf("[Xray Supervisor] Launch error: %v. Retrying in 3s...", err)
				timer := time.NewTimer(3 * time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
					continue
				}
			}

			readyCtx, readyCancel := context.WithCancel(ctx)
			go markReadyWhenBound(readyCtx)

			waitChan := make(chan error, 1)
			go func() { waitChan <- cmd.Wait() }()

			select {
			case <-ctx.Done():
				readyCancel()
				xrayReady.Store(false)
				log.Println("[Xray Supervisor] Shutdown signal received.")
				killProcessGroup(cmd)
				select {
				case <-waitChan:
				case <-time.After(2 * time.Second):
				}
				return
			case err := <-waitChan:
				readyCancel()
				xrayReady.Store(false)
				log.Printf("[Xray Supervisor] Process exited: %v. Re-spawning in 1s...", err)
				timer := time.NewTimer(time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}
	}()
}

func waitForXray(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if xrayReady.Load() && portOpen(wsBackendAddr) && portOpen(xhBackendAddr) {
			return
		}
		if portOpen(wsBackendAddr) && portOpen(xhBackendAddr) {
			xrayReady.Store(true)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.Println("[Health] Timed out waiting for Xray; /healthz stays 503 until both ports bind")
}

func healthz(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	wsOK := portOpen(wsBackendAddr)
	xhOK := portOpen(xhBackendAddr)
	if !xrayReady.Load() || !wsOK || !xhOK {
		if !wsOK || !xhOK {
			xrayReady.Store(false)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("Xray inbounds unhealthy"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func main() {
	appCtx, cancelApp := context.WithCancel(context.Background())
	defer cancelApp()

	startXraySupervisor(appCtx, ConfigFile)
	waitForXray(20 * time.Second)

	port := os.Getenv("PORT")
	if port == "" {
		port = DefaultPort
	}

	wsBackend, _ := url.Parse("http://127.0.0.1:8081")
	xhBackend, _ := url.Parse("http://127.0.0.1:8082")
	wsProxy := attachReverseProxy(wsBackend, "WS")
	xhProxy := attachReverseProxy(xhBackend, "XHTTP")

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqPath := r.URL.Path
		if reqPath == "/healthz" {
			healthz(w)
			return
		}
		if routeWithinSecretPath(reqPath, WSSecretPath) {
			wsProxy.ServeHTTP(w, r)
			return
		}
		if routeWithinSecretPath(reqPath, XHTTPSecretPath) {
			xhProxy.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})

	server := &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       300 * time.Second,
		MaxHeaderBytes:    64 * 1024,
	}

	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		<-stop
		log.Println("[Server] Shutting down gateway...")
		cancelApp()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	log.Printf("[Server] BERMUDA Stealth Gateway running on :%s\n", port)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[Server] Fatal error: %v\n", err)
	}
}
