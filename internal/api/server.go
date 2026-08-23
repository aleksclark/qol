package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/aleksclark/qol/domain"
	qolv1 "github.com/aleksclark/qol/gen/qol/v1"
	"github.com/aleksclark/qol/internal/auth"
	"github.com/aleksclark/qol/internal/eventbus"
	"github.com/aleksclark/qol/internal/media"
	"github.com/aleksclark/qol/internal/wire"
	"github.com/nats-io/nats.go"
	"github.com/quic-go/quic-go/http3"
	webtransport "github.com/quic-go/webtransport-go"
	"google.golang.org/protobuf/proto"
)

const sessionCookie = "qol_session"

type Server struct {
	auth          *auth.Service
	bus           *eventbus.Bus
	decoder       *media.Decoder
	allowedOrigin string
	secureCookie  bool
	transport     *webtransport.Server
}

func New(authentication *auth.Service, bus *eventbus.Bus, decoder *media.Decoder, origin string, secureCookie bool) *Server {
	server := &Server{auth: authentication, bus: bus, decoder: decoder, allowedOrigin: origin, secureCookie: secureCookie}
	http3Server := &http3.Server{TLSConfig: &tls.Config{NextProtos: []string{http3.NextProtoH3}}}
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
		return
	}
	go s.serveSession(session)
}

func (s *Server) serveSession(session *webtransport.Session) {
	defer session.CloseWithError(0, "complete")
	stream, err := session.AcceptStream(session.Context())
	if err != nil {
		return
	}
	reader := bufio.NewReader(stream)
	start := new(qolv1.ClientFrame)
	if err := wire.ReadFrame(reader, start); err != nil || start.GetStartUpload() == nil {
		_ = wire.WriteFrame(stream, serverError("invalid_start", "The first frame must start an upload"))
		return
	}
	sessionID := randomID(start.GetStartUpload().Name, strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := wire.WriteFrame(stream, &qolv1.ServerFrame{Body: &qolv1.ServerFrame_UploadAccepted{UploadAccepted: &qolv1.UploadAccepted{SessionId: sessionID}}}); err != nil {
		return
	}
	input, output, wait, err := s.decoder.Start(session.Context())
	if err != nil {
		_ = wire.WriteFrame(stream, serverError("decoder_failed", err.Error()))
		return
	}
	var writerMu sync.Mutex
	var outputCount uint64
	seen := make(map[string]struct{})
	subscription, err := s.bus.SubscribeOutput(fmt.Sprintf("qol.session.%s.pcm.output", sessionID), func(event domain.Event) error {
		writerMu.Lock()
		defer writerMu.Unlock()
		if _, exists := seen[event.ID]; exists {
			return nil
		}
		seen[event.ID] = struct{}{}
		message, err := wire.EventToProto(event)
		if err != nil {
			return err
		}
		if err := wire.WriteFrame(stream, &qolv1.ServerFrame{Body: &qolv1.ServerFrame_OutputEvent{OutputEvent: message}}); err != nil {
			return err
		}
		outputCount++
		return nil
	})
	if err != nil {
		_ = wire.WriteFrame(stream, serverError("subscription_failed", err.Error()))
		return
	}
	defer subscription.Unsubscribe()
	decodeDone := make(chan error, 1)
	var inputCount uint64
	go func() {
		decodeDone <- s.publishPCM(session.Context(), sessionID, output, &inputCount)
	}()
	for {
		frame := new(qolv1.ClientFrame)
		if err := wire.ReadFrame(reader, frame); err != nil {
			_ = input.Close()
			break
		}
		chunk := frame.GetMp3Chunk()
		if chunk == nil {
			_ = input.Close()
			break
		}
		if len(chunk.Data) > 0 {
			if _, err := input.Write(chunk.Data); err != nil {
				break
			}
		}
		if chunk.EndOfStream {
			_ = input.Close()
			break
		}
	}
	decodeErr := <-decodeDone
	waitErr := wait()
	if decodeErr != nil || waitErr != nil {
		_ = wire.WriteFrame(stream, serverError("decode_failed", "MP3 decoding failed"))
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for outputCount < inputCount && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	writerMu.Lock()
	defer writerMu.Unlock()
	_ = wire.WriteFrame(stream, &qolv1.ServerFrame{Body: &qolv1.ServerFrame_UploadCompleted{UploadCompleted: &qolv1.UploadCompleted{InputEvents: inputCount, OutputEvents: outputCount}}})
}

func (s *Server) publishPCM(ctx context.Context, sessionID string, output io.Reader, count *uint64) error {
	buffer := make([]byte, media.ChunkFrames*2)
	var frames uint64
	for {
		size, err := io.ReadFull(output, buffer)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return err
		}
		if size > 0 {
			frameCount := uint32(size / 2)
			sequence := *count
			event := domain.Event{ID: randomID(sessionID, strconv.FormatUint(sequence, 10)), SessionID: sessionID, Sequence: sequence, Span: domain.Span{Start: domain.MediaTime(frames * 1_000_000_000 / media.SampleRate), End: domain.MediaTime((frames + uint64(frameCount)) * 1_000_000_000 / media.SampleRate)}, Produced: time.Now().UTC(), PCM: domain.PCM{SampleRate: media.SampleRate, Channels: media.Channels, Frames: frameCount, Data: append([]byte(nil), buffer[:size]...)}}
			if err := s.bus.Publish(ctx, fmt.Sprintf("qol.session.%s.pcm.input", sessionID), event); err != nil {
				return err
			}
			frames += uint64(frameCount)
			*count++
		}
		if err != nil {
			return nil
		}
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

func randomID(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

var _ = nats.ErrConnectionClosed
