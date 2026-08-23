package api

import (
	"testing"

	"github.com/quic-go/quic-go/http3"
)

func TestNewConfiguresHTTP3ForWebTransport(t *testing.T) {
	server := New(nil, nil, nil, "http://localhost:9876", true)
	transport := server.Transport()
	if transport.H3.TLSConfig == nil || len(transport.H3.TLSConfig.NextProtos) != 1 || transport.H3.TLSConfig.NextProtos[0] != http3.NextProtoH3 {
		t.Fatalf("HTTP/3 ALPN is not configured: %#v", transport.H3.TLSConfig)
	}
	if !transport.H3.EnableDatagrams {
		t.Fatal("HTTP/3 datagrams are not enabled")
	}
	if transport.H3.AdditionalSettings[0x2b603742] != 1 {
		t.Fatalf("WebTransport setting is not enabled: %#v", transport.H3.AdditionalSettings)
	}
}
