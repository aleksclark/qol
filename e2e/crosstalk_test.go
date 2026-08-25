package e2e_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCrosstalkAdmissionAndSourcePCM(t *testing.T) {
	api := requireCrosstalkE2E(t)
	natsURL, bus, cleanup := startInProcessNATS(t)
	defer cleanup()

	session := api.createSession(t, "qol-source")
	// Admin tickets may produce into broadcast only. ct-play uses that path;
	// the ABC monitors the same broadcast so Crosstalk audio reaches Qol.
	floor := api.createChannel(t, session.ID, "floor", "broadcast")
	abc := api.createABC(t, "qol-source-abc")
	api.assignABC(t, abc, "qol-source-abc", session.ID, &floor.ID)

	bin := buildQolWorker(t)
	tokenFile := writeTokenFile(t, abc.Token)
	healthFile := filepath.Join(t.TempDir(), "health.json")
	worker := startCrosstalkWorker(t, bin, natsURL, api.base, tokenFile, healthFile,
		"--crosstalk-source-channel", "graph.crosstalk.pcm",
		"--crosstalk-output-profile", "pcm-s16le",
		"--crosstalk-output-rate", "16000",
		"--crosstalk-output-channels", "1",
	)
	health := worker.waitReady(t, 20*time.Second)
	if health["assigned"] != session.ID {
		t.Fatalf("assigned=%v want %s", health["assigned"], session.ID)
	}
	if health["peer"] == "" || health["session_id"] == "" {
		t.Fatalf("health missing peer/session: %#v", health)
	}

	wav := writeToneWAV(t, sourceToneHz, 4*time.Second)
	startCtPlay(t, api.base, session.ID, floor.ID, wav)

	pcm := collectPCM(t, bus, "graph.crosstalk.pcm", sourceRate*2, 25*time.Second)
	assertTone(t, pcm, sourceRate, sourceToneHz, 400*time.Millisecond)
	worker.assertTokenAbsent(t)
}

func TestCrosstalkSinkPCM(t *testing.T) {
	api := requireCrosstalkE2E(t)
	natsURL, bus, cleanup := startInProcessNATS(t)
	defer cleanup()

	session := api.createSession(t, "qol-sink")
	feed := api.createChannel(t, session.ID, "translation", "feed")
	abc := api.createABC(t, "qol-sink-abc")
	api.assignABC(t, abc, "qol-sink-abc", session.ID, nil)

	bin := buildQolWorker(t)
	tokenFile := writeTokenFile(t, abc.Token)
	healthFile := filepath.Join(t.TempDir(), "health.json")
	worker := startCrosstalkWorker(t, bin, natsURL, api.base, tokenFile, healthFile,
		"--crosstalk-sink-channel", "graph.tts.audio",
	)
	health := worker.waitReady(t, 20*time.Second)
	if health["assigned"] != session.ID {
		t.Fatalf("assigned=%v want %s", health["assigned"], session.ID)
	}

	listener := startFeedListener(t, api, session.ID, feed.Name)
	publishTone(t, bus, "graph.tts.audio", "qol-sink-session", sinkToneHz, sinkRate, 3*time.Second)
	pcm := listener.waitPCM(t, 48000, 20*time.Second)
	assertTone(t, pcm, 48000, sinkToneHz, 400*time.Millisecond)
	worker.assertTokenAbsent(t)
}

