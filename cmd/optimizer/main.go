package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/spf13/viper"

	"github.com/colthreepv/immich-optimizer/internal/adapter/filesystem"
	"github.com/colthreepv/immich-optimizer/internal/adapter/immich"
	"github.com/colthreepv/immich-optimizer/internal/adapter/processor"
	"github.com/colthreepv/immich-optimizer/internal/adapter/proxy"
	"github.com/colthreepv/immich-optimizer/internal/adapter/replacement"
	"github.com/colthreepv/immich-optimizer/internal/adapter/watcher"
	"github.com/colthreepv/immich-optimizer/internal/coordinator"
	"github.com/colthreepv/immich-optimizer/internal/domain/entity"
	"github.com/colthreepv/immich-optimizer/internal/domain/port"
	"github.com/colthreepv/immich-optimizer/internal/jobqueue"
	customlogger "github.com/colthreepv/immich-optimizer/internal/logger"
	"github.com/colthreepv/immich-optimizer/internal/usecase/pipeline"
	"github.com/colthreepv/immich-optimizer/internal/utils"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func parseConfig(args []string) (*entity.AppConfig, bool, error) {
	ac := &entity.AppConfig{}

	viper.SetEnvPrefix("iuo")
	viper.AutomaticEnv()

	_ = viper.BindEnv("mode", "IUO_MODE")
	_ = viper.BindEnv("immich_url", "IUO_IMMICH_URL")
	_ = viper.BindEnv("immich_api_key", "IUO_IMMICH_API_KEY")
	_ = viper.BindEnv("tasks_file", "IUO_TASKS_FILE")
	_ = viper.BindEnv("log_level", "IUO_LOG_LEVEL")
	_ = viper.BindEnv("max_concurrent_requests", "IUO_MAX_CONCURRENT_REQUESTS")
	_ = viper.BindEnv("http_timeout_seconds", "IUO_HTTP_TIMEOUT_SECONDS")

	_ = viper.BindEnv("bind_addr", "IUO_PROXY_BIND_ADDR", "IUO_BIND_ADDR")
	_ = viper.BindEnv("filter_path", "IUO_PROXY_FILTER_PATH", "IUO_FILTER_PATH")
	_ = viper.BindEnv("filter_form_key", "IUO_PROXY_FILTER_FORM_KEY", "IUO_FILTER_FORM_KEY")

	_ = viper.BindEnv("watch_dir", "IUO_WATCHER_WATCH_DIR", "IUO_WATCH_DIR")
	_ = viper.BindEnv("undone_dir", "IUO_WATCHER_UNDONE_DIR", "IUO_UNDONE_DIR")
	_ = viper.BindEnv("inotify_buffer_size", "IUO_WATCHER_INOTIFY_BUFFER_SIZE", "IUO_INOTIFY_BUFFER_SIZE")

	viper.SetDefault("mode", "watcher")
	viper.SetDefault("bind_addr", ":8080")
	viper.SetDefault("filter_path", "/api/assets")
	viper.SetDefault("filter_form_key", "assetData")
	viper.SetDefault("immich_url", "")
	viper.SetDefault("immich_api_key", "")
	viper.SetDefault("watch_dir", "/watch")
	viper.SetDefault("undone_dir", "/undone")
	viper.SetDefault("tasks_file", "tasks.yaml")
	viper.SetDefault("log_level", "info")
	viper.SetDefault("max_concurrent_requests", 10)
	viper.SetDefault("http_timeout_seconds", 120)
	viper.SetDefault("inotify_buffer_size", 8192)

	fs := flag.NewFlagSet("immich-optimizer", flag.ContinueOnError)
	fs.StringVar(&ac.StateDir, "state_dir", os.Getenv("IUO_STATE_DIR"), "Persistent storage-saver state and staging directory (enables verified ingestion)")
	originalFirst := false
	if value := os.Getenv("IUO_ORIGINAL_FIRST"); value != "" {
		var err error
		originalFirst, err = strconv.ParseBool(value)
		if err != nil {
			return nil, false, fmt.Errorf("invalid IUO_ORIGINAL_FIRST: %w", err)
		}
	}
	fs.BoolVar(&ac.OriginalFirst, "original_first", originalFirst, "Upload originals immediately and queue optimization")
	retireOriginal := false
	if value := os.Getenv("IUO_RETIRE_ORIGINAL"); value != "" {
		var err error
		retireOriginal, err = strconv.ParseBool(value)
		if err != nil {
			return nil, false, fmt.Errorf("invalid IUO_RETIRE_ORIGINAL: %w", err)
		}
	}
	fs.BoolVar(&ac.RetireOriginal, "retire_original", retireOriginal, "Trash originals after verified replacement and metadata migration")
	maxSpoolBytes, err := optionalByteSizeEnv("IUO_QUEUE_MAX_SPOOL_BYTES")
	if err != nil {
		return nil, false, err
	}
	maxJobs, err := optionalIntEnv("IUO_QUEUE_MAX_JOBS")
	if err != nil {
		return nil, false, err
	}
	if int64(int(maxJobs)) != maxJobs {
		return nil, false, fmt.Errorf("IUO_QUEUE_MAX_JOBS exceeds platform integer range")
	}
	ac.QueueMaxSpoolBytes = maxSpoolBytes
	fs.Func("queue_max_spool_bytes", "Queue spool limit in bytes or units, e.g. 100GiB (0 uses default)", func(value string) error {
		parsed, err := parseByteSize(value)
		if err == nil {
			ac.QueueMaxSpoolBytes = parsed
		}
		return err
	})
	fs.IntVar(&ac.QueueMaxJobs, "queue_max_jobs", int(maxJobs), "Queue job limit (0 uses default)")
	fs.StringVar(&ac.WorkerBindAddr, "worker_bind_addr", envOr("IUO_WORKER_BIND_ADDR", ":8082"), "Address for the worker API")
	fs.StringVar(&ac.PresetID, "preset_id", envOr("IUO_PRESET_ID", "avif-q50-s6-420-v1"), "Versioned conversion preset identifier")
	fs.BoolVar(&ac.MobileView, "mobile_view", os.Getenv("IUO_MOBILE_VIEW") == "true", "Experimental original-checksum view for sync API responses")
	fs.BoolVar(&ac.ShowVersion, "version", false, "Show the current version")
	fs.StringVar(&ac.Mode, "mode", viper.GetString("mode"), "Operating mode: watcher or proxy (comma-separated for multiple)")
	fs.StringVar(&ac.BindAddr, "bind_addr", viper.GetString("bind_addr"), "Address for reverse proxy server to listen on")
	fs.StringVar(&ac.FilterPath, "filter_path", viper.GetString("filter_path"), "Path pattern to intercept for proxy uploads")
	fs.StringVar(&ac.FilterFormKey, "filter_form_key", viper.GetString("filter_form_key"), "Form key for upload files")
	fs.StringVar(&ac.ImmichURL, "immich_url", viper.GetString("immich_url"), "Immich server URL")
	fs.StringVar(&ac.ImmichAPIKey, "immich_api_key", viper.GetString("immich_api_key"), "Immich API key")
	fs.StringVar(&ac.WatchDir, "watch_dir", viper.GetString("watch_dir"), "Directory to watch for new files")
	fs.StringVar(&ac.UndoneDir, "undone_dir", viper.GetString("undone_dir"), "Directory to copy files that failed")
	fs.StringVar(&ac.ConfigFile, "tasks_file", viper.GetString("tasks_file"), "Path to the configuration file")
	fs.StringVar(&ac.LogLevel, "log_level", viper.GetString("log_level"), "Log level (debug, info, warn, error)")
	fs.IntVar(&ac.MaxConcurrentRequests, "max_concurrent_requests", viper.GetInt("max_concurrent_requests"), "Maximum number of files/uploads to process concurrently")
	fs.IntVar(&ac.HTTPTimeoutSeconds, "http_timeout_seconds", viper.GetInt("http_timeout_seconds"), "HTTP client timeout in seconds")
	fs.IntVar(&ac.InotifyBufferSize, "inotify_buffer_size", viper.GetInt("inotify_buffer_size"), "Inotify event read buffer size in bytes")

	if err := fs.Parse(args); err != nil {
		return nil, false, err
	}

	if ac.ShowVersion {
		return ac, true, nil
	}

	var secretErr error
	ac.ImmichAPIKey, secretErr = secretFromFile(ac.ImmichAPIKey, "IUO_IMMICH_API_KEY_FILE")
	if secretErr != nil {
		return nil, false, secretErr
	}
	ac.WorkerToken, secretErr = secretFromFile(os.Getenv("IUO_WORKER_TOKEN"), "IUO_WORKER_TOKEN_FILE")
	if secretErr != nil {
		return nil, false, secretErr
	}

	if err := validateAppConfig(ac); err != nil {
		return nil, false, err
	}

	return ac, false, nil
}

