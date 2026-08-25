package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aleksclark/qol/internal/config"
	"github.com/aleksclark/qol/internal/media"
	"github.com/aleksclark/qol/internal/worker"
	"github.com/spf13/cobra"
)

const sentinelToken = "crosstalk-sentinel-token-never-log"

func TestBuildCrosstalkConfigFromFlags(t *testing.T) {
	command := newCommand()
	run := mustRunCommand(t, command)
	if err := run.ParseFlags([]string{
		"--type", "crosstalk",
		"--crosstalk-url", "wss://crosstalk.example/ws/signaling",
		"--crosstalk-token", sentinelToken,
		"--crosstalk-source-channel", "graph.stt-whisper.input",
		"--crosstalk-sink-channel", "graph.record-es.input",
		"--crosstalk-output-profile", "ogg-opus",
	}); err != nil {
		t.Fatal(err)
	}
	settings, err := config.Bind(run)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := buildCrosstalkConfig(settings, "crosstalk", "ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerURL != "wss://crosstalk.example/ws/signaling" {
		t.Fatalf("url %q", cfg.ServerURL)
	}
	if cfg.Token != sentinelToken {
		t.Fatal("token was not loaded")
	}
	if cfg.OutputChannel != "graph.stt-whisper.input" || cfg.InputChannel != "graph.record-es.input" {
		t.Fatalf("channels %#v %#v", cfg.OutputChannel, cfg.InputChannel)
	}
	if cfg.OutputProfile.Kind != media.ProfileOggOpus {
		t.Fatalf("profile %#v", cfg.OutputProfile)
	}
	if !cfg.Reconnect || cfg.MinBackoff != 250*time.Millisecond || cfg.MaxBackoff != 8*time.Second {
		t.Fatalf("reconnect defaults %#v", cfg)
	}
	if cfg.DisableMDNS || cfg.DisableSTUN {
		t.Fatal("localhost ICE overrides must stay off by default")
	}
}

func TestBuildCrosstalkConfigPrefersTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(sentinelToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := newCommand()
	run := mustRunCommand(t, command)
	if err := run.ParseFlags([]string{
		"--type", "crosstalk",
		"--crosstalk-url", "wss://crosstalk.example/ws/signaling",
		"--crosstalk-token", "env-should-lose",
		"--crosstalk-token-file", path,
		"--crosstalk-source-channel", "graph.stt-whisper.input",
	}); err != nil {
		t.Fatal(err)
	}
	settings, err := config.Bind(run)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := buildCrosstalkConfig(settings, "crosstalk", "ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != sentinelToken {
		t.Fatalf("got %q", cfg.Token)
	}
}

func TestBuildCrosstalkConfigMissingToken(t *testing.T) {
	command := newCommand()
	run := mustRunCommand(t, command)
	if err := run.ParseFlags([]string{
		"--type", "crosstalk",
		"--crosstalk-url", "wss://crosstalk.example/ws/signaling",
		"--crosstalk-source-channel", "graph.stt-whisper.input",
	}); err != nil {
		t.Fatal(err)
	}
	settings, err := config.Bind(run)
	if err != nil {
		t.Fatal(err)
	}
	_, err = buildCrosstalkConfig(settings, "crosstalk", "ffmpeg")
	if err == nil {
		t.Fatal("expected missing token")
	}
	if strings.Contains(err.Error(), sentinelToken) {
		t.Fatalf("token leaked: %v", err)
	}
}

func TestBuildCrosstalkConfigSelfLoop(t *testing.T) {
	command := newCommand()
	run := mustRunCommand(t, command)
	if err := run.ParseFlags([]string{
		"--type", "crosstalk",
		"--crosstalk-url", "wss://crosstalk.example/ws/signaling",
		"--crosstalk-token", sentinelToken,
		"--crosstalk-source-channel", "graph.loop",
		"--crosstalk-sink-channel", "graph.loop",
	}); err != nil {
		t.Fatal(err)
	}
	settings, err := config.Bind(run)
	if err != nil {
		t.Fatal(err)
	}
	_, err = buildStage("crosstalk", settings)
	if err == nil {
		t.Fatal("expected self-loop rejection")
	}
	if !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), sentinelToken) {
		t.Fatalf("token leaked: %v", err)
	}
}

func TestBuildCrosstalkConfigSourceOnlyAndSinkOnly(t *testing.T) {
	t.Run("source", func(t *testing.T) {
		command := newCommand()
		run := mustRunCommand(t, command)
		if err := run.ParseFlags([]string{
			"--crosstalk-url", "wss://crosstalk.example/ws/signaling",
			"--crosstalk-token", sentinelToken,
			"--crosstalk-source-channel", "graph.stt-whisper.input",
		}); err != nil {
			t.Fatal(err)
		}
		settings, err := config.Bind(run)
		if err != nil {
			t.Fatal(err)
		}
		stage, err := buildStage("crosstalk", settings)
		if err != nil {
			t.Fatal(err)
		}
		spec := stage.Spec()
		if len(spec.Outputs) != 1 || len(spec.Inputs) != 0 {
			t.Fatalf("source-only spec %#v", spec)
		}
	})
	t.Run("sink", func(t *testing.T) {
		command := newCommand()
		run := mustRunCommand(t, command)
		if err := run.ParseFlags([]string{
			"--crosstalk-url", "wss://crosstalk.example/ws/signaling",
			"--crosstalk-token", sentinelToken,
			"--crosstalk-sink-channel", "graph.record-es.input",
		}); err != nil {
			t.Fatal(err)
		}
		settings, err := config.Bind(run)
		if err != nil {
			t.Fatal(err)
		}
		stage, err := buildStage("crosstalk", settings)
		if err != nil {
			t.Fatal(err)
		}
		spec := stage.Spec()
		if len(spec.Inputs) != 1 || len(spec.Outputs) != 0 {
			t.Fatalf("sink-only spec %#v", spec)
		}
	})
}

func TestUsageOmitsTokenDefault(t *testing.T) {
	command := newCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"run", "--help"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	usage := out.String()
	if strings.Contains(usage, sentinelToken) {
		t.Fatal("usage leaked sentinel")
	}
	if strings.Contains(usage, "crosstalk-token") && strings.Contains(usage, "default") && strings.Contains(strings.ToLower(usage), "token") {
		if strings.Contains(usage, `--crosstalk-token ""`) || strings.Contains(usage, "--crosstalk-token string") {
			return
		}
	}
	if !strings.Contains(usage, "--crosstalk-token") {
		t.Fatal("missing token flag")
	}
}

func TestHealthFileExitCodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health")
	if err := writeHealthFile(path, worker.CrosstalkHealth{Ready: false, Reason: "connecting"}); err != nil {
		t.Fatal(err)
	}
	payload, err := readHealthFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Ready || payload.Reason != "connecting" {
		t.Fatalf("%#v", payload)
	}
	if err := writeHealthFile(path, worker.CrosstalkHealth{Ready: true, Reason: "ready", SessionID: "sess/peer/1"}); err != nil {
		t.Fatal(err)
	}
	payload, err = readHealthFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !payload.Ready || payload.SessionID != "sess/peer/1" {
		t.Fatalf("%#v", payload)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), sentinelToken) {
		t.Fatal("token written to health file")
	}
}

func mustRunCommand(t *testing.T, root *cobra.Command) *cobra.Command {
	t.Helper()
	for _, command := range root.Commands() {
		if command.Name() == "run" {
			return command
		}
	}
	t.Fatal("run command missing")
	return nil
}
