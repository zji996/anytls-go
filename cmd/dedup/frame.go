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
	frame     []byte
	refBytes  int
	litBytes  int
	refs      int
	literals  int
	refHashes []digest
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
func encodeFrame(body []byte, known *lru, mode compression) (encodeResult, error) {
	var res encodeResult
	// Sized once: like readBody, growing multi-MiB buffers stalls small routers.
	// A record is at most a varint longer than its chunk.
	records := bytes.NewBuffer(make([]byte, 0, len(body)+(len(body)/minChunk+2)*(1+binary.MaxVarintLen64)))
	var tmp [binary.MaxVarintLen64]byte
	for _, piece := range chunks(body) {
		sum := sha256.Sum256(piece)
		if _, ok := known.get(sum); ok {
			records.WriteByte('R')
			records.Write(sum[:])
			res.refs++
			res.refBytes += len(piece)
			res.refHashes = append(res.refHashes, sum)
			continue
		}
		known.put(sum, len(piece), nil)
		records.WriteByte('L')
		records.Write(tmp[:binary.PutUvarint(tmp[:], uint64(len(piece)))])
		records.Write(piece)
		res.literals++
		res.litBytes += len(piece)
	}
	records.WriteByte('E')
	level := flate.BestSpeed
	switch mode {
	case compressHuffman:
		level = flate.HuffmanOnly
	case compressOff:
		level = flate.NoCompression
	}
	frame, err := deflate(records.Bytes(), level)
	if err != nil {
		return res, err
	}
	// Since Go 1.27 BestSpeed stores what it cannot match, such as base64
	// images, which Huffman coding alone still shrinks by a quarter.
	if mode == compressAuto && len(frame)*10 > records.Len()*9 {
		frame = nil
		if frame, err = deflate(records.Bytes(), flate.HuffmanOnly); err != nil {
			return res, err
		}
	}
	res.frame = frame
	return res, nil
}

func deflate(records []byte, level int) ([]byte, error) {
	// Room for stored blocks, deflate's worst case.
	buf := bytes.NewBuffer(make([]byte, 0, len(frameMagic)+len(records)+len(records)/64+64))
	buf.WriteString(frameMagic)
	zw, err := flate.NewWriter(buf, level)
	if err != nil {
		return nil, err
	}
	zw.Write(records)
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
