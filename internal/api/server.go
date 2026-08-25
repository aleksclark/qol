package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	qol "github.com/aleksclark/qol"
	qolv1 "github.com/aleksclark/qol/gen/qol/v1"
	"github.com/aleksclark/qol/internal/auth"
	"github.com/aleksclark/qol/internal/eventbus"
	"github.com/aleksclark/qol/internal/wire"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	webtransport "github.com/quic-go/webtransport-go"
	"google.golang.org/protobuf/proto"
)

const sessionCookie = "qol_session"

type topicActivitySource interface {
	TopicActivity() ([]eventbus.TopicActivity, error)
}

type Server struct {
	auth          *auth.Service
	bus           *eventbus.Bus
	activity      topicActivitySource
	allowedOrigin string
	secureCookie  bool
	transport     *webtransport.Server
}

func New(authentication *auth.Service, bus *eventbus.Bus, origin string, secureCookie bool) *Server {
	server := &Server{auth: authentication, bus: bus, activity: bus, allowedOrigin: origin, secureCookie: secureCookie}
	http3Server := &http3.Server{
		TLSConfig: &tls.Config{NextProtos: []string{http3.NextProtoH3}},
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			MaxIdleTimeout:                   30 * time.Minute,
			KeepAlivePeriod:                  10 * time.Second,
		},
	}
	webtransport.ConfigureHTTP3Server(http3Server)
	server.transport = &webtransport.Server{H3: http3Server, CheckOrigin: func(request *http.Request) bool { return request.Header.Get("Origin") == origin }}
	return server
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /v1/login", s.login)
	mux.HandleFunc("POST /v1/logout", s.logout)
	mux.HandleFunc("POST /v1/current-user", s.currentUser)
	mux.HandleFunc("POST /v1/topic-activity", s.topicActivity)
	mux.HandleFunc("POST /v1/upload-ticket", s.uploadTicket)
	mux.HandleFunc("CONNECT /v1/upload", s.upload)
	return s.cors(mux)
}

func (s *Server) Transport() *webtransport.Server {
	s.transport.H3.Handler = s.Handler()
	return s.transport
}

func (s *Server) login(writer http.ResponseWriter, request *http.Request) {
	input := new(qolv1.LoginRequest)
	if !readProto(writer, request, input) {
		return
	}
	token, user, err := s.auth.Login(request.Context(), input.Username, input.Password)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "invalid_credentials", "Invalid username or password")
		return
	}
	http.SetCookie(writer, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.secureCookie, SameSite: http.SameSiteStrictMode})
	writeProto(writer, http.StatusOK, &qolv1.LoginResponse{User: user})
}

func (s *Server) logout(writer http.ResponseWriter, request *http.Request) {
	if cookie, err := request.Cookie(sessionCookie); err == nil {
		_ = s.auth.Logout(request.Context(), cookie.Value)
	}
	http.SetCookie(writer, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secureCookie, SameSite: http.SameSiteStrictMode})
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) currentUser(writer http.ResponseWriter, request *http.Request) {
	user, ok := s.authorize(writer, request)
	if !ok {
		return
	}
	writeProto(writer, http.StatusOK, &qolv1.CurrentUserResponse{User: user})
}

func (s *Server) topicActivity(writer http.ResponseWriter, request *http.Request) {
	if _, ok := s.authorize(writer, request); !ok {
		return
	}
	input := new(qolv1.TopicActivityRequest)
	if !readProto(writer, request, input) {
		return
	}
	activity, err := s.activity.TopicActivity()
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "activity_unavailable", "Could not inspect graph activity")
		return
	}
	counts := make(map[string]uint64, len(activity))
	for _, topic := range activity {
		counts[topic.Subject] = topic.EventCount
	}
	response := &qolv1.TopicActivityResponse{ObservedAtUnixMilli: time.Now().UnixMilli()}
	for _, subject := range activitySubjects(counts) {
		response.Topics = append(response.Topics, &qolv1.TopicActivity{Subject: subject, EventCount: counts[subject]})
	}
	writeProto(writer, http.StatusOK, response)
}

