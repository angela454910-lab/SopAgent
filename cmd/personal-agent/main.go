// Command personal-agent runs LoopForge's generic, embedded Agent service.
// Business data, model credentials and tools are supplied by a trusted gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"agentflow/internal/application/personalagent"
	"agentflow/internal/httpapi/personalagentapi"
	"agentflow/internal/repo"
)

func main() {
	if err := run(); err != nil {
		log.Printf("personal-agent: %v", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", "", "HTTP listen address; overrides SOP_AGENT_ADDR")
	envFile := flag.String("env-file", "local.env", "local environment file (exported variables take precedence)")
	flag.Parse()
	explicitEnvFile := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "env-file" {
			explicitEnvFile = true
		}
	})
	if err := loadLocalEnv(*envFile, explicitEnvFile); err != nil {
		return err
	}
	listenAddr := *addr
	if listenAddr == "" {
		listenAddr = envOr("SOP_AGENT_ADDR", "127.0.0.1:8091")
	}
	serviceToken := os.Getenv("SOP_AGENT_SERVICE_TOKEN")
	gatewayToken := os.Getenv("SOP_AGENT_GATEWAY_TOKEN")
	if strings.TrimSpace(serviceToken) == "" || strings.TrimSpace(gatewayToken) == "" {
		return errors.New("SOP_AGENT_SERVICE_TOKEN and SOP_AGENT_GATEWAY_TOKEN are required")
	}
	gateway, err := personalagent.NewHTTPGateway(envOr("SOP_AGENT_GATEWAY_URL", "http://127.0.0.1:3009/internal/sop"), gatewayToken)
	if err != nil {
		return fmt.Errorf("configure gateway: %w", err)
	}
	store, err := repo.Open(os.Getenv("AGENTFLOW_MYSQL_DSN"))
	if err != nil {
		// Connection errors may contain a database host or username. Avoid
		// printing DSNs or the database driver's unfiltered connection error.
		return errors.New("cannot open MySQL; check AGENTFLOW_MYSQL_DSN and database availability")
	}
	defer store.Close()
	service := personalagent.NewService(store, gateway)
	defer service.Close()
	handler, err := personalagentapi.NewPersonalAgentRouter(service, serviceToken)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	// The MVP uses one headless service instance per database. A new process
	// records interrupted work as failed/unknown; it never replays writes.
	// Bind first so a duplicate process on this address cannot recover tasks
	// that the existing process is still executing.
	recoveryCtx, stopRecovery := context.WithTimeout(context.Background(), 15*time.Second)
	err = service.RecoverInterrupted(recoveryCtx)
	stopRecovery()
	if err != nil {
		return errors.New("cannot reconcile interrupted tasks; check database availability")
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	log.Printf("personal-agent listening on %s", listener.Addr())
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("HTTP shutdown: %w", err)
		}
		return nil
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
