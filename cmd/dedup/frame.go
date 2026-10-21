package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Frame: "EDG1" + deflate stream of records, then 'E'.
//
//	'L' uvarint(len) bytes   literal chunk; the decoder caches it
//	'R' 32-byte sha256       chunk the decoder already holds
const frameMagic = "EDG1"

type encodeResult struct {
	frame    []byte
	refBytes int
	litBytes int
	refs     int
	literals int
	// litHashes are the chunks this frame claimed in the index; the caller
	// settles them once the decoder has answered or cannot have read them.
	litHashes []digest
}

// compression is how much CPU a frame's new content gets. References to
// cached chunks cost the same in every mode.
type compression int

const (
	compressAuto    compression = iota // BestSpeed, HuffmanOnly where BestSpeed stores
	compressHuffman                    // HuffmanOnly: cheap, but text shrinks about half as much
	compressOff                        // stored deflate blocks: only the cache saves bytes
)

func parseCompression(s string) (compression, error) {
	switch s {
	case "auto":
		return compressAuto, nil
	case "huffman":
		return compressHuffman, nil
	case "off":
		return compressOff, nil
	}
	return 0, fmt.Errorf("unknown compression %q, want auto, huffman or off", s)
}

// encodeFrame replaces chunks the peer is believed to hold with references.
// Records are streamed into the deflater from body, so besides body only the
// frame is held: 2x the body at most, which the encoder's budget counts on.
func encodeFrame(body []byte, known *lru, mode compression) (encodeResult, error) {
	var res encodeResult
	pieces := chunks(body)
	sums := make([]digest, len(pieces))
	literal := make([]bool, len(pieces))
	inFrame := map[digest]bool{}
	size := 1 // the closing 'E'
	for i, piece := range pieces {
		sum := sha256.Sum256(piece)
		sums[i] = sum
		// A repeat of a literal earlier in this frame is a reference: the decoder
		// caches literals as it reads them.
		if inFrame[sum] || known.claim(sum, len(piece)) {
			size += 1 + len(sum)
			res.refs++
			res.refBytes += len(piece)
			continue
		}
		inFrame[sum] = true
		literal[i] = true
		res.litHashes = append(res.litHashes, sum)
		size += 1 + uvarintLen(uint64(len(piece))) + len(piece)
		res.literals++
		res.litBytes += len(piece)
	}
	writeRecords := func(w io.Writer) error {
		bw := bufio.NewWriterSize(w, 64<<10)
		var tmp [binary.MaxVarintLen64]byte
		for i, piece := range pieces {
			if !literal[i] {
				bw.WriteByte('R')
				bw.Write(sums[i][:])
				continue
			}
			bw.WriteByte('L')
			bw.Write(tmp[:binary.PutUvarint(tmp[:], uint64(len(piece)))])
			bw.Write(piece)
		}
		bw.WriteByte('E')
		return bw.Flush()
	}
	level := flate.BestSpeed
	switch mode {
	case compressHuffman:
		level = flate.HuffmanOnly
	case compressOff:
		level = flate.NoCompression
	}
	// Sized once: like readBody, growing multi-MiB buffers stalls small routers.
	// Stored blocks are deflate's worst case.
	buf := bytes.NewBuffer(make([]byte, 0, len(frameMagic)+size+size/64+64))
	if err := deflate(buf, writeRecords, level); err != nil {
		return res, err
	}
	// Since Go 1.27 BestSpeed stores what it cannot match, such as base64
	// images, which Huffman coding alone still shrinks by a quarter. The
	// records are rebuilt from body, so the same buffer takes the second try.
	if mode == compressAuto && buf.Len()*10 > size*9 {
		buf.Reset()
		if err := deflate(buf, writeRecords, flate.HuffmanOnly); err != nil {
			return res, err
		}
	}
	res.frame = buf.Bytes()
	return res, nil
}

func uvarintLen(x uint64) int {
	var tmp [binary.MaxVarintLen64]byte
	return binary.PutUvarint(tmp[:], x)
}

func deflate(buf *bytes.Buffer, writeRecords func(io.Writer) error, level int) error {
	buf.WriteString(frameMagic)
	zw, err := flate.NewWriter(buf, level)
	if err != nil {
		return err
	}
	if err := writeRecords(zw); err != nil {
		return err
	}
	return zw.Close()
}

var errFrame = errors.New("malformed dedup frame")

// decodeFrame rebuilds the body. Literals are cached even when some
// references are missing, so a plain resend is the only cost of a miss.
func decodeFrame(frame []byte, want int, cache *lru) (body []byte, missing []digest, err error) {
	if !bytes.HasPrefix(frame, []byte(frameMagic)) {
		return nil, nil, errFrame
	}
	r := bufio.NewReader(flate.NewReader(bytes.NewReader(frame[len(frameMagic):])))
	out := make([]byte, 0, want)
	for {
		tag, err := r.ReadByte()
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", errFrame, err)
		}
		switch tag {
		case 'E':
			if len(missing) == 0 && len(out) != want {
				return nil, nil, fmt.Errorf("%w: length %d, want %d", errFrame, len(out), want)
			}
			return out, missing, nil
		case 'L':
			n, err := binary.ReadUvarint(r)
			if err != nil || n == 0 || n > maxChunk {
				return nil, nil, fmt.Errorf("%w: literal length", errFrame)
			}
			piece := make([]byte, n)
			if _, err := io.ReadFull(r, piece); err != nil {
				return nil, nil, fmt.Errorf("%w: literal body", errFrame)
			}
			cache.put(sha256.Sum256(piece), len(piece), piece)
			if len(missing) == 0 {
				if len(out)+len(piece) > want {
					return nil, nil, fmt.Errorf("%w: overrun", errFrame)
				}
				out = append(out, piece...)
			}
		case 'R':
			var sum digest
			if _, err := io.ReadFull(r, sum[:]); err != nil {
				return nil, nil, fmt.Errorf("%w: reference", errFrame)
			}
			piece, ok := cache.get(sum)
			if !ok {
				missing = append(missing, sum)
				continue
			}
			if len(missing) == 0 {
				if len(out)+len(piece) > want {
					return nil, nil, fmt.Errorf("%w: overrun", errFrame)
				}
				out = append(out, piece...)
			}
		default:
			return nil, nil, fmt.Errorf("%w: tag %q", errFrame, tag)
		}
	}
}
