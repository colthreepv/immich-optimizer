package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/colthreepv/immich-optimizer/internal/adapter/workerclient"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
func run(args []string) error {
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version") {
		fmt.Println(version)
		return nil
	}
	if len(args) > 0 && args[0] == "health" {
		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:8081/health")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unhealthy: %s", resp.Status)
		}
		return nil
	}
	fs := flag.NewFlagSet("optimizer-worker", flag.ContinueOnError)
	server := fs.String("server-url", os.Getenv("IUO_SERVER_URL"), "server URL")
	id := fs.String("worker-id", os.Getenv("IUO_WORKER_ID"), "worker ID")
	converter := fs.String("converter", "jpeg-to-avif.sh", "converter executable")
	temp := fs.String("temp-dir", "", "temporary directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if *id == "" {
		*id, _ = os.Hostname()
	}
	token := os.Getenv("IUO_WORKER_TOKEN")
	if p := os.Getenv("IUO_WORKER_TOKEN_FILE"); p != "" {
		if token != "" {
			return errors.New("set only one worker token source")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		token = strings.TrimSpace(string(b))
	}
	c, err := workerclient.New(workerclient.Config{ServerURL: *server, Token: token, WorkerID: *id, TempDir: *temp, Converter: *converter})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if ctx.Err() != nil {
			http.Error(w, "stopping", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok\n")
	})
	srv := &http.Server{Addr: "127.0.0.1:8081", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	go func() {
		err := <-errs
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("health server: %v", err)
			stop()
		}
	}()
	c.Run(ctx)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
