// Package server implements MCP transports. Every connection shares one vault
// (and thus one write lock), but gets fresh initialization instructions.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/E13Lau/vault-mcp/internal/config"
	"github.com/E13Lau/vault-mcp/internal/tools"
	"github.com/E13Lau/vault-mcp/internal/vault"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Version = "0.1.0-dev"

func New(v *vault.Vault) (*mcp.Server, error) {
	g, err := v.Guide()
	if err != nil {
		return nil, err
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "vault-mcp", Version: Version}, &mcp.ServerOptions{Instructions: g.Instructions()})
	tools.Register(s, v)
	s.AddResource(&mcp.Resource{URI: "vault://guide", Name: "Vault guide", Description: "Full, current vault rules", MIMEType: "text/markdown"},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			g, err := v.Guide()
			if err != nil {
				return nil, err
			}
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "vault://guide", MIMEType: "text/markdown", Text: g.Content}}}, nil
		})
	return s, nil
}

// Handler protects all MCP methods, including GET streams and DELETE sessions.
func Handler(v *vault.Vault, token string) http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		s, err := New(v)
		if err != nil {
			slog.Error("cannot load vault guide", "error", err)
			return nil
		}
		return s
	}, &mcp.StreamableHTTPOptions{JSONResponse: true, SessionTimeout: 30 * time.Minute, MaxRequestBodyBytes: 32 << 20})
	protected := http.NewCrossOriginProtection().Handler(mcpHandler)
	expected := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			scheme, credentials, found := strings.Cut(r.Header.Get("Authorization"), " ")
			provided := sha256.Sum256([]byte(credentials))
			if len(r.Header.Values("Authorization")) != 1 || !found || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

func Run(ctx context.Context, c config.Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	v, err := vault.Open(c.Vault, c.Exclude, c.ReadOnly)
	if err != nil {
		return err
	}
	defer v.Close()
	if err := v.SetIndexFile(c.IndexFile); err != nil {
		return err
	}
	if err := v.Refresh(ctx); err != nil {
		return err
	}
	s, err := New(v)
	if err != nil {
		return err
	}
	if c.Transport == "stdio" {
		return s.Run(ctx, &mcp.StdioTransport{})
	}
	l, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return errors.New("cannot listen on configured address")
	}
	defer l.Close()
	slog.Info("vault-mcp listening", "address", l.Addr().String(), "read_only", c.ReadOnly)
	h := &http.Server{Handler: Handler(v, c.Token), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	serveCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	defer func() { cancel(); <-stopped }()
	go func() {
		defer close(stopped)
		<-serveCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := h.Shutdown(shutdownCtx); err != nil {
			_ = h.Close()
		}
	}()
	err = h.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
