package qol

const (
	ChannelCaptureInput      = "pipeline.capture.input"
	ChannelCaptureOutput     = "pipeline.capture.output"
	ChannelAudioInput        = "pipeline.stt-whisper.input"
	ChannelRecordENInput     = "pipeline.record-en.input"
	ChannelASRText           = "pipeline.translate.input"
	ChannelTranslationText   = "pipeline.tts.input"
	ChannelTTSAudio          = "pipeline.record-es.input"
	ChannelRecordENCompleted = "pipeline.record-en.completed"
	ChannelRecordESCompleted = "pipeline.record-es.completed"
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
