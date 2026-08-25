package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"nhooyr.io/websocket"
)

type ticketListener struct {
	pc        *webrtc.PeerConnection
	ws        *websocket.Conn
	connected chan struct{}

	mu  sync.Mutex
	pcm []byte
}

func startFeedListener(t *testing.T, api *crosstalkAPI, sessionID, feedName string) *ticketListener {
	t.Helper()
	ticket := api.issueTicket(t, sessionID, []string{}, []string{feedName}, "admin")
	wsURL := strings.Replace(api.base, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL += "/api/sessions/" + url.PathEscape(sessionID) + "/ws?token=" + url.QueryEscape(ticket)
	return startPublicListener(t, "feed-"+feedName, wsURL, false)
}

func startPublicListener(t *testing.T, name, wsURL string, serverOffer bool) *ticketListener {
	t.Helper()
	var settings webrtc.SettingEngine
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settings))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{}})
	if err != nil {
		t.Fatal(err)
	}
	listener := &ticketListener{pc: pc, connected: make(chan struct{})}
	if !serverOffer {
		track, err := webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
			name+"-mic", name,
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionSendrecv,
		}); err != nil {
			t.Fatal(err)
		}
	} else if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}

	decodeCtx, decodeCancel := context.WithCancel(context.Background())
	t.Cleanup(decodeCancel)
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if err := streamRemoteTrack(decodeCtx, remote, listener); err != nil && decodeCtx.Err() == nil {
			t.Logf("%s decode ended: %v", name, err)
		}
	})

	var once sync.Once
	mark := func() { once.Do(func() { close(listener.connected) }) }
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateConnected || state == webrtc.ICEConnectionStateCompleted {
			mark()
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			mark()
		}
	})

	ctx := context.Background()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		_ = pc.Close()
		t.Fatalf("%s ws dial: %v", name, err)
	}
	listener.ws = ws
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		msg, _ := json.Marshal(map[string]any{"type": "candidate", "candidate": init})
		_ = ws.Write(ctx, websocket.MessageText, msg)
	})
	go readPublicSignaling(ctx, ws, pc)
	if !serverOffer {
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := pc.SetLocalDescription(offer); err != nil {
			t.Fatal(err)
		}
		gather := webrtc.GatheringCompletePromise(pc)
		msg, _ := json.Marshal(map[string]string{"type": "offer", "sdp": offer.SDP})
		if err := ws.Write(ctx, websocket.MessageText, msg); err != nil {
			t.Fatal(err)
		}
		select {
		case <-gather:
		case <-time.After(5 * time.Second):
		}
	}
	select {
	case <-listener.connected:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s ICE connect timeout pc=%s ice=%s", name, pc.ConnectionState(), pc.ICEConnectionState())
	}
	t.Cleanup(func() {
		_ = ws.Close(websocket.StatusNormalClosure, "")
		_ = pc.Close()
	})
	return listener
}

func readPublicSignaling(ctx context.Context, ws *websocket.Conn, pc *webrtc.PeerConnection) {
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			Type      string                  `json:"type"`
			SDP       string                  `json:"sdp"`
			Candidate *webrtc.ICECandidateInit `json:"candidate"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "answer":
			_ = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: msg.SDP})
		case "offer":
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: msg.SDP}); err != nil {
				continue
			}
			ans, err := pc.CreateAnswer(nil)
			if err != nil {
				continue
			}
			_ = pc.SetLocalDescription(ans)
			out, _ := json.Marshal(map[string]string{"type": "answer", "sdp": ans.SDP})
			_ = ws.Write(ctx, websocket.MessageText, out)
		case "ice", "candidate":
			if msg.Candidate != nil {
				_ = pc.AddICECandidate(*msg.Candidate)
			}
		}
	}
}

func (l *ticketListener) captured() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.pcm...)
}

func (l *ticketListener) waitPCM(t *testing.T, minBytes int, wait time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		pcm := l.captured()
		if len(pcm) >= minBytes {
			return pcm
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("listener collected %d pcm bytes, want >= %d", len(l.captured()), minBytes)
	return nil
}

func streamRemoteTrack(ctx context.Context, remote *webrtc.TrackRemote, listener *ticketListener) error {
	port, err := freeUDPPort()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", "pipe,file,udp,rtp",
		"-probesize", "32", "-analyzeduration", "0", "-fflags", "+nobuffer",
		"-f", "sdp", "-i", "pipe:0",
		"-f", "s16le", "-ar", "48000", "-ac", "1", "pipe:1",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	sdp := fmt.Sprintf("v=0\no=- 0 0 IN IP4 127.0.0.1\ns=Qol\nc=IN IP4 127.0.0.1\nt=0 0\nm=audio %d RTP/AVP 111\na=rtpmap:111 opus/48000/2\n", port)
	if _, err := io.WriteString(stdin, sdp); err != nil {
		return err
	}
	_ = stdin.Close()
	if err := waitUDPBound(ctx, port); err != nil {
		return err
	}
	conn, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return err
	}
	defer conn.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				listener.mu.Lock()
				listener.pcm = append(listener.pcm, buf[:n]...)
				listener.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		packet, _, err := remote.ReadRTP()
		if err != nil {
			break
		}
		if len(packet.Payload) == 0 {
			continue
		}
		raw, err := packet.Marshal()
		if err != nil {
			continue
		}
		_, _ = conn.Write(raw)
	}
	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

func freeUDPPort() (int, error) {
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	if err := probe.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func waitUDPBound(ctx context.Context, port int) error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		probe, err := net.ListenPacket("udp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			return nil
		}
		_ = probe.Close()
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("ffmpeg did not bind udp port %d", port)
}
