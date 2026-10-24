// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Package connector provides tools for sharing the connection of diverse exporters
// (Prometheus, OTEL...) from different nodes
package connector // import "go.opentelemetry.io/obi/pkg/export/connector"

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"go.opentelemetry.io/obi/pkg/internal/helpers/maps"
)

func log() *slog.Logger {
	return slog.With("component", "connector.PrometheusManager")
}

// PrometheusManager allows exporting metrics from different sources (instrumented metrics, internal metrics...)
// sharing the same port and path, or using different ones, depending on the configuration provided by the registrars.
type PrometheusManager struct {
	// ConstLabels are appended to every series registered after they are set.
	ConstLabels prometheus.Labels

	mt sync.Mutex
	// mux of each port that already has a listener, so a path registered later can be
	// added to the running mux instead of needing a second listener
	muxes map[int]*http.ServeMux
	// (port, path) pairs already wired into a mux. http.ServeMux panics on a duplicate
	// pattern, so a repeated StartHTTP must skip them.
	handled maps.Map2[int, string, struct{}]
	// key 1: port. Key 2: path
	registries maps.Map2[int, string, *prometheus.Registry]

	metrics internalIntrumenter
}

type internalIntrumenter interface {
	PrometheusRequest(port, path string)
}

func (pm *PrometheusManager) InstrumentWith(ii internalIntrumenter) {
	pm.mt.Lock()
	defer pm.mt.Unlock()
	pm.metrics = ii
}

// Register a set of prometheus metrics to be accessible through an HTTP port/path.
// This method is not thread-safe
func (pm *PrometheusManager) Register(port int, path string, collectors ...prometheus.Collector) {
	pm.mt.Lock()
	defer pm.mt.Unlock()
	log().Debug("registering Prometheus metrics collectors",
		"len", len(collectors), "port", port, "path", path)

	if pm.registries == nil {
		pm.registries = maps.Map2[int, string, *prometheus.Registry]{}
	}
	reg, ok := pm.registries.Get(port, path)
	if !ok {
		reg = prometheus.NewRegistry()
		pm.registries.Put(port, path, reg)
	}
	prometheus.WrapRegistererWith(pm.ConstLabels, reg).MustRegister(collectors...)
}

// StartHTTP serves metrics in background. Safe to call repeatedly: each call serves what is
// registered but not served yet, opening a new port or adding a path to a running mux.
func (pm *PrometheusManager) StartHTTP(ctx context.Context) {
	pm.mt.Lock()
	defer pm.mt.Unlock()

	if pm.muxes == nil {
		pm.muxes = map[int]*http.ServeMux{}
	}
	if pm.handled == nil {
		pm.handled = maps.Map2[int, string, struct{}]{}
	}

	log := log()
	for port, paths := range pm.registries {
		mux, listening := pm.muxes[port]
		if !listening {
			mux = http.NewServeMux()
		}
		for path, registry := range paths {
			if _, ok := pm.handled.Get(port, path); ok {
				continue
			}
			log.With("port", port, "path", path).Info("opening prometheus scrape endpoint")
			promHandler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{
				Registry:          registry,
				EnableOpenMetrics: true,
			})
			promHandler = wrapLogHandler(log.With("port", port, "path", path), promHandler)
			promHandler = wrapInstrumentedHandler(pm.metrics, port, path, promHandler)
			// ServeMux takes its own write lock, so this is safe while it serves
			mux.Handle(path, promHandler)
			pm.handled.Put(port, path, struct{}{})
		}
		if !listening {
			pm.muxes[port] = mux
			listenAndServe(ctx, port, mux)
		}
	}
}

func wrapInstrumentedHandler(metrics internalIntrumenter, port int, path string, promHandler http.Handler) http.HandlerFunc {
	// we don't wrap anything if the reporter is nil
	if metrics == nil {
		return promHandler.ServeHTTP
	}
	portStr := strconv.Itoa(port)
	return func(rw http.ResponseWriter, req *http.Request) {
		promHandler.ServeHTTP(rw, req)
		metrics.PrometheusRequest(portStr, path)
	}
}

// statusRecorder captures the status code and body size written by the wrapped handler.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func wrapLogHandler(log *slog.Logger, promHandler http.Handler) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: rw, status: http.StatusOK}
		promHandler.ServeHTTP(rec, req)
		log.Info("metrics scraped",
			"remoteAddr", req.RemoteAddr,
			"userAgent", req.UserAgent(),
			"status", rec.status,
			"bytes", rec.bytes,
			"duration", time.Since(started).Round(time.Millisecond))
	}
}

func listenAndServe(ctx context.Context, port int, handler http.Handler) {
	// TODO: support TLS configuration
	server := http.Server{Addr: fmt.Sprintf(":%d", port), Handler: handler}
	log := log().With("port", port)
	go func() {
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			log.Debug("Prometheus endpoint server was closed", "error", err)
		} else {
			log.Error("Prometheus endpoint service ended unexpectedly", "error", err)
			err = syscall.Kill(os.Getpid(), syscall.SIGINT) // interrupt for graceful shutdown, instead of os.Exit
			if err != nil {
				log.Error("unable to terminate", "error", err)
			}
		}
	}()
	go func() {
		<-ctx.Done()
		if err := server.Close(); err != nil {
			log.Warn("error closing HTTP server", "err", err.Error())
		}
	}()
}