func parseModes(modeStr string) (runWatcher bool, runProxy bool, err error) {
	rawModes := strings.Split(strings.ToLower(modeStr), ",")
	var recognizedCount int

	for _, m := range rawModes {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		switch m {
		case "watcher":
			runWatcher = true
			recognizedCount++
		case "proxy":
			runProxy = true
			recognizedCount++
		default:
			return false, false, fmt.Errorf("unrecognized operating mode %q (supported modes: watcher, proxy)", m)
		}
	}

	if recognizedCount == 0 {
		return false, false, fmt.Errorf("no valid operating mode specified")
	}

	return runWatcher, runProxy, nil
}

func validateAppConfig(ac *entity.AppConfig) error {
	runWatcher, runProxy, err := parseModes(ac.Mode)
	if err != nil {
		return err
	}
	if ac.OriginalFirst {
		if !runProxy || ac.StateDir == "" || strings.TrimSpace(ac.ImmichAPIKey) == "" || strings.TrimSpace(ac.WorkerToken) == "" || strings.TrimSpace(ac.WorkerBindAddr) == "" {
			return fmt.Errorf("original-first requires proxy mode, state_dir, an Immich API key, a worker token and a worker bind address")
		}
	}
	if ac.QueueMaxSpoolBytes < 0 || ac.QueueMaxJobs < 0 {
		return fmt.Errorf("queue limits cannot be negative")
	}

	if ac.ImmichURL == "" {
		return fmt.Errorf("the -immich_url flag is required")
	}

	if ac.MaxConcurrentRequests <= 0 {
		return fmt.Errorf("max_concurrent_requests must be greater than 0")
	}
	if ac.HTTPTimeoutSeconds <= 0 {
		return fmt.Errorf("http_timeout_seconds must be greater than 0")
	}
	if ac.InotifyBufferSize <= 0 {
		return fmt.Errorf("inotify_buffer_size must be greater than 0")
	}

	parsedURL, urlErr := url.Parse(ac.ImmichURL)
	if urlErr != nil {
		return fmt.Errorf("invalid immich_url format: %w", urlErr)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return fmt.Errorf("immich_url must use http or https scheme")
	}
	if parsedURL.Host == "" {
		return fmt.Errorf("immich_url must include a valid host")
	}

	if runWatcher {
		if ac.ImmichAPIKey == "" {
			return fmt.Errorf("the -immich_api_key flag is required in watcher mode")
		}

		if len(strings.TrimSpace(ac.ImmichAPIKey)) < 10 {
			return fmt.Errorf("immich_api_key appears to be too short")
		}

		if mkdirErr := os.MkdirAll(ac.WatchDir, 0750); mkdirErr != nil {
			return fmt.Errorf("error creating watch directory: %v", mkdirErr)
		}

		if mkdirErr := os.MkdirAll(ac.UndoneDir, 0750); mkdirErr != nil {
			return fmt.Errorf("error creating undone directory: %v", mkdirErr)
		}
	}

	if ac.ConfigFile == "" {
		return fmt.Errorf("the -tasks_file flag is required")
	}

	ac.Tasks, err = loadTasks(ac.ConfigFile)
	if err != nil {
		return fmt.Errorf("error loading config file: %v", err)
	}

	return nil
}

