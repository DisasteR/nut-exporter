// Command nut-exporter exports every variable and session metadata of NUT (upsd) servers to
// Prometheus. See docs/design.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DisasteR/nut-exporter/internal/collect"
	"github.com/DisasteR/nut-exporter/internal/config"
	"github.com/DisasteR/nut-exporter/internal/export"
	"github.com/DisasteR/nut-exporter/internal/nut"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "nut-exporter:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		file    = flag.String("config", "", "YAML configuration file (docs/design.md §7)")
		server  = flag.String("nut.server", "", "upsd address host:port, when no configuration file is given")
		listen  = flag.String("web.listen-address", "", "listen address, overrides the configuration")
		level   = flag.String("log.level", "info", "debug, info, warn or error")
		showVer = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return nil
	}
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(*level)); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))

	cfg, err := loadConfig(*file, *server)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cols []export.Collector
	for _, s := range cfg.Servers {
		sc := collect.NewServer(collect.Options{
			Name: s.Name, Address: s.Address, Workers: cfg.Workers, MaxUPS: cfg.Guard.MaxUPS,
			MetadataTTL: cfg.MetadataTTL, UPSFilter: cfg.UPS.Match,
			Client: nut.Options{
				ConnectTimeout: cfg.Timeouts.Connect, CommandTimeout: cfg.Timeouts.Command,
				MaxLine: cfg.Limits.MaxLine, MaxResponse: cfg.Limits.MaxResponse,
			},
		})
		go sc.Run(ctx)
		cols = append(cols, sc)
	}
	exp, err := export.New(cfg, cols, version, log)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", exp.ServeMetrics)
	mux.HandleFunc("GET /inventory", exp.ServeInventory)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "nut-exporter: /metrics /inventory")
	})
	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "address", cfg.Listen, "servers", len(cfg.Servers), "version", version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shut); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func loadConfig(file, server string) (config.Config, error) {
	switch {
	case file != "" && server != "":
		return config.Config{}, errors.New("-config and -nut.server are exclusive")
	case file != "":
		return config.Load(file)
	case server != "":
		c := config.Default()
		c.Servers = []config.Server{{Name: server, Address: server}}
		return c, c.Validate()
	default:
		return config.Config{}, errors.New("give -config or -nut.server")
	}
}
