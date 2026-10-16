// Package config parses flags and environment variables without changing process state.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

type Config struct {
	Vault, Transport, Listen, Token string
	ReadOnly                        bool
	Exclude                         []string
}

func Parse(args []string, getenv func(string) string, output io.Writer) (Config, error) {
	var c Config
	value := func(key, fallback string) string {
		if v := getenv("VAULT_MCP_" + key); v != "" {
			return v
		}
		return fallback
	}
	ro, err := strconv.ParseBool(value("READ_ONLY", "false"))
	if err != nil {
		return c, errors.New("VAULT_MCP_READ_ONLY must be a boolean")
	}
	f := flag.NewFlagSet("vault-mcp", flag.ContinueOnError)
	f.SetOutput(output)
	f.StringVar(&c.Vault, "vault", value("VAULT", ""), "vault root directory (required)")
	f.StringVar(&c.Transport, "transport", value("TRANSPORT", "stdio"), "stdio or http")
	f.StringVar(&c.Listen, "listen", value("LISTEN", "127.0.0.1:8080"), "HTTP listen address")
	f.StringVar(&c.Token, "token", value("TOKEN", ""), "HTTP Bearer token (prefer VAULT_MCP_TOKEN)")
	f.BoolVar(&c.ReadOnly, "read-only", ro, "do not register or execute write tools")
	var excludes string
	f.StringVar(&excludes, "exclude", value("EXCLUDE", ""), "comma-separated vault-relative doublestar globs")
	if err := f.Parse(args); err != nil {
		return c, err
	}
	if f.NArg() != 0 {
		return c, errors.New("unexpected positional arguments")
	}
	for _, p := range strings.Split(excludes, ",") {
		if p = strings.TrimSpace(p); p != "" {
			c.Exclude = append(c.Exclude, p)
		}
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if c.Vault == "" {
		return errors.New("--vault or VAULT_MCP_VAULT is required")
	}
	if c.Transport != "stdio" && c.Transport != "http" {
		return errors.New("transport must be stdio or http")
	}
	for _, p := range c.Exclude {
		if !doublestar.ValidatePattern(p) || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
			return fmt.Errorf("invalid vault-relative exclusion pattern: %q", p)
		}
	}
	if c.Transport == "http" {
		host, port, err := net.SplitHostPort(c.Listen)
		if err != nil {
			return errors.New("listen must have host:port form")
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 {
			return errors.New("invalid listen port")
		}
		if c.Token == "" && host != "127.0.0.1" {
			return errors.New("HTTP without a token must listen on 127.0.0.1")
		}
		if strings.ContainsAny(c.Token, "\r\n\t ") {
			return errors.New("token must not contain whitespace")
		}
	}
	return nil
}
