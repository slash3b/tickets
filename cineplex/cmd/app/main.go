package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	config "cineplex/configs"
	"cineplex/internal/services/fetcher"
	"cineplex/internal/services/sender"
	"cineplex/pkg/env"
	"cineplex/pkg/health"
	http2 "cineplex/pkg/http"
	"cineplex/pkg/logger"
	"cineplex/pkg/otel"

	"go.uber.org/zap"
)

func main() {
	var (
		wg      sync.WaitGroup
		port    = env.Get("PORT", config.Port)
		isDebug = env.Get("DEBUG", "false") == "true"
		timeout = env.Get("TIMEOUT", "10s")
		mux     = http.NewServeMux()
		server  = http.Server{
			Addr:              ":" + port,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}
	)

	lg, logSync := logger.MustNew(config.ServiceName, isDebug)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	defer cancel()

	health.Livez(mux, lg)
	health.Readyz(ctx, mux, lg, time.Second*15)

	_ = timeout

	// Set up OpenTelemetry.
	otelShutdown, err := otel.SetupOTelSDK(ctx)
	if err != nil {
		lg.Error("unable to setup OTEL ", zap.Error(err))
		cancel()
	}

	lg.Info("otel sdk is setup successfully...")

	// Handle shutdown properly so nothing leaks.
	defer func() {
		err = otelShutdown(context.Background())
		if err != nil {
			lg.Error("unable to setup OTEL ", zap.Error(err))
			cancel()
		}
	}()

	wg.Add(2)

	go func() {
		defer wg.Done()

		lg.Info("starting http server...", zap.String("http_port", port))
		lg.Info("/livez path is available", zap.String("http_port", port))
		lg.Info("/readyz path is available", zap.String("http_port", port))

		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			lg.Error("http server error", zap.Error(err))
		}

		lg.Debug("http server has been successfully stopped")
	}()

	go func() {
		defer wg.Done()

		<-ctx.Done()

		lg.Debug("calling http server shutdown...")

		err := server.Shutdown(ctx)
		if err != nil {
			lg.Error("unexpected http server error", zap.Error(err))
		}
	}()

	client, err := http2.NewHttpClientWithCookies(time.Second * 15)
	if err != nil {
		lg.Error("http client error", zap.Error(err))
		cancel()
	}

	dec := fetcher.NewCineplex(client, lg)

	senderservice := sender.New(dec, lg)

	const SCRAPE_INTERVAL = "SCRAPE_INTERVAL"

	interval, err := strconv.Atoi(env.Get(SCRAPE_INTERVAL, "120"))
	if err != nil {
		lg.Warn("unable to parse scrape interval value", zap.Error(err))
		interval = 120
	}

	lg.Info("scrape interval in seconds", zap.Int("interval", interval))

	ticker := time.NewTicker(time.Minute * time.Duration(interval))

	go func() {
		<-ctx.Done()

		lg.Debug("calling ticker stop ...")

		ticker.Stop()
	}()

	go func() {
		lg.Info("starting to fetch movies...")

		wg.Add(1)
		defer wg.Done()

		err := senderservice.Broadcast(ctx)
		if err != nil {
			lg.Error("broadcast error", zap.Error(err))
		}

		for {
			select {
			case <-ctx.Done():
				lg.Debug("ticker has been successfully stopped")

				return
			case <-ticker.C:
				err = senderservice.Broadcast(ctx)
				if err != nil {
					lg.Error("broadcast error", zap.Error(err))
				}
			}
		}
	}()

	<-ctx.Done()

	wg.Wait()

	lg.Info("application has been gracefully shutdown")

	_ = logSync()
}
