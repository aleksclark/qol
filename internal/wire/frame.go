package wire

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

const MaxFrameSize = 4 << 20

func ReadFrame(reader *bufio.Reader, message proto.Message) error {
	size, err := binary.ReadUvarint(reader)
	if err != nil {
		return err
	}
	if size > MaxFrameSize {
		return fmt.Errorf("protobuf frame exceeds %d bytes", MaxFrameSize)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return err
	}
	return proto.Unmarshal(payload, message)
}

func WriteFrame(writer io.Writer, message proto.Message) error {
	payload, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("protobuf frame exceeds %d bytes", MaxFrameSize)
	}
	buffer := make([]byte, binary.MaxVarintLen64)
	size := binary.PutUvarint(buffer, uint64(len(payload)))
	if _, err := writer.Write(buffer[:size]); err != nil {
		return err
	}
	_, err = writer.Write(payload)
	return err
}