func (s *Server) uploadTicket(writer http.ResponseWriter, request *http.Request) {
	user, ok := s.authorize(writer, request)
	if !ok {
		return
	}
	ticket, expires, err := s.auth.CreateTicket(request.Context(), user.Username)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "ticket_failed", "Could not create an upload ticket")
		return
	}
	writeProto(writer, http.StatusOK, &qolv1.CreateUploadTicketResponse{Ticket: ticket, ExpiresAtUnix: expires.Unix()})
}

func (s *Server) upload(writer http.ResponseWriter, request *http.Request) {
	if _, err := s.auth.ConsumeTicket(request.Context(), request.URL.Query().Get("ticket")); err != nil {
		writeError(writer, http.StatusUnauthorized, "invalid_ticket", "Upload ticket is invalid or expired")
		return
	}
	session, err := s.transport.Upgrade(writer, request)
	if err != nil {
		slog.Error("webtransport upgrade failed", "err", err)
		return
	}
	s.serveSession(session)
}

func (s *Server) serveSession(session *webtransport.Session) {
	closeMsg := "session_ended"
	defer func() {
		if closeMsg != "" {
			_ = session.CloseWithError(0, closeMsg)
		}
	}()
	stream, err := session.AcceptStream(session.Context())
	if err != nil {
		closeMsg = "accept_failed"
		slog.Error("webtransport accept stream failed", "err", err)
		return
	}
	reader := bufio.NewReader(stream)
	frame := new(qolv1.ClientFrame)
	if err := wire.ReadFrame(reader, frame); err != nil || frame.GetStartUpload() == nil {
		closeMsg = "invalid_start"
		slog.Error("webtransport start frame invalid", "err", err)
		_ = wire.WriteFrame(stream, serverError("invalid_start", "The first frame must start an upload"))
		return
	}
	start := frame.GetStartUpload()
	if start.MediaType == "" {
		start.MediaType = "audio/mpeg"
	}
	sessionID := randomID(start.Name, strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := wire.WriteFrame(stream, &qolv1.ServerFrame{Body: &qolv1.ServerFrame_UploadAccepted{UploadAccepted: &qolv1.UploadAccepted{SessionId: sessionID}}}); err != nil {
		closeMsg = "accept_write_failed"
		slog.Error("webtransport accept write failed", "err", err, "session", sessionID)
		return
	}
	completed := make(chan qol.Event, 2)
	var subscriptions []qol.Subscription
	for _, channel := range []string{qol.ChannelRecordENCompleted, qol.ChannelRecordESCompleted} {
		subscription, err := s.bus.Subscribe(session.Context(), channel, qol.SubscribeOptions{}, func(_ context.Context, event qol.Event) error {
			if event.SessionID == sessionID {
				select {
				case completed <- event:
				default:
				}
			}
			return nil
		})
		if err != nil {
			closeMsg = "subscription_failed"
			_ = wire.WriteFrame(stream, serverError("subscription_failed", err.Error()))
			return
		}
		subscriptions = append(subscriptions, subscription)
	}
	defer func() {
		for _, subscription := range subscriptions {
			_ = subscription.Close()
		}
	}()
	var expected uint64
	for {
		frame = new(qolv1.ClientFrame)
		if err := wire.ReadFrame(reader, frame); err != nil {
			closeMsg = "upload_failed"
			slog.Error("webtransport audio ended early", "err", err, "session", sessionID, "expected", expected)
			_ = wire.WriteFrame(stream, serverError("upload_failed", "Audio stream ended before completion"))
			return
		}
		chunk := frame.GetAudioChunk()
		if chunk == nil || chunk.Sequence != expected {
			closeMsg = "invalid_sequence"
			_ = wire.WriteFrame(stream, serverError("invalid_sequence", fmt.Sprintf("Expected audio chunk %d", expected)))
			return
		}
		payload, err := wire.MarshalAudioStream(start.MediaType, start.Name, chunk.EndOfStream, chunk.Data)
		if err != nil {
			closeMsg = "invalid_audio"
			_ = wire.WriteFrame(stream, serverError("invalid_audio", err.Error()))
			return
		}
		event := qol.Event{ID: randomID(sessionID, strconv.FormatUint(chunk.Sequence, 10)), SessionID: sessionID, Channel: qol.ChannelCaptureInput, Type: qol.TypeAudioStream, Seq: chunk.Sequence, ProducedAt: time.Now().UTC(), Encoding: qol.EncodingProtobuf, Payload: payload}
		if err := s.bus.Publish(session.Context(), event); err != nil {
			closeMsg = "publish_failed"
			_ = wire.WriteFrame(stream, serverError("publish_failed", err.Error()))
			return
		}
		expected++
		if chunk.EndOfStream {
			break
		}
	}
	paths := make(map[string]string, 2)
	deadline := time.After(30 * time.Minute)
	for len(paths) < 2 {
		select {
		case event := <-completed:
			stored, err := wire.UnmarshalStoredAudio(event.Payload)
			if err != nil {
				closeMsg = "invalid_completion"
				_ = wire.WriteFrame(stream, serverError("invalid_completion", err.Error()))
				return
			}
			paths[event.Channel] = stored.Path
		case <-deadline:
			closeMsg = "graph_timeout"
			_ = wire.WriteFrame(stream, serverError("graph_timeout", "The graph did not complete in time"))
			return
		case <-session.Context().Done():
			closeMsg = "session_canceled"
			slog.Error("webtransport session canceled while waiting", "session", sessionID, "have", len(paths))
			return
		}
	}
	outputPaths := []string{paths[qol.ChannelRecordENCompleted], paths[qol.ChannelRecordESCompleted]}
	if err := wire.WriteFrame(stream, &qolv1.ServerFrame{Body: &qolv1.ServerFrame_UploadCompleted{UploadCompleted: &qolv1.UploadCompleted{SessionId: sessionID, Status: "complete", OutputPath: outputPaths[1], OutputPaths: outputPaths}}}); err != nil {
		closeMsg = "complete_write_failed"
		slog.Error("webtransport completion write failed", "err", err, "session", sessionID)
		return
	}
	closeMsg = ""
	select {
	case <-session.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

func (s *Server) authorize(writer http.ResponseWriter, request *http.Request) (*qolv1.User, bool) {
	cookie, err := request.Cookie(sessionCookie)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "Log in to continue")
		return nil, false
	}
	user, err := s.auth.Current(request.Context(), cookie.Value)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "Log in to continue")
		return nil, false
	}
	return user, true
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Origin") == s.allowedOrigin {
			writer.Header().Set("Access-Control-Allow-Origin", s.allowedOrigin)
			writer.Header().Set("Access-Control-Allow-Credentials", "true")
			writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if request.Method == http.MethodOptions {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func readProto(writer http.ResponseWriter, request *http.Request, message proto.Message) bool {
	payload, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, wire.MaxFrameSize))
	if err != nil || proto.Unmarshal(payload, message) != nil {
		writeError(writer, http.StatusBadRequest, "invalid_protobuf", "Request body must be valid protobuf")
		return false
	}
	return true
}

