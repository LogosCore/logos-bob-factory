// Command bob-factory is BOB (Body Opponent Bag): a bare factory that
// registers with core, says hello, keeps heartbeating, and builds dummy
// minions (build.go). It is the dummy the factory contracts are worked out
// against.
package main

import (
	"context"
	"log"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/logoscore/logos-golang-factory-core/pkg/factory"
	"github.com/logoscore/logos-golang-module-core/pkg/lifecycle"
	"github.com/logoscore/logos-golang-module-core/pkg/logging"
	"github.com/logoscore/logos-golang-module-core/pkg/logstream"
	"github.com/logoscore/logos-golang-module-core/pkg/module"
)

// version is reported at registration; set with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	instance := env("FACTORY_ID", "bob-local")

	// Logs go to stderr (LOG_FORMAT) at the managed log_level, and to core's
	// live log tail (which can ask for debug on its own) through the hub.
	var level slog.LevelVar
	logs := logstream.New(instance)
	logger, err := logging.New(os.Getenv("LOG_FORMAT"), os.Stderr, &level, logs)
	if err != nil {
		log.Fatalf("logging: %v", err)
	}
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("hello from BOB, the factory punching bag", "instance", instance, "version", version)
	b := &bob{builtBy: "bob " + instance + " " + version}
	mgmtSrv, err := newManagement(b, &level)
	if err != nil {
		fatal("management", "err", err)
	}
	schema := buildSchema
	err = factory.Run(ctx, factory.Config{
		Config: module.Config{
			AMQPURL: amqpURL(),
			Identity: lifecycle.Identity{
				ModuleName:  "bob",
				Instance:    instance,
				Version:     version,
				Description: "BOB: bare factory that builds dummy minions",
				Icon:        icon,
			},
			// factory-core adds the builds section; the live log tail is
			// served on the same rpc_queue.
			Management: mgmtSrv,
			Logs:       logs,
		},
		Build:               &schema,
		Builder:             b,
		MaxConcurrentBuilds: 4,
	})
	if err != nil {
		fatal("control plane failed", "err", err)
	}
	slog.Info("bye")
}

// fatal logs at error level and exits. log.Fatal would go through slog's
// default handler at info level once slog.SetDefault has run.
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

func amqpURL() string {
	u := url.URL{
		Scheme: "amqp",
		User:   url.UserPassword(env("RABBITMQ_USER", "guest"), env("RABBITMQ_PASSWORD", "guest")),
		Host:   net.JoinHostPort(env("RABBITMQ_HOST", "localhost"), env("RABBITMQ_PORT", "5672")),
		// amqp091 takes the vhost from the path minus its leading "/", so the
		// default "/" vhost becomes the path "//".
		Path: "/" + env("RABBITMQ_VHOST", "/"),
	}
	return u.String()
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