func TestCrosstalkDuplexPCM(t *testing.T) {
	api := requireCrosstalkE2E(t)
	natsURL, bus, cleanup := startInProcessNATS(t)
	defer cleanup()

	session := api.createSession(t, "qol-duplex")
	floor := api.createChannel(t, session.ID, "floor", "broadcast")
	feed := api.createChannel(t, session.ID, "translation", "feed")
	abc := api.createABC(t, "qol-duplex-abc")
	api.assignABC(t, abc, "qol-duplex-abc", session.ID, &floor.ID)

	bin := buildQolWorker(t)
	tokenFile := writeTokenFile(t, abc.Token)
	healthFile := filepath.Join(t.TempDir(), "health.json")
	worker := startCrosstalkWorker(t, bin, natsURL, api.base, tokenFile, healthFile,
		"--crosstalk-source-channel", "graph.crosstalk.pcm",
		"--crosstalk-sink-channel", "graph.tts.audio",
		"--crosstalk-output-profile", "pcm-s16le",
		"--crosstalk-output-rate", "16000",
		"--crosstalk-output-channels", "1",
	)
	health := worker.waitReady(t, 20*time.Second)
	if health["assigned"] != session.ID {
		t.Fatalf("assigned=%v want %s", health["assigned"], session.ID)
	}

	listener := startFeedListener(t, api, session.ID, feed.Name)
	wav := writeToneWAV(t, sourceToneHz, 4*time.Second)
	startCtPlay(t, api.base, session.ID, floor.ID, wav)
	source := startPCMCollector(t, bus, "graph.crosstalk.pcm")
	publishTone(t, bus, "graph.tts.audio", "qol-duplex-session", sinkToneHz, sinkRate, 3*time.Second)

	sourcePCM, err := source.wait(sourceRate*2, 25*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sinkPCM := listener.waitPCM(t, 48000, 20*time.Second)

	assertTone(t, sourcePCM, sourceRate, sourceToneHz, 400*time.Millisecond)
	assertTone(t, sinkPCM, 48000, sinkToneHz, 400*time.Millisecond)
	if dominant := dominantFreq(pcmSamples(energeticPCM(sourcePCM)), sourceRate); mathAbs(dominant-sinkToneHz) < 50 {
		t.Fatalf("source channel heard sink tone %.1f; self-loop", dominant)
	}
	if dominant := dominantFreq(pcmSamples(energeticPCM(sinkPCM)), 48000); mathAbs(dominant-sourceToneHz) < 50 {
		t.Fatalf("sink listener heard source tone %.1f; self-loop", dominant)
	}
	worker.assertTokenAbsent(t)
}

func TestCrosstalkSourceEncoded(t *testing.T) {
	api := requireCrosstalkE2E(t)
	natsURL, bus, cleanup := startInProcessNATS(t)
	defer cleanup()

	session := api.createSession(t, "qol-source-ogg")
	floor := api.createChannel(t, session.ID, "floor", "broadcast")
	abc := api.createABC(t, "qol-source-ogg-abc")
	api.assignABC(t, abc, "qol-source-ogg-abc", session.ID, &floor.ID)

	source := startStreamCollector(t, bus, "graph.stt-whisper.input")
	bin := buildQolWorker(t)
	tokenFile := writeTokenFile(t, abc.Token)
	healthFile := filepath.Join(t.TempDir(), "health.json")
	worker := startCrosstalkWorker(t, bin, natsURL, api.base, tokenFile, healthFile,
		"--crosstalk-source-channel", "graph.stt-whisper.input",
		"--crosstalk-output-profile", "ogg-opus",
	)
	health := worker.waitReady(t, 20*time.Second)
	if health["assigned"] != session.ID {
		t.Fatalf("assigned=%v want %s", health["assigned"], session.ID)
	}

	wav := writeToneWAV(t, sourceToneHz, 4*time.Second)
	startCtPlay(t, api.base, session.ID, floor.ID, wav)
	ogg, err := source.wait(12000, 25*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pcm := decodeOggToPCM(t, ogg, sourceRate)
	assertTone(t, pcm, sourceRate, sourceToneHz, 400*time.Millisecond)
	worker.assertTokenAbsent(t)
}

func TestCrosstalkSinkEncoded(t *testing.T) {
	api := requireCrosstalkE2E(t)
	natsURL, bus, cleanup := startInProcessNATS(t)
	defer cleanup()

	session := api.createSession(t, "qol-sink-ogg")
	feed := api.createChannel(t, session.ID, "translation", "feed")
	abc := api.createABC(t, "qol-sink-ogg-abc")
	api.assignABC(t, abc, "qol-sink-ogg-abc", session.ID, nil)

	bin := buildQolWorker(t)
	tokenFile := writeTokenFile(t, abc.Token)
	healthFile := filepath.Join(t.TempDir(), "health.json")
	worker := startCrosstalkWorker(t, bin, natsURL, api.base, tokenFile, healthFile,
		"--crosstalk-sink-channel", "graph.capture.output",
	)
	health := worker.waitReady(t, 20*time.Second)
	if health["assigned"] != session.ID {
		t.Fatalf("assigned=%v want %s", health["assigned"], session.ID)
	}

	listener := startFeedListener(t, api, session.ID, feed.Name)
	publishEncodedTone(t, bus, "graph.capture.output", "qol-sink-ogg-session", sinkToneHz, 3*time.Second)
	pcm := listener.waitPCM(t, 48000, 20*time.Second)
	assertTone(t, pcm, 48000, sinkToneHz, 400*time.Millisecond)
	worker.assertTokenAbsent(t)
}

func TestCrosstalkIsolationPCM(t *testing.T) {
	api := requireCrosstalkE2E(t)
	natsURL, bus, cleanup := startInProcessNATS(t)
	defer cleanup()

	sessionA := api.createSession(t, "qol-iso-a")
	floorA := api.createChannel(t, sessionA.ID, "floor", "broadcast")
	abcA := api.createABC(t, "qol-iso-abc-a")
	api.assignABC(t, abcA, "qol-iso-abc-a", sessionA.ID, &floorA.ID)

	sessionB := api.createSession(t, "qol-iso-b")
	floorB := api.createChannel(t, sessionB.ID, "floor", "broadcast")
	abcB := api.createABC(t, "qol-iso-abc-b")
	api.assignABC(t, abcB, "qol-iso-abc-b", sessionB.ID, &floorB.ID)

	bin := buildQolWorker(t)
	workerA := startCrosstalkWorker(t, bin, natsURL, api.base, writeTokenFile(t, abcA.Token), filepath.Join(t.TempDir(), "health-a.json"),
		"--crosstalk-source-channel", "graph.crosstalk.a",
		"--crosstalk-output-profile", "pcm-s16le",
		"--crosstalk-output-rate", "16000",
		"--crosstalk-output-channels", "1",
		"--crosstalk-client-name", "qol-e2e-a",
	)
	workerB := startCrosstalkWorker(t, bin, natsURL, api.base, writeTokenFile(t, abcB.Token), filepath.Join(t.TempDir(), "health-b.json"),
		"--crosstalk-source-channel", "graph.crosstalk.b",
		"--crosstalk-output-profile", "pcm-s16le",
		"--crosstalk-output-rate", "16000",
		"--crosstalk-output-channels", "1",
		"--crosstalk-client-name", "qol-e2e-b",
	)
	healthA := workerA.waitReady(t, 20*time.Second)
	healthB := workerB.waitReady(t, 20*time.Second)
	if healthA["assigned"] != sessionA.ID {
		t.Fatalf("A assigned=%v want %s", healthA["assigned"], sessionA.ID)
	}
	if healthB["assigned"] != sessionB.ID {
		t.Fatalf("B assigned=%v want %s", healthB["assigned"], sessionB.ID)
	}
	if healthA["session_id"] == healthB["session_id"] {
		t.Fatalf("workers shared session_id %v", healthA["session_id"])
	}

	collectA := startPCMCollector(t, bus, "graph.crosstalk.a")
	collectB := startPCMCollector(t, bus, "graph.crosstalk.b")
	startCtPlay(t, api.base, sessionA.ID, floorA.ID, writeToneWAV(t, sourceToneHz, 4*time.Second))
	startCtPlay(t, api.base, sessionB.ID, floorB.ID, writeToneWAV(t, sinkToneHz, 4*time.Second))
	pcmA, err := collectA.wait(sourceRate*2, 25*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pcmB, err := collectB.wait(sourceRate*2, 25*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	assertTone(t, pcmA, sourceRate, sourceToneHz, 400*time.Millisecond)
	assertTone(t, pcmB, sourceRate, sinkToneHz, 400*time.Millisecond)
	if dominant := dominantFreq(pcmSamples(energeticPCM(pcmA)), sourceRate); mathAbs(dominant-sinkToneHz) < 50 {
		t.Fatalf("session A heard session B tone %.1f", dominant)
	}
	if dominant := dominantFreq(pcmSamples(energeticPCM(pcmB)), sourceRate); mathAbs(dominant-sourceToneHz) < 50 {
		t.Fatalf("session B heard session A tone %.1f", dominant)
	}
	workerA.assertTokenAbsent(t)
	workerB.assertTokenAbsent(t)
}

func TestCrosstalkWorkerSIGTERM(t *testing.T) {
	api := requireCrosstalkE2E(t)
	natsURL, _, cleanup := startInProcessNATS(t)
	defer cleanup()

	session := api.createSession(t, "qol-sigterm")
	abc := api.createABC(t, "qol-sigterm-abc")
	api.assignABC(t, abc, "qol-sigterm-abc", session.ID, nil)

	bin := buildQolWorker(t)
	worker := startCrosstalkWorker(t, bin, natsURL, api.base, writeTokenFile(t, abc.Token), filepath.Join(t.TempDir(), "health.json"),
		"--crosstalk-source-channel", "graph.crosstalk.pcm",
	)
	if health := worker.waitReady(t, 20*time.Second); health["assigned"] != session.ID {
		t.Fatalf("assigned=%v want %s", health["assigned"], session.ID)
	}
	if err := worker.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- worker.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not exit after SIGINT")
	}
	worker.assertTokenAbsent(t)
}