func writeProto(writer http.ResponseWriter, status int, message proto.Message) {
	payload, err := proto.Marshal(message)
	if err != nil {
		http.Error(writer, "protobuf encoding failed", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/protobuf")
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeProto(writer, status, &qolv1.Error{Code: code, Message: message})
}

func serverError(code, message string) *qolv1.ServerFrame {
	return &qolv1.ServerFrame{Body: &qolv1.ServerFrame_Error{Error: &qolv1.Error{Code: code, Message: message}}}
}

func graphChannels() []string {
	return []string{
		qol.ChannelCaptureInput,
		qol.ChannelCaptureOutput,
		qol.ChannelAudioInput,
		qol.ChannelRecordENInput,
		qol.ChannelASRText,
		qol.ChannelTranslationText,
		qol.ChannelTTSAudio,
		qol.ChannelRecordENCompleted,
		qol.ChannelRecordESCompleted,
	}
}

func activitySubjects(counts map[string]uint64) []string {
	seen := make(map[string]struct{}, len(counts)+len(graphChannels()))
	subjects := make([]string, 0, len(counts)+len(graphChannels()))
	for _, channel := range graphChannels() {
		subject := "qol." + channel
		seen[subject] = struct{}{}
		subjects = append(subjects, subject)
	}
	var extra []string
	for subject := range counts {
		if _, ok := seen[subject]; ok {
			continue
		}
		if !strings.HasPrefix(subject, "qol.graph.") {
			continue
		}
		extra = append(extra, subject)
	}
	sort.Strings(extra)
	return append(subjects, extra...)
}

func randomID(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
