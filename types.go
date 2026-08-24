package qol

const (
	ChannelCaptureInput      = "graph.capture.input"
	ChannelCaptureOutput     = "graph.capture.output"
	ChannelAudioInput        = "graph.stt-whisper.input"
	ChannelRecordENInput     = "graph.record-en.input"
	ChannelASRText           = "graph.translate.input"
	ChannelTranslationText   = "graph.tts.input"
	ChannelTTSAudio          = "graph.record-es.input"
	ChannelRecordENCompleted = "graph.record-en.completed"
	ChannelRecordESCompleted = "graph.record-es.completed"
)

const (
	TypeAudioPCM         = "qol.audio.pcm.partial"
	TypeAudioPCMFinal    = "qol.audio.pcm.final"
	TypeAudioStream      = "qol.audio.stream"
	TypeTextFinal        = "qol.text.final"
	TypeTranslationFinal = "qol.translation.final"
	TypeStoredAudio      = "qol.audio.stored"
	EncodingProtobuf     = "application/protobuf"
)
