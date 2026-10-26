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
	"github.com/logoscore/logos-golang-module-core/pkg/module"
)

// version is reported at registration; set with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	instance := env("FACTORY_ID", "bob-local")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("hello from BOB, the factory punching bag", "instance", instance, "version", version)
	schema := buildSchema
	err := factory.Run(ctx, factory.Config{
		Config: module.Config{
			AMQPURL: amqpURL(),
			Identity: lifecycle.Identity{
				ModuleName:  "bob",
				Instance:    instance,
				Version:     version,
				Description: "BOB: bare factory that builds dummy minions",
			},
		},
		Build:               &schema,
		Builder:             bob{builtBy: "bob " + instance + " " + version},
		MaxConcurrentBuilds: 4,
	})
	if err != nil {
		log.Fatalf("control plane: %v", err)
	}
	slog.Info("bye")
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
