package e2e_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/cmplx"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	qol "github.com/aleksclark/qol"
	"github.com/aleksclark/qol/internal/eventbus"
	"github.com/aleksclark/qol/internal/wire"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

const (
	sourceToneHz = 440.0
	sinkToneHz   = 880.0
	sourceRate   = 16000
	sinkRate     = 22050
)

type crosstalkAPI struct {
	base     string
	admin    string
	user     string
	password string
	client   *http.Client
}

type abcRecord struct {
	ID    string
	Token string
}

type sessionRecord struct {
	ID             string `json:"id"`
	BroadcastToken string `json:"broadcast_token"`
}

type channelRecord struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func requireCrosstalkE2E(t *testing.T) *crosstalkAPI {
	t.Helper()
	if os.Getenv("QOL_CROSSTALK_E2E") == "" {
		t.Skip("set QOL_CROSSTALK_E2E=1 to run compiled Crosstalk proofs")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Fatal("ffmpeg is required for Phase 5 audio assertions")
	}
	base := strings.TrimRight(envOr("QOL_CROSSTALK_URL", "http://127.0.0.1:28080"), "/")
	api := &crosstalkAPI{
		base:     base,
		user:     envOr("QOL_CROSSTALK_ADMIN_USER", "admin"),
		password: envOr("QOL_CROSSTALK_ADMIN_PASSWORD", "admin"),
		client:   &http.Client{Timeout: 10 * time.Second},
	}
	token, err := api.login()
	if err != nil {
		t.Fatalf("crosstalk login at %s failed: %v", base, err)
	}
	api.admin = token
	return api
}

func (a *crosstalkAPI) login() (string, error) {
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := a.do(http.MethodPost, "/api/auth/login", "", map[string]string{
		"username": a.user,
		"password": a.password,
	}, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("empty access token")
	}
	return out.AccessToken, nil
}

func (a *crosstalkAPI) createSession(t *testing.T, name string) sessionRecord {
	t.Helper()
	var out sessionRecord
	if err := a.do(http.MethodPost, "/api/sessions", a.admin, map[string]string{"name": name}, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID == "" {
		t.Fatal("empty session id")
	}
	return out
}

func (a *crosstalkAPI) createChannel(t *testing.T, sessionID, name, kind string) channelRecord {
	t.Helper()
	var out channelRecord
	if err := a.do(http.MethodPost, "/api/sessions/"+sessionID+"/channels", a.admin, map[string]string{
		"name": name,
		"type": kind,
	}, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID == "" || out.Name == "" {
		t.Fatalf("invalid channel %#v", out)
	}
	return out
}

func (a *crosstalkAPI) createABC(t *testing.T, name string) abcRecord {
	t.Helper()
	var out struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := a.do(http.MethodPost, "/api/abcs", a.admin, map[string]string{"name": name}, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID == "" || out.Token == "" {
		t.Fatal("abc token was not returned")
	}
	return abcRecord{ID: out.ID, Token: out.Token}
}

func (a *crosstalkAPI) assignABC(t *testing.T, abc abcRecord, name, sessionID string, monitor *string) {
	t.Helper()
	body := map[string]any{"name": name, "session_id": sessionID}
	if monitor != nil {
		body["monitor_channel_id"] = *monitor
	}
	if err := a.do(http.MethodPut, "/api/abcs/"+abc.ID, a.admin, body, nil); err != nil {
		t.Fatal(err)
	}
}

func (a *crosstalkAPI) issueTicket(t *testing.T, sessionID string, produce, listen []string, role string) string {
	t.Helper()
	payload := map[string]any{"session_id": sessionID, "role": role}
	if produce != nil {
		payload["produce"] = produce
	}
	if listen != nil {
		payload["listen"] = listen
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := a.do(http.MethodPost, "/api/webrtc/token", a.admin, payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.Token == "" {
		t.Fatal("empty media ticket")
	}
	return out.Token
}

func (a *crosstalkAPI) do(method, path, bearer string, body any, dest any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, a.base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, truncate(string(data), 300))
	}
	if dest == nil {
		return nil
	}
	return json.Unmarshal(data, dest)
}

func startInProcessNATS(t *testing.T) (string, *eventbus.Bus, func()) {
	t.Helper()
	opts := &natsserver.Options{Port: -1, NoLog: true, NoSigs: true}
	server, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go server.Start()
	if !server.ReadyForConnections(5 * time.Second) {
		t.Fatal("in-process NATS did not start")
	}
	conn, err := nats.Connect(server.ClientURL())
	if err != nil {
		server.Shutdown()
		t.Fatal(err)
	}
	bus, err := eventbus.New(conn)
	if err != nil {
		conn.Close()
		server.Shutdown()
		t.Fatal(err)
	}
	return server.ClientURL(), bus, func() {
		conn.Close()
		server.Shutdown()
	}
}

func buildQolWorker(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "qol-worker")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/qol-worker")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build qol-worker: %v\n%s", err, out)
	}
	return bin
}

func writeTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crosstalk.token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type workerProc struct {
	cmd        *exec.Cmd
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	healthFile string
	token      string
}

func startCrosstalkWorker(t *testing.T, bin, natsURL, serverURL, tokenFile, healthFile string, args ...string) *workerProc {
	t.Helper()
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	cmdArgs := append([]string{
		"run",
		"--type", "crosstalk",
		"--nats-url", natsURL,
		"--health-file", healthFile,
		"--crosstalk-url", serverURL,
		"--crosstalk-token-file", tokenFile,
		"--crosstalk-disable-mdns",
		"--crosstalk-disable-stun",
		"--crosstalk-client-name", "qol-e2e",
	}, args...)
	cmd := exec.Command(bin, cmdArgs...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	proc := &workerProc{cmd: cmd, stdout: stdout, stderr: stderr, healthFile: healthFile, token: strings.TrimSpace(string(token))}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt)
			done := make(chan struct{})
			go func() {
				_ = cmd.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
	})
	return proc
}

func (p *workerProc) waitReady(t *testing.T, wait time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(wait)
	var last string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(p.healthFile)
		if err == nil {
			last = string(data)
			var payload map[string]any
			if json.Unmarshal(data, &payload) == nil {
				if ready, _ := payload["ready"].(bool); ready {
					return payload
				}
			}
		}
		if p.cmd.ProcessState != nil && p.cmd.ProcessState.Exited() {
			t.Fatalf("worker exited early: stdout=%s stderr=%s", p.stdout.String(), p.stderr.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("worker not ready after %s health=%s stdout=%s stderr=%s", wait, last, p.stdout.String(), p.stderr.String())
	return nil
}

func (p *workerProc) assertTokenAbsent(t *testing.T) {
	t.Helper()
	if p.token == "" {
		t.Fatal("empty token")
	}
	blobs := []string{p.stdout.String(), p.stderr.String()}
	if data, err := os.ReadFile(p.healthFile); err == nil {
		blobs = append(blobs, string(data))
	}
	for _, blob := range blobs {
		if strings.Contains(blob, p.token) {
			t.Fatal("ABC token leaked into worker output or health file")
		}
	}
}

type pcmCollector struct {
	channel string
	mu      sync.Mutex
	pcm     []byte
	events  []qol.Event
}

func startPCMCollector(t *testing.T, bus *eventbus.Bus, channel string) *pcmCollector {
	t.Helper()
	collector := &pcmCollector{channel: channel}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub, err := bus.Subscribe(ctx, channel, qol.SubscribeOptions{}, func(_ context.Context, event qol.Event) error {
		audio, err := wire.UnmarshalAudio(event.Payload)
		if err != nil {
			return nil
		}
		collector.mu.Lock()
		collector.pcm = append(collector.pcm, audio.Data...)
		collector.events = append(collector.events, event)
		collector.mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	return collector
}

func (c *pcmCollector) wait(minBytes int, wait time.Duration) ([]byte, error) {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := append([]byte(nil), c.pcm...)
		c.mu.Unlock()
		if len(got) >= minBytes && pcmEnergy(got) > 0 {
			return got, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.pcm...), fmt.Errorf("collected %d pcm bytes on %s from %d events, want >= %d with energy", len(c.pcm), c.channel, len(c.events), minBytes)
}

func collectPCM(t *testing.T, bus *eventbus.Bus, channel string, minBytes int, wait time.Duration) []byte {
	t.Helper()
	pcm, err := startPCMCollector(t, bus, channel).wait(minBytes, wait)
	if err != nil {
		t.Fatal(err)
	}
	return pcm
}

type streamCollector struct {
	channel string
	mu      sync.Mutex
	ogg     []byte
	events  []qol.Event
}

func startStreamCollector(t *testing.T, bus *eventbus.Bus, channel string) *streamCollector {
	t.Helper()
	collector := &streamCollector{channel: channel}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub, err := bus.Subscribe(ctx, channel, qol.SubscribeOptions{}, func(_ context.Context, event qol.Event) error {
		if event.Type != qol.TypeAudioStream {
			return nil
		}
		_, _, _, data, err := wire.UnmarshalAudioStream(event.Payload)
		if err != nil {
			return nil
		}
		collector.mu.Lock()
		collector.ogg = append(collector.ogg, data...)
		collector.events = append(collector.events, event)
		collector.mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	return collector
}

func completeOggPages(ogg []byte) []byte {
	offset := 0
	for offset+27 <= len(ogg) {
		if string(ogg[offset:offset+4]) != "OggS" {
			offset++
			continue
		}
		nseg := int(ogg[offset+26])
		header := 27 + nseg
		if offset+header > len(ogg) {
			break
		}
		body := 0
		for _, size := range ogg[offset+27 : offset+header] {
			body += int(size)
		}
		if offset+header+body > len(ogg) {
			break
		}
		offset += header + body
	}
	return ogg[:offset]
}

func (c *streamCollector) wait(minBytes int, wait time.Duration) ([]byte, error) {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := completeOggPages(append([]byte(nil), c.ogg...))
		c.mu.Unlock()
		if len(got) >= minBytes && len(got) >= 4 && string(got[:4]) == "OggS" {
			return got, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	got := completeOggPages(append([]byte(nil), c.ogg...))
	return got, fmt.Errorf("collected %d complete ogg bytes on %s from %d events, want >= %d", len(got), c.channel, len(c.events), minBytes)
}

func decodeOggToPCM(t *testing.T, ogg []byte, rate int) []byte {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", "pipe:0", "-f", "s16le", "-ac", "1", "-ar", strconv.Itoa(rate), "pipe:1")
	cmd.Stdin = bytes.NewReader(ogg)
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		if exit, ok := err.(*exec.ExitError); ok {
			msg = string(exit.Stderr)
		}
		t.Fatalf("ffmpeg decode ogg: %v\n%s", err, msg)
	}
	return out
}

func encodedTone(t *testing.T, freq float64, duration time.Duration) []byte {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%.0f:duration=%.3f", freq, duration.Seconds()),
		"-ac", "1", "-c:a", "libopus", "-page_duration", "20000", "-f", "ogg", "pipe:1")
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		if exit, ok := err.(*exec.ExitError); ok {
			msg = string(exit.Stderr)
		}
		t.Fatalf("ffmpeg encode ogg: %v\n%s", err, msg)
	}
	if len(out) < 4 || string(out[:4]) != "OggS" {
		t.Fatalf("expected ogg pages, got %d bytes", len(out))
	}
	return out
}

func publishEncodedTone(t *testing.T, bus *eventbus.Bus, channel, sessionID string, freq float64, duration time.Duration) {
	t.Helper()
	ogg := encodedTone(t, freq, duration)
	const chunk = 2048
	var seq uint64
	for offset := 0; offset < len(ogg); offset += chunk {
		end := offset + chunk
		if end > len(ogg) {
			end = len(ogg)
		}
		final := end == len(ogg)
		payload, err := wire.MarshalAudioStream("audio/ogg; codecs=opus", "capture", final, ogg[offset:end])
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.Publish(context.Background(), qol.Event{
			ID:         fmt.Sprintf("%s-%d", sessionID, seq),
			SessionID:  sessionID,
			Channel:    channel,
			Type:       qol.TypeAudioStream,
			Seq:        seq,
			Span:       qol.Span{Start: qol.MediaTime(time.Duration(seq) * 20 * time.Millisecond), End: qol.MediaTime(time.Duration(seq+1) * 20 * time.Millisecond)},
			ProducedAt: time.Now().UTC(),
			Encoding:   qol.EncodingProtobuf,
			Payload:    payload,
		}); err != nil {
			t.Fatal(err)
		}
		seq++
		time.Sleep(20 * time.Millisecond)
	}
}

func publishTone(t *testing.T, bus *eventbus.Bus, channel, sessionID string, freq float64, rate int, duration time.Duration) {
	t.Helper()
	pcm := tonePCM(freq, rate, 1, duration)
	const frame = 20 * time.Millisecond
	samplesPerFrame := rate * int(frame) / int(time.Second)
	bytesPerFrame := samplesPerFrame * 2
	var seq uint64
	for offset := 0; offset < len(pcm); offset += bytesPerFrame {
		end := offset + bytesPerFrame
		if end > len(pcm) {
			end = len(pcm)
		}
		chunk := pcm[offset:end]
		final := end == len(pcm)
		payload, err := wire.MarshalAudio(qol.Audio{
			Format: qol.PCMFormat{SampleRate: rate, Channels: 1, Format: qol.SampleS16LE},
			Data:   chunk,
		})
		if err != nil {
			t.Fatal(err)
		}
		eventType := qol.TypeAudioPCM
		if final {
			eventType = qol.TypeAudioPCMFinal
		}
		if err := bus.Publish(context.Background(), qol.Event{
			ID:         fmt.Sprintf("%s-%d", sessionID, seq),
			SessionID:  sessionID,
			Channel:    channel,
			Type:       eventType,
			Seq:        seq,
			Span:       qol.Span{Start: qol.MediaTime(time.Duration(seq) * frame), End: qol.MediaTime(time.Duration(seq+1) * frame)},
			ProducedAt: time.Now().UTC(),
			Encoding:   qol.EncodingProtobuf,
			Payload:    payload,
		}); err != nil {
			t.Fatal(err)
		}
		seq++
		time.Sleep(frame)
	}
}

func writeToneWAV(t *testing.T, freq float64, duration time.Duration) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("tone-%.0f.wav", freq))
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%.0f:duration=%.3f", freq, duration.Seconds()),
		"-ac", "1", "-ar", "48000", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg tone wav: %v\n%s", err, out)
	}
	return path
}

func startCtPlay(t *testing.T, host, sessionID, channelID, wav string) *exec.Cmd {
	t.Helper()
	bin := envOr("QOL_CROSSTALK_PLAY", "/home/aleks/work/projects/crosstalk/bin/ct-play")
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("ct-play not found at %s: %v", bin, err)
	}
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	cmd := exec.Command(bin, "--session", sessionID, "--channel", channelID, wav)
	cmd.Env = append(os.Environ(),
		"CT_PLAY_HOST="+host,
		"CT_PLAY_USERNAME="+envOr("QOL_CROSSTALK_ADMIN_USER", "admin"),
		"CT_PLAY_PASSWORD="+envOr("QOL_CROSSTALK_ADMIN_PASSWORD", "admin"),
	)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatalf("ct-play exited early: stdout=%s stderr=%s", stdout.String(), stderr.String())
		}
		if strings.Contains(stderr.String(), "playback started") || strings.Contains(stderr.String(), "played ") {
			return cmd
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		t.Fatalf("ct-play exited: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	return cmd
}

func tonePCM(freq float64, rate, channels int, duration time.Duration) []byte {
	samples := int(duration.Seconds() * float64(rate))
	out := make([]byte, samples*channels*2)
	for i := 0; i < samples; i++ {
		value := int16(math.Sin(2*math.Pi*freq*float64(i)/float64(rate)) * 20000)
		for ch := 0; ch < channels; ch++ {
			binary.LittleEndian.PutUint16(out[(i*channels+ch)*2:], uint16(value))
		}
	}
	return out
}

func pcmEnergy(pcm []byte) float64 {
	energy := 0.0
	for i := 0; i+1 < len(pcm); i += 2 {
		sample := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
		energy += sample * sample
	}
	return energy
}

func pcmSamples(pcm []byte) []float64 {
	samples := make([]float64, len(pcm)/2)
	for i := range samples {
		samples[i] = float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
	}
	return samples
}

func mathAbs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func energeticPCM(pcm []byte) []byte {
	const window = 512
	samples := len(pcm) / 2
	for start := 0; start+window <= samples; start += window / 2 {
		var energy float64
		for i := range window {
			sample := float64(int16(binary.LittleEndian.Uint16(pcm[(start+i)*2:])))
			energy += sample * sample
		}
		if energy/float64(window) > 1e6 {
			return pcm[start*2:]
		}
	}
	return pcm
}

func assertTone(t *testing.T, pcm []byte, rate int, freq float64, minDur time.Duration) {
	t.Helper()
	if len(pcm) < 64 {
		t.Fatalf("pcm too short: %d", len(pcm))
	}
	pcm = energeticPCM(pcm)
	samples := pcmSamples(pcm)
	if pcmEnergy(pcm) == 0 {
		t.Fatal("decoded audio is silent")
	}
	got := dominantFreq(samples, rate)
	if math.Abs(got-freq) > freq*0.15 && math.Abs(got-freq) > 50 {
		t.Fatalf("dominant frequency %.1f, want %.1f", got, freq)
	}
	dur := time.Duration(len(samples)) * time.Second / time.Duration(rate)
	if dur < minDur {
		t.Fatalf("duration %s below %s", dur, minDur)
	}
}

func dominantFreq(samples []float64, rate int) float64 {
	n := 1
	for n < len(samples) {
		n <<= 1
	}
	if n > 8192 {
		n = 8192
	}
	if len(samples) > n {
		samples = samples[:n]
	}
	spec := make([]complex128, n)
	for i, sample := range samples {
		spec[i] = complex(sample, 0)
	}
	fft(spec)
	bestBin := 1
	bestMag := 0.0
	for i := 1; i < n/2; i++ {
		mag := cmplx.Abs(spec[i])
		if mag > bestMag {
			bestMag = mag
			bestBin = i
		}
	}
	return float64(bestBin) * float64(rate) / float64(n)
}

func fft(a []complex128) {
	n := len(a)
	for i, j := 0, 0; i < n; i++ {
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
		mask := n / 2
		for j >= mask && mask > 0 {
			j -= mask
			mask /= 2
		}
		j += mask
	}
	for size := 2; size <= n; size <<= 1 {
		half := size / 2
		step := -2 * math.Pi / float64(size)
		for i := 0; i < n; i += size {
			for j := 0; j < half; j++ {
				angle := complex(0, step*float64(j))
				w := cmplx.Exp(angle) * a[i+j+half]
				a[i+j+half] = a[i+j] - w
				a[i+j] += w
			}
		}
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func truncate(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return value[:n]
}
