package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api"
	"github.com/urnetwork/server/router"
)

func main() {
	usage := `BringYour API server.

Usage:
  api [--port=<port>]
  api -h | --help
  api --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>  Listen port [default: 80].`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}

	quitEvent := server.NewEventWithContext(context.Background())
	defer quitEvent.Set()

	closeFn := quitEvent.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM)
	defer closeFn()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// drain on sigterm: cancel ctx, which triggers graceful http shutdown
	// (stops accepting new conns and waits up to DrainTimeout for in-flight)
	go server.HandleError(func() {
		defer cancel()
		select {
		case <-ctx.Done():
		case <-quitEvent.Ctx.Done():
		}
	})

	// Debugging
	go server.HandleError(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}

			glog.Infof("[api]goroutines=%d/%d\n", runtime.NumGoroutine(), runtime.GOMAXPROCS(0))
		}
	})

	routes := api.Routes()

	port, _ := opts.Int("--port")

	server.Warmup()

	server.StartStatsPusher(ctx)

	glog.Infof(
		"[api]serving %s %s on *:%d\n",
		server.RequireEnv(),
		server.RequireVersion(),
		port,
	)

	listenIpv4, _, listenPort := server.RequireListenIpPort(port)

	reusePort := false

	httpServerOptions := server.HttpServerOptions{
		ReadTimeout:     15 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     5 * time.Minute,
		ShutdownTimeout: 30 * time.Second,
	}
	corsHandler := withCors(router.NewRouter(ctx, routes))

	err = server.HttpListenAndServeWithReusePort(
		ctx,
		net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
		corsHandler,
		reusePort,
		httpServerOptions,
	)
	if err != nil {
		panic(err)
	}
	glog.Infof("[api]close\n")
}

// withCors wraps a handler so browsers running the dashboard can call the beta
// API cross-origin. In production the API is behind a CDN that adds CORS
// headers; the beta server exposes the API directly, so we add them here.
func withCors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			// treat empty origin as a same-site/non-browser request
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, X-Requested-With")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if strings.EqualFold(r.Method, http.MethodOptions) {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
