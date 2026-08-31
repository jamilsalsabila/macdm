// Command macdmd is the MacDM daemon: it owns the job store and download engine
// and serves the loopback REST + SSE API that the CLI, the menu-bar app, and the
// browser native-messaging host all talk to.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"macdm/internal/api"
	"macdm/internal/config"
	"macdm/internal/engine"
	"macdm/internal/manager"
	"macdm/internal/store"
	"macdm/internal/tools"
)

func main() {
	cfg := config.Load()

	addr := flag.String("addr", cfg.Addr, "loopback listen address")
	dir := flag.String("dir", cfg.DownloadDir, "download directory")
	flag.Parse()

	// Claim the port FIRST. ResumeAll below restarts every unfinished job, and
	// binding only inside the serving goroutine meant a second macdmd would
	// resume all of them — racing the running daemon on the same .part files and
	// jobs.json — before discovering the address was taken and dying via
	// log.Fatalf, which skips every deferred cleanup.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v (is another macdmd already running?)", *addr, err)
	}

	st, err := store.Open(config.StorePath())
	if err != nil {
		_ = ln.Close()
		log.Fatalf("open store: %v", err)
	}
	defer st.Close() // final flush of any pending progress write

	toolset := tools.Resolve(cfg)
	log.Printf("tools: ffmpeg=%q yt-dlp=%q", toolset.Ffmpeg, toolset.YtDlp)

	mgr := manager.New(manager.Config{
		DownloadDir:      *dir,
		MaxActive:        cfg.MaxActive,
		Tools:            toolset,
		CookiesFrom:      cfg.CookiesFrom,
		AutoAccept:       cfg.AutoAccept,
		PromptTimeoutSec: cfg.PromptTimeoutSec,
		Engine: engine.Config{
			MaxConns:  cfg.MaxConns,
			MinChunk:  1 << 20,
			UserAgent: engine.DefaultUserAgent,
			Timeout:   30 * time.Second,
		},
	}, st)
	mgr.ResumeAll()

	// Keep the managed yt-dlp binary current in the background (daily check).
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	go tools.AutoUpdateLoop(bgCtx, cfg)

	srv := &http.Server{
		Handler:           api.New(mgr),
		ReadHeaderTimeout: 5 * time.Second,
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("macdmd listening on http://%s  (downloads -> %s)", *addr, *dir)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			// Not Fatalf: that calls os.Exit and would skip st.Close()'s final
			// flush and the manager shutdown.
			log.Printf("serve: %v", err)
			sig <- syscall.SIGTERM
		}
	}()

	<-sig
	log.Print("shutting down")
	bgCancel()

	mgr.Shutdown(4 * time.Second) // stop jobs + kill yt-dlp/ffmpeg children

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
