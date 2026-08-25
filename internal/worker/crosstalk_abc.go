package worker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"strings"
	"time"

	"github.com/aleksclark/crosstalk/abc"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/aleksclark/qol/internal/media"
)

func isTerminalABCError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, media.ErrUnsupportedCodec) || errors.Is(err, media.ErrUnsupportedMedia) {
		return true
	}
	if abc.IsAuthError(err) || abc.IsProtocolError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unsupported-codec") || strings.Contains(msg, "unsupported codec") || strings.Contains(msg, "incompatible protocol")
}

func terminalReason(err error) string {
	if err == nil {
		return "closed"
	}
	if abc.IsAuthError(err) {
		return "auth"
	}
	if abc.IsProtocolError(err) {
		return "protocol"
	}
	if errors.Is(err, media.ErrUnsupportedCodec) || errors.Is(err, media.ErrUnsupportedMedia) {
		return "unsupported-codec"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unsupported"):
		return "unsupported-codec"
	case strings.Contains(msg, "auth"):
		return "auth"
	case strings.Contains(msg, "protocol"):
		return "protocol"
	default:
		return "closed"
	}
}

func nextBackoff(cfg CrosstalkConfig, current time.Duration) time.Duration {
	minDelay := cfg.MinBackoff
	if minDelay <= 0 {
		minDelay = 250 * time.Millisecond
	}
	maxDelay := cfg.MaxBackoff
	if maxDelay < minDelay {
		maxDelay = 8 * time.Second
	}
	if current <= 0 {
		current = minDelay
	} else {
		current *= 2
	}
	if current < minDelay {
		current = minDelay
	}
	if current > maxDelay {
		current = maxDelay
	}
	jitter := time.Duration(rand.Int63n(int64(current/2) + 1))
	return current/2 + jitter
}


func defaultABCDial(ctx context.Context, cfg CrosstalkConfig) (ABCSession, error) {
	clientName := cfg.ClientName
	if clientName == "" {
		clientName = "qol"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	dialCfg := abc.Config{
		ServerURL:      cfg.ServerURL,
		Token:          cfg.Token,
		ClientName:     clientName,
		RequireWelcome: true,
		Logger:         logger,
		DisableMDNS:    cfg.DisableMDNS,
	}
	if cfg.DisableSTUN {
		dialCfg.ICEServers = []webrtc.ICEServer{}
	}
	session, err := abc.Dial(ctx, dialCfg)
	if err != nil {
		return nil, err
	}
	session.OnControl(func(msg abc.ControlMessage) {
		if msg.Restart != nil {
			logger.Info("crosstalk restart command", "reason", msg.Restart.Reason)
			_ = session.Close()
		}
	})
	return &abcSession{session: session}, nil
}

type abcSession struct {
	session *abc.Session
}

func (s *abcSession) Welcome() ABCWelcome {
	welcome := s.session.Welcome()
	return ABCWelcome{PeerID: welcome.PeerID, AssignedSessionID: welcome.AssignedSessionID, Epoch: welcome.Epoch}
}

func (s *abcSession) NegotiatedCodec() (media.Codec, bool) {
	codec, ok := s.session.NegotiatedCodec()
	return media.Codec{MimeType: codec.MimeType, ClockRate: codec.ClockRate, Channels: codec.Channels}, ok
}

func (s *abcSession) WriteRTP(packet *rtp.Packet) error {
	return s.session.SendTrack().WriteRTP(packet)
}

func (s *abcSession) OnTrack(fn func(ABCTrack)) {
	s.session.OnTrack(func(track abc.IncomingTrack) {
		fn(abcTrack{track: track})
	})
}

func (s *abcSession) OnClose(fn func(string)) { s.session.OnClose(fn) }
func (s *abcSession) Done() <-chan struct{}   { return s.session.Done() }
func (s *abcSession) Close() error            { return s.session.Close() }
func (s *abcSession) Err() error              { return s.session.Err() }

type abcTrack struct {
	track abc.IncomingTrack
}

func (t abcTrack) ReadRTP() (*rtp.Packet, error) { return t.track.ReadRTP() }

func (t abcTrack) Codec() media.Codec {
	return media.Codec{MimeType: t.track.Codec.MimeType, ClockRate: t.track.Codec.ClockRate, Channels: t.track.Codec.Channels}
}
