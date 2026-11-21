package wire

import (
	"bufio"
	"errors"
	"io"
	"math"
	"strings"

	qol "github.com/aleksclark/qol"
	qolv1 "github.com/aleksclark/qol/gen/qol/v1"
	"google.golang.org/protobuf/proto"
)

func glossaryToProto(entries []qol.GlossaryEntry) []*qolv1.GlossaryEntry {
	var result []*qolv1.GlossaryEntry
	for _, entry := range entries {
		result = append(result, &qolv1.GlossaryEntry{Source: entry.Source, Target: entry.Target})
	}
	return result
}

func glossaryFromProto(entries []*qolv1.GlossaryEntry) []qol.GlossaryEntry {
	var result []qol.GlossaryEntry
	for _, entry := range entries {
		result = append(result, qol.GlossaryEntry{Source: entry.GetSource(), Target: entry.GetTarget()})
	}
	return result
}

func validateGlossary(entries []qol.GlossaryEntry) error {
	for _, entry := range entries {
		if strings.TrimSpace(entry.Source) == "" || strings.TrimSpace(entry.Target) == "" {
			return errors.New("glossary entries need source and target text")
		}
	}
	return nil
}

func validRatio(ratio float32) bool {
	return !math.IsNaN(float64(ratio)) && ratio > 0 && ratio < 1
}

func MarshalControl(value qol.Control) ([]byte, error) {
	message := new(qolv1.ControlPayload)
	switch value.Kind {
	case qol.ControlCondense:
		if len(value.TargetSequences) == 0 || !validRatio(value.TargetRatio) {
			return nil, errors.New("condense needs target sequences and a ratio within (0, 1)")
		}
		message.Directive = &qolv1.ControlPayload_Condense{Condense: &qolv1.CondenseDirective{TargetSequences: append([]uint64(nil), value.TargetSequences...), TargetRatio: value.TargetRatio}}
	case qol.ControlSummarize:
		message.Directive = &qolv1.ControlPayload_Summarize{Summarize: &qolv1.SummarizeDirective{}}
	case qol.ControlGlossary:
		if len(value.Glossary) == 0 {
			return nil, errors.New("glossary control needs entries")
		}
		if err := validateGlossary(value.Glossary); err != nil {
			return nil, err
		}
		message.Directive = &qolv1.ControlPayload_Glossary{Glossary: &qolv1.GlossaryDirective{Entries: glossaryToProto(value.Glossary)}}
	default:
		return nil, errors.New("unknown control directive")
	}
	return proto.Marshal(message)
}

func UnmarshalControl(payload []byte) (qol.Control, error) {
	message := new(qolv1.ControlPayload)
	if err := proto.Unmarshal(payload, message); err != nil {
		return qol.Control{}, err
	}
	var value qol.Control
	switch directive := message.Directive.(type) {
	case *qolv1.ControlPayload_Condense:
		value = qol.Control{Kind: qol.ControlCondense, TargetSequences: append([]uint64(nil), directive.Condense.GetTargetSequences()...), TargetRatio: directive.Condense.GetTargetRatio()}
	case *qolv1.ControlPayload_Summarize:
		value = qol.Control{Kind: qol.ControlSummarize}
	case *qolv1.ControlPayload_Glossary:
		value = qol.Control{Kind: qol.ControlGlossary, Glossary: glossaryFromProto(directive.Glossary.GetEntries())}
	default:
		return qol.Control{}, errors.New("control directive is required")
	}
	_, err := MarshalControl(value)
	return value, err
}

func MarshalTelemetry(value qol.Telemetry) ([]byte, error) {
	if err := validateTelemetry(value); err != nil {
		return nil, err
	}
	return proto.Marshal(&qolv1.TelemetryPayload{
		Stage:               value.Stage,
		ConsumedThroughNs:   int64(value.ConsumedThrough),
		OutputCursorNs:      int64(value.OutputCursor),
		BacklogSeconds:      value.BacklogSeconds,
		QueuedSequences:     append([]uint64(nil), value.QueuedSequences...),
		ProjectedLagSeconds: value.ProjectedLagSeconds,
		ProcessingMs:        value.ProcessingMillis,
		ContextTurns:        value.ContextTurns,
	})
}

func UnmarshalTelemetry(payload []byte) (qol.Telemetry, error) {
	message := new(qolv1.TelemetryPayload)
	if err := proto.Unmarshal(payload, message); err != nil {
		return qol.Telemetry{}, err
	}
	value := qol.Telemetry{
		Stage:               message.Stage,
		ConsumedThrough:     qol.MediaTime(message.ConsumedThroughNs),
		OutputCursor:        qol.MediaTime(message.OutputCursorNs),
		BacklogSeconds:      message.BacklogSeconds,
		QueuedSequences:     append([]uint64(nil), message.QueuedSequences...),
		ProjectedLagSeconds: message.ProjectedLagSeconds,
		ProcessingMillis:    message.ProcessingMs,
		ContextTurns:        message.ContextTurns,
	}
	return value, validateTelemetry(value)
}

