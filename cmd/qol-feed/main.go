package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	qolv1 "github.com/aleksclark/qol/gen/qol/v1"
	"github.com/aleksclark/qol/internal/config"
	"github.com/aleksclark/qol/internal/wire"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	webtransport "github.com/quic-go/webtransport-go"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

const (
	chunkSize          = 64 * 1024
	fallbackByteRate   = 16000
	sessionIdleTimeout = 30 * time.Minute
	sessionKeepAlive   = 10 * time.Second
)

func main() {
	if err := newCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "qol-feed API_URL AUDIO_FILE",
		Short: "Stream an audio file through the Qol WebTransport upload at playback rate",
		Args:  cobra.ExactArgs(2),
		RunE:  run,
	}
	command.Flags().String("username", "admin", "Administrator username")
	command.Flags().String("password", "change-me", "Administrator password")
	command.Flags().String("origin", "http://localhost:9876", "WebTransport origin")
	command.Flags().String("webtransport", "https://localhost:4443", "WebTransport origin URL")
	command.Flags().String("tls-ca", "", "PEM certificate used to trust the local WebTransport server")
	return command
}

func run(command *cobra.Command, args []string) error {
	settings, err := config.Bind(command)
	if err != nil {
		return err
	}
	apiURL := strings.TrimRight(args[0], "/")
	path := args[1]
	client, err := newAPIClient(apiURL)
	if err != nil {
		return err
	}
	if err := client.login(settings.GetString("username"), settings.GetString("password")); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	ticket, err := client.ticket()
	if err != nil {
		return fmt.Errorf("upload ticket: %w", err)
	}
	tlsConfig, err := loadTLS(settings.GetString("tls-ca"))
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	completed, err := streamFile(settings.GetString("webtransport")+"/v1/upload?ticket="+ticket, settings.GetString("origin"), tlsConfig, path)
	if err != nil {
		return fmt.Errorf("webtransport: %w", err)
	}
	fmt.Fprintf(command.OutOrStdout(), "session %s\n", completed.SessionId)
	for _, output := range completed.OutputPaths {
		fmt.Fprintln(command.OutOrStdout(), output)
	}
	if len(completed.OutputPaths) == 0 && completed.OutputPath != "" {
		fmt.Fprintln(command.OutOrStdout(), completed.OutputPath)
	}
	return nil
}

type apiClient struct {
	base   string
	client *http.Client
}

func newAPIClient(base string) (*apiClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &apiClient{base: base, client: &http.Client{Timeout: 30 * time.Second, Jar: jar}}, nil
}

func (c *apiClient) login(username, password string) error {
	response := new(qolv1.LoginResponse)
	if err := c.post("/v1/login", &qolv1.LoginRequest{Username: username, Password: password}, response); err != nil {
		return err
	}
	if response.User == nil {
		return errors.New("login returned no user")
	}
	return nil
}

func (c *apiClient) ticket() (string, error) {
	response := new(qolv1.CreateUploadTicketResponse)
	if err := c.post("/v1/upload-ticket", &qolv1.CreateUploadTicketRequest{}, response); err != nil {
		return "", err
	}
	if response.Ticket == "" {
		return "", errors.New("upload ticket is empty")
	}
	return response.Ticket, nil
}

func (c *apiClient) post(path string, input, output proto.Message) error {
	payload, err := proto.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/protobuf")
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		problem := new(qolv1.Error)
		if proto.Unmarshal(body, problem) == nil && problem.Message != "" {
			return errors.New(problem.Message)
		}
		return fmt.Errorf("request %s failed: %s", path, response.Status)
	}
	return proto.Unmarshal(body, output)
}

