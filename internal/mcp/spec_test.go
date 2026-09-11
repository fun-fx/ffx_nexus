package mcp

import "testing"

func TestDecodeSpecStdio(t *testing.T) {
	spec, err := DecodeSpec([]byte(`
connection:
  type: stdio
  command: echo
  args: ["hello"]
timeout_ms: 5000
`))
	if err != nil {
		t.Fatal(err)
	}
	if spec.ConnectionType() != "stdio" {
		t.Fatalf("type = %q", spec.ConnectionType())
	}
	if spec.Timeout() != 5000 {
		t.Fatalf("timeout = %d", spec.Timeout())
	}
}

func TestDecodeSpecHTTP(t *testing.T) {
	spec, err := DecodeSpec([]byte(`
connection:
  type: http
  url: http://127.0.0.1:8082/mcp
`))
	if err != nil {
		t.Fatal(err)
	}
	if spec.ConnectionType() != "http" {
		t.Fatalf("type = %q", spec.ConnectionType())
	}
}

func TestDecodeSpecRejectsMissingCommand(t *testing.T) {
	_, err := DecodeSpec([]byte(`connection: { type: stdio }`))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestAllowsVirtualKey(t *testing.T) {
	spec := &ServerSpec{AllowedVirtualKeyIDs: []string{"vk-1"}}
	if !spec.AllowsVirtualKey("vk-1") {
		t.Fatal("expected allow")
	}
	if spec.AllowsVirtualKey("vk-2") {
		t.Fatal("expected deny")
	}
}

func TestAllowsTool(t *testing.T) {
	spec := &ServerSpec{AllowedTools: []string{"read_file"}}
	if !spec.AllowsTool("read_file") {
		t.Fatal("expected allow")
	}
	if spec.AllowsTool("delete_file") {
		t.Fatal("expected deny")
	}
	open := &ServerSpec{}
	if !open.AllowsTool("anything") {
		t.Fatal("empty allowlist permits all")
	}
}
