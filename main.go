package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	os.Exit(run())
}

func run() int {
	syscall.Umask(0o077)
	configFlag := flag.String("config", "", "path to config.json (defaults to the executable directory)")
	checkFlag := flag.Bool("check", false, "validate configuration and database, then exit")
	flag.Parse()
	configPath, err := defaultConfigPath(*configFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logger, logCloser, err := newErrorLogger(cfg.LogPath, cfg.Logging.MaxSizeMB)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer logCloser.Close()
	app, err := newApp(cfg, logger)
	if err != nil {
		logger.Printf("initialize application: %v", err)
		fmt.Fprintln(os.Stderr, "EasyConnect failed to initialize; see error.log")
		return 1
	}
	if *checkFlag {
		if err := app.store.integrityCheck(); err != nil {
			logger.Printf("health check: %v", err)
			_ = app.store.Close()
			return 1
		}
		_ = app.store.Close()
		fmt.Println("configuration and database check passed")
		return 0
	}
	address := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	app.server = &http.Server{
		Addr:              address,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          log.New(logger.Writer(), "http: ", 0),
	}
	serverErr := make(chan error, 1)
	go func() { serverErr <- app.server.ListenAndServe() }()
	fmt.Printf("EasyConnect running on http://%s\n", address)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-signals:
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			logger.Printf("HTTP server: %v", err)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = app.close(ctx)
			return 1
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.close(ctx); err != nil {
		logger.Printf("shutdown: %v", err)
		return 1
	}
	return 0
}

func defaultConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if environment := os.Getenv("EASYCONNECT_CONFIG"); environment != "" {
		return environment, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve executable symlink: %w", err)
	}
	return filepath.Join(filepath.Dir(executable), "config.json"), nil
}
