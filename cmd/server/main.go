package main

import (
	"context"
	"errors"
	"log"
	"m365-copilot2api/internal/outbound"
	"m365-copilot2api/internal/web"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); dir != "" {
			os.Chdir(dir)
		}
	}
	web.ApplyStartupSettingsEnv()
	if err := outbound.ConfigureFromEnv(); err != nil {
		log.Fatalf("configure outbound proxy: %v", err)
	}
	s, e := web.New()
	if e != nil {
		log.Fatal(e)
	}
	s.InitM365CloudClient()
	s.StartAutoCleanup()
	// Token refresh is a serial network sweep over every authorized account
	// and can run for minutes. It must not delay the HTTP listener: move it to
	// the background so the console and API are reachable immediately, while
	// per-request EnsureValid keeps serving tokens as they are needed.
	go func() {
		log.Println("[token-refresh] background sweep started")
		s.RefreshExpiredTokens()
		log.Println("[token-refresh] background sweep finished")
	}()
	listen := "127.0.0.1:9090"
	if v := os.Getenv("M365_LISTEN"); v != "" {
		listen = v
	}
	log.Printf("m365-copilot2api listening on http://%s\\n", listen)
	server := &http.Server{
		Addr:              listen,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout must not cap large streaming uploads (multi-MB tool
		// results / attachments): the documented ChatTimeoutSeconds governs the
		// full request window, and a 30s read ceiling turned big slow uploads
		// into a mid-request "context canceled" 502. Header-only protection is
		// kept via ReadHeaderTimeout; the body read window stays open-ended,
		// matching the open-ended WriteTimeout below.
		ReadTimeout:  0,
		IdleTimeout:  120 * time.Second,
		WriteTimeout: 0, // streaming endpoints need an open-ended write window.
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	web.StopPersistLoop()
	log.Println("shutdown complete")
}