func streamFile(url, origin string, tlsConfig *tls.Config, path string) (*qolv1.UploadCompleted, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	mediaType := mime.TypeByExtension(filepath.Ext(path))
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	duration, err := probeDuration(path)
	if err != nil {
		duration = fallbackDuration(info.Size())
		fmt.Fprintf(os.Stderr, "warning: using %s fallback pacing (%v)\n", duration.Round(time.Millisecond), err)
	}
	dialer := &webtransport.Dialer{TLSClientConfig: tlsConfig, QUICConfig: sessionQUICConfig()}
	headers := http.Header{}
	headers.Set("Origin", origin)
	response, session, err := dialer.Dial(context.Background(), url, headers)
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("dial: status %s: %w", response.Status, err)
		}
		return nil, fmt.Errorf("dial: %w", err)
	}
	closeReason := "aborted"
	defer func() { _ = session.CloseWithError(0, closeReason) }()
	stream, err := session.OpenStreamSync(session.Context())
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}
	results := make(chan *qolv1.UploadCompleted, 1)
	errorsCh := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(stream)
		for {
			frame := new(qolv1.ServerFrame)
			if err := wire.ReadFrame(reader, frame); err != nil {
				errorsCh <- fmt.Errorf("read server frame: %w", err)
				return
			}
			if problem := frame.GetError(); problem != nil {
				errorsCh <- fmt.Errorf("server %s: %s", problem.Code, problem.Message)
				return
			}
			if completed := frame.GetUploadCompleted(); completed != nil {
				results <- completed
				return
			}
		}
	}()
	if err := wire.WriteFrame(stream, &qolv1.ClientFrame{Body: &qolv1.ClientFrame_StartUpload{StartUpload: &qolv1.StartUpload{Name: filepath.Base(path), SizeBytes: uint64(info.Size()), MediaType: mediaType}}}); err != nil {
		return nil, fmt.Errorf("start upload: %w", err)
	}
	if err := writeAudioChunks(stream, file, info.Size(), duration); err != nil {
		return nil, fmt.Errorf("write audio: %w", err)
	}
	select {
	case completed := <-results:
		closeReason = "complete"
		return completed, nil
	case err := <-errorsCh:
		return nil, err
	}
}

func writeAudioChunks(writer io.Writer, file io.Reader, size int64, duration time.Duration) error {
	buffer := make([]byte, chunkSize)
	var sequence uint64
	var sent int64
	started := time.Now()
	for {
		n, err := io.ReadFull(file, buffer)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return err
		}
		if n > 0 {
			if err := wire.WriteFrame(writer, &qolv1.ClientFrame{Body: &qolv1.ClientFrame_AudioChunk{AudioChunk: &qolv1.AudioChunk{Sequence: sequence, Data: append([]byte(nil), buffer[:n]...)}}}); err != nil {
				return err
			}
			sequence++
			sent += int64(n)
			if wait := paceDelay(time.Since(started), sent, size, duration); wait > 0 {
				time.Sleep(wait)
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || err == nil && n == 0 {
			return wire.WriteFrame(writer, &qolv1.ClientFrame{Body: &qolv1.ClientFrame_AudioChunk{AudioChunk: &qolv1.AudioChunk{Sequence: sequence, EndOfStream: true}}})
		}
	}
}

func paceDelay(elapsed time.Duration, bytesSent, size int64, duration time.Duration) time.Duration {
	if size <= 0 || duration <= 0 || bytesSent <= 0 {
		return 0
	}
	target := time.Duration(float64(duration) * float64(bytesSent) / float64(size))
	if target <= elapsed {
		return 0
	}
	return target - elapsed
}

func probeDuration(path string) (time.Duration, error) {
	output, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		return 0, err
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil {
		return 0, err
	}
	if seconds <= 0 {
		return 0, fmt.Errorf("non-positive duration %v", seconds)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func fallbackDuration(size int64) time.Duration {
	if size <= 0 {
		return 0
	}
	return time.Duration(size) * time.Second / fallbackByteRate
}

func sessionQUICConfig() *quic.Config {
	return &quic.Config{
		EnableDatagrams:                  true,
		EnableStreamResetPartialDelivery: true,
		MaxIdleTimeout:                   sessionIdleTimeout,
		KeepAlivePeriod:                  sessionKeepAlive,
	}
}

func loadTLS(caPath string) (*tls.Config, error) {
	config := &tls.Config{NextProtos: []string{http3.NextProtoH3}, MinVersion: tls.VersionTLS13}
	if caPath == "" {
		for _, candidate := range []string{".dev-certs/localhost.pem", "dev-certs/localhost.pem"} {
			if _, err := os.Stat(candidate); err == nil {
				caPath = candidate
				break
			}
		}
	}
	if caPath == "" {
		config.InsecureSkipVerify = true
		return config, nil
	}
	pem, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("TLS CA file does not contain a certificate")
	}
	config.RootCAs = pool
	config.ServerName = "localhost"
	return config, nil
}
