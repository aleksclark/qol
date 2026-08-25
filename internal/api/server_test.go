package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	qolv1 "github.com/aleksclark/qol/gen/qol/v1"
	"github.com/aleksclark/qol/internal/auth"
	"github.com/aleksclark/qol/internal/eventbus"
	"github.com/aleksclark/qol/internal/store/memory"
	"github.com/quic-go/quic-go/http3"
	"google.golang.org/protobuf/proto"
)

func TestNewConfiguresHTTP3ForWebTransport(t *testing.T) {
	server := New(nil, nil, "http://localhost:9876", true)
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
	if transport.H3.QUICConfig == nil || transport.H3.QUICConfig.MaxIdleTimeout != 30*time.Minute || transport.H3.QUICConfig.KeepAlivePeriod != 10*time.Second {
		t.Fatalf("QUIC idle timeout is not configured: %#v", transport.H3.QUICConfig)
	}
	if !transport.H3.QUICConfig.EnableDatagrams || !transport.H3.QUICConfig.EnableStreamResetPartialDelivery {
		t.Fatalf("QUIC WebTransport flags are not set: %#v", transport.H3.QUICConfig)
	}
}

func TestTopicActivityListsEveryGraphSubject(t *testing.T) {
	server, token := authenticatedServer(t)
	server.activity = staticActivity{topics: []eventbus.TopicActivity{
		{Subject: "qol.graph.capture.input", EventCount: 12},
		{Subject: "qol.graph.record-es.completed", EventCount: 3},
		{Subject: "qol.graph.crosstalk.source", EventCount: 7},
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1/topic-activity", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("topic activity returned %d: %s", response.Code, response.Body.String())
	}
	message := new(qolv1.TopicActivityResponse)
	if err := proto.Unmarshal(response.Body.Bytes(), message); err != nil {
		t.Fatal(err)
	}
	if len(message.Topics) != 10 {
		t.Fatalf("got %d topics, want 10", len(message.Topics))
	}
	if message.Topics[0].Subject != "qol.graph.capture.input" || message.Topics[0].EventCount != 12 {
		t.Fatalf("unexpected first topic: %#v", message.Topics[0])
	}
	if message.Topics[1].EventCount != 0 || message.Topics[8].EventCount != 3 {
		t.Fatalf("missing zero or completed counts: %#v", message.Topics)
	}
	last := message.Topics[len(message.Topics)-1]
	if last.Subject != "qol.graph.crosstalk.source" || last.EventCount != 7 {
		t.Fatalf("missing watched crosstalk subject: %#v", last)
	}
	if message.ObservedAtUnixMilli == 0 {
		t.Fatal("observation time is missing")
	}
}

func TestTopicActivityRequiresAuthentication(t *testing.T) {
	server := New(auth.New(memory.New(), time.Hour, time.Minute), nil, "http://localhost:9876", false)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/topic-activity", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestTopicActivityReportsUnavailableNATS(t *testing.T) {
	server, token := authenticatedServer(t)
	server.activity = staticActivity{err: errors.New("nats unavailable")}
	request := httptest.NewRequest(http.MethodPost, "/v1/topic-activity", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

type staticActivity struct {
	topics []eventbus.TopicActivity
	err    error
}

func (s staticActivity) TopicActivity() ([]eventbus.TopicActivity, error) {
	return s.topics, s.err
}

func authenticatedServer(t *testing.T) (*Server, string) {
	t.Helper()
	ctx := context.Background()
	authentication := auth.New(memory.New(), time.Hour, time.Minute)
	if err := authentication.Bootstrap(ctx, "admin", "secret"); err != nil {
		t.Fatal(err)
	}
	token, _, err := authentication.Login(ctx, "admin", "secret")
	if err != nil {
		t.Fatal(err)
	}
	return New(authentication, nil, "http://localhost:9876", false), token
}
