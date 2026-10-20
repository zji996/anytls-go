package main

// Content-defined chunking with a gear rolling hash (FastCDC style).
// Boundaries depend on the last 64 bytes only, so an insertion near the start
// of a body moves the chunks around it and leaves the rest identical.

const (
	minChunk = 2 << 10
	avgChunk = 8 << 10
	maxChunk = 64 << 10
	// The mask tests the high bits: bit 63 of the gear hash covers the last 64 bytes.
	maskStrict = uint64(0xFFFE000000000000) // 15 bits before avgChunk
	maskLoose  = uint64(0xFFE0000000000000) // 11 bits after avgChunk
)

var gear [256]uint64

func init() {
	// Deterministic table; only the encoder chunks, so it never has to match a peer.
	x := uint64(0x9E3779B97F4A7C15)
	for i := range gear {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		gear[i] = z ^ (z >> 31)
	}
}

// cut returns the length of the next chunk at the start of data.
func cut(data []byte) int {
	n := len(data)
	if n <= minChunk {
		return n
	}
	if n > maxChunk {
		n = maxChunk
	}
	normal := avgChunk
	if normal > n {
		normal = n
	}
	var h uint64
	i := minChunk
	for ; i < normal; i++ {
		h = (h << 1) + gear[data[i]]
		if h&maskStrict == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		h = (h << 1) + gear[data[i]]
		if h&maskLoose == 0 {
			return i + 1
		}
	}
	return n
}

// chunks splits data into content-defined pieces that share its backing array.
func chunks(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		n := cut(data)
		out = append(out, data[:n])
		data = data[n:]
	}
	return out
}
