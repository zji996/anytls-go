//go:build linux && dedupimages && nodynamic && webpnative && cgo

package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	stdimage "image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Run with IMAGE_MEMORY_MEASURE=1 and -run '^TestImageNativeMemory$' -v.
// Each codec run gets a fresh process; fixture generation is outside its RSS.
func TestImageNativeMemory(t *testing.T) {
	if path := os.Getenv("IMAGE_MEMORY_INPUT"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		in := base64.StdEncoding.AppendEncode(nil, raw)
		raw = nil
		debug.FreeOSMemory()
		rss := func(field string) uint64 {
			status, err := os.ReadFile("/proc/self/status")
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(status), "\n") {
				if strings.HasPrefix(line, field+":") {
					n, _ := strconv.ParseUint(strings.Fields(line)[1], 10, 64)
					return n * 1024
				}
			}
			t.Fatalf("missing %s", field)
			return 0
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		baseGo := m.Sys - m.HeapReleased
		baseRSS := rss("VmRSS")
		peakGo := baseGo
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				runtime.ReadMemStats(&m)
				peakGo = max(peakGo, m.Sys-m.HeapReleased)
				select {
				case <-stop:
					return
				case <-tick.C:
				}
			}
		}()
		transcode, _ := webpTranscoder(85)
		out, err := transcode(in)
		close(stop)
		wg.Wait()
		if err != nil {
			t.Fatal(err)
		}
		runtime.KeepAlive(in)
		runtime.KeepAlive(out)
		t.Logf("12 MP: RSS peak delta %.2f B/pixel, Go committed peak delta %.2f B/pixel (baseline RSS %d)",
			float64(rss("VmHWM")-baseRSS)/12e6, float64(peakGo-baseGo)/12e6, baseRSS)
		return
	}
	if os.Getenv("IMAGE_MEMORY_MEASURE") == "" {
		t.Skip("opt-in native RSS measurement")
	}
	for _, kind := range []string{"rgba", "gray", "paletted"} {
		t.Run(kind, func(t *testing.T) {
			var raw []byte
			if kind == "rgba" {
				raw = screenshot(4000, 3000)
			} else {
				var img stdimage.Image
				if kind == "gray" {
					g := stdimage.NewGray(stdimage.Rect(0, 0, 4000, 3000))
					for y := 0; y < 3000; y++ {
						for x := 0; x < 4000; x++ {
							g.SetGray(x, y, color.Gray{Y: uint8(x*3 + y*5)})
						}
					}
					img = g
				} else {
					p := stdimage.NewPaletted(stdimage.Rect(0, 0, 4000, 3000), color.Palette{color.Black, color.White, color.RGBA{200, 30, 40, 255}})
					for y := 0; y < 3000; y++ {
						for x := 0; x < 4000; x++ {
							p.SetColorIndex(x, y, uint8((x/7+y/5)%3))
						}
					}
					img = p
				}
				var buf bytes.Buffer
				if err := png.Encode(&buf, img); err != nil {
					t.Fatal(err)
				}
				raw = buf.Bytes()
			}
			path := t.TempDir() + "/input.png"
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestImageNativeMemory$", "-test.v")
			cmd.Env = append(os.Environ(), "IMAGE_MEMORY_INPUT="+path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			fmt.Print(string(out))
		})
	}
}
