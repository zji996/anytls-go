package session

import (
	"encoding/binary"
	"fmt"

	"github.com/sagernet/sing/common/buf"
)

const ( // cmds
	cmdWaste               = 0 // Paddings
	cmdSYN                 = 1 // stream open
	cmdPSH                 = 2 // data push
	cmdFIN                 = 3 // stream close, a.k.a EOF mark
	cmdSettings            = 4 // Settings (Client send to Server)
	cmdAlert               = 5 // Alert
	cmdUpdatePaddingScheme = 6 // update padding scheme
	// Since version 2
	cmdSYNACK         = 7  // Server reports to the client that the stream has been opened
	cmdHeartRequest   = 8  // Keep alive command
	cmdHeartResponse  = 9  // Keep alive command
	cmdServerSettings = 10 // Settings (Server send to client)
)

const (
	maxFrameDataLen    = 1<<16 - 1
	headerOverHeadSize = 1 + 4 + 2
)

// frame defines a packet from or to be multiplexed into a single connection
type frame struct {
	cmd  byte   // 1
	sid  uint32 // 4
	data []byte // 2 + len(data)
}

func newFrame(cmd byte, sid uint32) frame {
	return frame{cmd: cmd, sid: sid}
}

func encodeFrame(f frame) (*buf.Buffer, error) {
	return encodeFrameRaw(f.cmd, f.sid, f.data)
}

func encodeFrameRaw(cmd byte, sid uint32, data []byte) (*buf.Buffer, error) {
	dataLen := len(data)
	if dataLen > maxFrameDataLen {
		return nil, fmt.Errorf("frame data too large: %d", dataLen)
	}

	buffer := buf.NewSize(dataLen + headerOverHeadSize)
	buffer.WriteByte(cmd)
	binary.BigEndian.PutUint32(buffer.Extend(4), sid)
	binary.BigEndian.PutUint16(buffer.Extend(2), uint16(dataLen))
	buffer.Write(data)
	return buffer, nil
}

func newRemoteError(message string) error {
	return fmt.Errorf("remote: %s", message)
}

type rawHeader [headerOverHeadSize]byte

func (h rawHeader) Cmd() byte {
	return h[0]
}

func (h rawHeader) StreamID() uint32 {
	return binary.BigEndian.Uint32(h[1:])
}

func (h rawHeader) Length() uint16 {
	return binary.BigEndian.Uint16(h[5:])
}
