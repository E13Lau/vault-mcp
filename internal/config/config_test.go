package config

import (
	"io"
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	env := map[string]string{"VAULT_MCP_VAULT": "/env", "VAULT_MCP_TOKEN": "secret", "VAULT_MCP_READ_ONLY": "true", "VAULT_MCP_EXCLUDE": "raw/assets/**, private/**", "VAULT_MCP_INDEX_FILE": "custom/index.md"}
	c, err := Parse([]string{"--vault", "/flag", "--read-only=false", "--transport=http", "--listen=0.0.0.0:8080"}, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Vault != "/flag" || c.ReadOnly || c.Token != "secret" || c.IndexFile != "custom/index.md" || !reflect.DeepEqual(c.Exclude, []string{"raw/assets/**", "private/**"}) {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		ok   bool
	}{
		{"missing vault", nil, false},
		{"stdio", []string{"--vault=here"}, true},
		{"unknown transport", []string{"--vault=here", "--transport=sse"}, false},
		{"local no token", []string{"--vault=here", "--transport=http"}, true},
		{"wildcard no token", []string{"--vault=here", "--transport=http", "--listen=:8080"}, false},
		{"public no token", []string{"--vault=here", "--transport=http", "--listen=0.0.0.0:8080"}, false},
		{"hostname no token", []string{"--vault=here", "--transport=http", "--listen=localhost:8080"}, false},
		{"public token", []string{"--vault=here", "--transport=http", "--listen=0.0.0.0:8080", "--token=secret"}, true},
		{"invalid port", []string{"--vault=here", "--transport=http", "--listen=127.0.0.1:65536"}, false},
		{"bad glob", []string{"--vault=here", "--exclude=["}, false},
		{"extra arg", []string{"--vault=here", "unexpected"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.args, func(string) string { return "" }, io.Discard)
			if (err == nil) != tc.ok {
				t.Fatalf("error = %v, want success %v", err, tc.ok)
			}
		})
	}
	_, err := Parse(nil, func(string) string { return "not-a-boolean" }, io.Discard)
	if err == nil {
		t.Fatal("accepted invalid environment boolean")
	}
}