func validateTelemetry(value qol.Telemetry) error {
	if value.Stage == "" {
		return errors.New("telemetry stage is required")
	}
	if value.ConsumedThrough < 0 || value.OutputCursor < 0 || value.BacklogSeconds < 0 || value.ProcessingMillis < 0 {
		return errors.New("telemetry times must not be negative")
	}
	for _, number := range []float64{value.BacklogSeconds, value.ProjectedLagSeconds, value.ProcessingMillis} {
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return errors.New("telemetry values must be finite")
		}
	}
	return nil
}

func WriteTranslatorRequest(writer io.Writer, value qol.TranslatorRequest) error {
	if value.SessionID == "" {
		return errors.New("translator request needs a session")
	}
	message := &qolv1.TranslatorRequest{SessionId: value.SessionID}
	switch value.Kind {
	case qol.TranslatorTurn:
		if strings.TrimSpace(value.Text) == "" && !value.Final {
			return errors.New("translator turn needs text unless it is final")
		}
		message.Request = &qolv1.TranslatorRequest_Turn{Turn: &qolv1.TranslateTurn{TurnId: value.TurnID, Text: value.Text, Final: value.Final}}
	case qol.TranslatorCondense:
		if len(value.TurnIDs) == 0 || !validRatio(value.TargetRatio) {
			return errors.New("condense needs turns and a ratio within (0, 1)")
		}
		message.Request = &qolv1.TranslatorRequest_Condense{Condense: &qolv1.CondenseTurns{TurnIds: append([]uint64(nil), value.TurnIDs...), TargetRatio: value.TargetRatio}}
	case qol.TranslatorSummarize:
		message.Request = &qolv1.TranslatorRequest_Summarize{Summarize: &qolv1.SummarizeContext{}}
	case qol.TranslatorGlossary:
		if err := validateGlossary(value.Glossary); err != nil || len(value.Glossary) == 0 {
			return errors.New("glossary update needs entries with source and target")
		}
		message.Request = &qolv1.TranslatorRequest_Glossary{Glossary: &qolv1.GlossaryUpdate{Entries: glossaryToProto(value.Glossary)}}
	case qol.TranslatorEnd:
		message.Request = &qolv1.TranslatorRequest_End{End: &qolv1.EndSession{}}
	default:
		return errors.New("unknown translator request")
	}
	return WriteFrame(writer, message)
}

func ReadTranslatorRequest(reader *bufio.Reader) (qol.TranslatorRequest, error) {
	message := new(qolv1.TranslatorRequest)
	if err := ReadFrame(reader, message); err != nil {
		return qol.TranslatorRequest{}, err
	}
	value := qol.TranslatorRequest{SessionID: message.SessionId}
	switch request := message.Request.(type) {
	case *qolv1.TranslatorRequest_Turn:
		value.Kind, value.TurnID, value.Text, value.Final = qol.TranslatorTurn, request.Turn.GetTurnId(), request.Turn.GetText(), request.Turn.GetFinal()
	case *qolv1.TranslatorRequest_Condense:
		value.Kind, value.TurnIDs, value.TargetRatio = qol.TranslatorCondense, append([]uint64(nil), request.Condense.GetTurnIds()...), request.Condense.GetTargetRatio()
	case *qolv1.TranslatorRequest_Summarize:
		value.Kind = qol.TranslatorSummarize
	case *qolv1.TranslatorRequest_Glossary:
		value.Kind, value.Glossary = qol.TranslatorGlossary, glossaryFromProto(request.Glossary.GetEntries())
	case *qolv1.TranslatorRequest_End:
		value.Kind = qol.TranslatorEnd
	default:
		return qol.TranslatorRequest{}, errors.New("translator request body is required")
	}
	if value.SessionID == "" {
		return qol.TranslatorRequest{}, errors.New("translator request needs a session")
	}
	return value, nil
}

func WriteTranslatorResponse(writer io.Writer, value qol.TranslatorResponse) error {
	return WriteFrame(writer, &qolv1.TranslatorResponse{Text: value.Text, ContextTurns: value.ContextTurns})
}

func ReadTranslatorResponse(reader *bufio.Reader) (qol.TranslatorResponse, error) {
	message := new(qolv1.TranslatorResponse)
	if err := ReadFrame(reader, message); err != nil {
		return qol.TranslatorResponse{}, err
	}
	return qol.TranslatorResponse{Text: message.Text, ContextTurns: message.ContextTurns}, nil
}