func loadTasks(configFile string) (*entity.Config, error) {
	var c *entity.Config
	viper.SetConfigFile(configFile)

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	if err := viper.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("error unmarshaling config: %w", err)
	}

	for i := range c.Tasks {
		if err := initTask(&c.Tasks[i]); err != nil {
			return nil, fmt.Errorf("error validating config: %w", err)
		}
	}

	return c, nil
}

func initTask(task *entity.Task) error {
	values := map[string]string{
		"src_folder": "/src_folder",
		"dst_folder": "/dst_folder",
		"name":       "name",
		"extension":  "ext",
	}

	for i, ext := range task.Extensions {
		task.Extensions[i] = utils.NormalizeExtension(ext)
	}

	tmpl, err := template.New("command").Parse(task.Command)
	if err != nil {
		return fmt.Errorf("task %s unable to parse command: %v", task.Name, err)
	}

	var cmdLine bytes.Buffer
	if err := tmpl.Execute(&cmdLine, values); err != nil {
		return fmt.Errorf("task %s unable to execute template for command: %v", task.Name, err)
	}

	return nil
}

func run(args []string) error {
	config, isVersion, err := parseConfig(args)
	if err != nil {
		return err
	}
	if isVersion {
		fmt.Println(printVersion())
		return nil
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	baseLogger := log.New(os.Stdout, "", log.Ldate|log.Ltime)
	logger := customlogger.NewWithLevel(baseLogger, config.LogLevel, "")
	logger.Printf("Starting %s (mode: %s)", printVersion(), config.Mode)

	processorAdapter := processor.NewLocalProcessor(logger, filepath.Dir(config.ConfigFile))

	var watcherAdapter port.FileWatcher
	var pipelineService *pipeline.Service
	var proxyServer *proxy.Server
	var workerServer *http.Server
	var queueStore *jobqueue.Store
	var publisherCancel context.CancelFunc
	var publisherWG sync.WaitGroup

	parsedImmichURL, urlErr := url.Parse(config.ImmichURL)
	if urlErr != nil {
		return fmt.Errorf("invalid immich_url: %w", urlErr)
	}

	runWatcher, runProxy, _ := parseModes(config.Mode)

	if runWatcher {
		fsAdapter := filesystem.NewLocalFileSystem(config.WatchDir, config.UndoneDir)
		immichAdapter := immich.NewClient(config.ImmichURL, config.ImmichAPIKey, config.HTTPTimeoutSeconds, logger)

		var err error
		watcherAdapter, err = watcher.NewInotifyWatcher(config.WatchDir, logger, config.InotifyBufferSize)
		if err != nil {
			return fmt.Errorf("error creating file watcher: %w", err)
		}

		pipelineService = pipeline.NewService(
			watcherAdapter,
			processorAdapter,
			immichAdapter,
			fsAdapter,
			logger,
			config.Tasks.Tasks,
			config.MaxConcurrentRequests,
		)

		if err := watcherAdapter.Start(); err != nil {
			return fmt.Errorf("error starting watcher: %w", err)
		}
		pipelineService.Start()
	}

	if runProxy {
		proxyServer = proxy.NewServer(
			parsedImmichURL,
			config.BindAddr,
			config.FilterPath,
			config.FilterFormKey,
			processorAdapter,
			config.Tasks.Tasks,
			logger,
			config.MaxConcurrentRequests,
			config.HTTPTimeoutSeconds,
		)
		if config.StateDir != "" {
			if err := proxyServer.EnableStorageSaver(config.StateDir, config.PresetID, config.MobileView); err != nil {
				return fmt.Errorf("storage saver: %w", err)
			}
		}
		if config.OriginalFirst {
			queueStore, err = jobqueue.Open(filepath.Join(config.StateDir, "queue"), jobqueue.Options{MaxSpoolBytes: config.QueueMaxSpoolBytes, MaxJobs: config.QueueMaxJobs})
			if err != nil {
				return fmt.Errorf("open job queue: %w", err)
			}
			defer func() {
				if publisherCancel != nil {
					publisherCancel()
				}
				publisherWG.Wait()
				_ = queueStore.Close()
			}()
			queue := proxy.OriginalFirstQueue{
				Enqueue:              queueStore.Enqueue,
				FindIntent:           queueStore.FindIntent,
				MarkOriginalUploaded: func(id, asset string) error { _, err := queueStore.MarkOriginalUploaded(id, asset); return err },
			}
			if err := proxyServer.EnableOriginalFirst(queue, config.ImmichAPIKey); err != nil {
				return fmt.Errorf("original-first: %w", err)
			}
			listener, err := net.Listen("tcp", config.WorkerBindAddr)
			if err != nil {
				return fmt.Errorf("worker listener: %w", err)
			}
			workerServer = &http.Server{Addr: config.WorkerBindAddr, Handler: &coordinator.WorkerHandler{Queue: queueStore, Token: config.WorkerToken, Paused: !config.RetireOriginal}, ReadHeaderTimeout: 10 * time.Second}
			go func() {
				if err := workerServer.Serve(listener); err != nil && err != http.ErrServerClosed {
					logger.Printf("Worker server error: %v", err)
				}
			}()
			publisherCtx, cancel := context.WithCancel(context.Background())
			publisherCancel = cancel
			publisher := &coordinator.Publisher{Queue: queueStore, Ledger: proxyServer.StorageSaverStore(), Client: replacement.NewClient(config.ImmichURL, config.ImmichAPIKey, &http.Client{Timeout: time.Duration(config.HTTPTimeoutSeconds) * time.Second}), RetireOriginal: config.RetireOriginal, Logger: log.Default()}
			publisher.ReconcileOriginal = func(ctx context.Context, job jobqueue.Job) (string, error) {
				return proxyServer.ReconcileOriginalIntent(ctx, job, config.ImmichAPIKey)
			}
			publisherWG.Add(1)
			go func() { defer publisherWG.Done(); publisher.Run(publisherCtx) }()
		}
		if err := proxyServer.Start(); err != nil {
			return fmt.Errorf("error starting proxy server: %w", err)
		}
	}

	// Wait for interrupt
	<-sigChan

	logger.Printf("Shutting down gracefully...")
	if publisherCancel != nil {
		publisherCancel()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	done := make(chan struct{})
	go func() {
		publisherWG.Wait()
		if workerServer != nil {
			_ = workerServer.Shutdown(shutdownCtx)
		}
		if watcherAdapter != nil {
			watcherAdapter.Stop()
		}
		if pipelineService != nil {
			pipelineService.Stop()
		}
		if proxyServer != nil {
			_ = proxyServer.Stop(shutdownCtx)
		}
		close(done)
	}()

	select {
	case <-done:
		logger.Printf("Shutdown completed successfully")
	case <-shutdownCtx.Done():
		logger.Printf("Shutdown timeout exceeded, forcing exit")
	}

	return nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func optionalIntEnv(key string) (int64, error) {
	value := os.Getenv(key)
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func secretFromFile(value, envKey string) (string, error) {
	filename := os.Getenv(envKey)
	if filename == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("%s cannot be combined with its direct value", envKey)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", envKey, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func printVersion() string {
	return fmt.Sprintf("immich-optimizer %s, commit %s, built at %s", version, commit, date)
}
