package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

// Recordings are fragmented MP4 files. To let a viewer jump to any moment
// without downloading a whole file, each one is also offered as an HLS
// playlist whose segments are byte ranges of that same file.

type vodSeg struct {
	Off, Len int64
	Dur      float64 // seconds
}

type vodIndex struct {
	InitLen int64 // ftyp+moov at the start of the file
	Segs    []vodSeg
}

type mp4Box struct {
	Type      string
	Off, Size int64 // whole box
	Hdr       int64 // header length
}

// readBox reads the box header at off; ok is false at a clean end of data.
func readBox(r io.ReaderAt, off, limit int64) (b mp4Box, ok bool) {
	var h [16]byte
	if off+8 > limit {
		return b, false
	}
	if _, err := r.ReadAt(h[:8], off); err != nil {
		return b, false
	}
	b = mp4Box{Type: string(h[4:8]), Off: off, Size: int64(binary.BigEndian.Uint32(h[:4])), Hdr: 8}
	switch b.Size {
	case 0:
		b.Size = limit - off
	case 1:
		if _, err := r.ReadAt(h[8:16], off+8); err != nil {
			return b, false
		}
		b.Size, b.Hdr = int64(binary.BigEndian.Uint64(h[8:16])), 16
	}
	if b.Size < b.Hdr {
		return b, false
	}
	return b, true
}

// children lists the boxes directly inside a box held in memory.
func children(buf []byte) [][2]int { // each child as [start, end) offsets into buf
	var out [][2]int
	for p := 0; p+8 <= len(buf); {
		size := int(binary.BigEndian.Uint32(buf[p:]))
		if size < 8 || p+size > len(buf) {
			break
		}
		out = append(out, [2]int{p, p + size})
		p += size
	}
	return out
}

func boxType(buf []byte, c [2]int) string { return string(buf[c[0]+4 : c[0]+8]) }
func payload(buf []byte, c [2]int) []byte { return buf[c[0]+8 : c[1]] }

// videoTrack finds the video track's id and timescale in a moov payload.
func videoTrack(moov []byte) (id, timescale uint32) {
	for _, t := range children(moov) {
		if boxType(moov, t) != "trak" {
			continue
		}
		var tid, ts uint32
		var handler string
		trak := payload(moov, t)
		for _, c := range children(trak) {
			p := payload(trak, c)
			switch boxType(trak, c) {
			case "tkhd":
				if len(p) >= 24 && p[0] == 1 {
					tid = binary.BigEndian.Uint32(p[20:])
				} else if len(p) >= 16 {
					tid = binary.BigEndian.Uint32(p[12:])
				}
			case "mdia":
				for _, m := range children(p) {
					q := payload(p, m)
					switch boxType(p, m) {
					case "mdhd":
						if len(q) >= 24 && q[0] == 1 {
							ts = binary.BigEndian.Uint32(q[20:])
						} else if len(q) >= 16 {
							ts = binary.BigEndian.Uint32(q[12:])
						}
					case "hdlr":
						if len(q) >= 12 {
							handler = string(q[8:12])
						}
					}
				}
			}
		}
		if id == 0 || handler == "vide" {
			id, timescale = tid, ts
		}
		if handler == "vide" {
			break
		}
	}
	return id, timescale
}

// fragTime returns the decode time of the given track in a moof payload.
func fragTime(moof []byte, track uint32) (uint64, bool) {
	for _, t := range children(moof) {
		if boxType(moof, t) != "traf" {
			continue
		}
		traf := payload(moof, t)
		var tid uint32
		var dt uint64
		var has bool
		for _, c := range children(traf) {
			p := payload(traf, c)
			switch boxType(traf, c) {
			case "tfhd":
				if len(p) >= 8 {
					tid = binary.BigEndian.Uint32(p[4:])
				}
			case "tfdt":
				if len(p) >= 12 && p[0] == 1 {
					dt, has = binary.BigEndian.Uint64(p[4:]), true
				} else if len(p) >= 8 {
					dt, has = uint64(binary.BigEndian.Uint32(p[4:])), true
				}
			}
		}
		if has && tid == track {
			return dt, true
		}
	}
	return 0, false
}

const vodTargetSec = 4 // fragments are grouped into segments of about this length

// indexFMP4 maps a recording's fragments. It only uses complete boxes, so it
// also works on the file that is still being written.
func indexFMP4(path string) (*vodIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()

	type frag struct {
		off, end int64
		t        uint64
		hasT     bool
	}
	var frags []frag
	var track, timescale uint32
	idx := &vodIndex{}
	for off := int64(0); ; {
		b, ok := readBox(f, off, size)
		if !ok || b.Off+b.Size > size {
			break // incomplete tail
		}
		switch b.Type {
		case "moov":
			if b.Size > 8<<20 {
				return nil, errors.New("moov too large")
			}
			buf := make([]byte, b.Size-b.Hdr)
			if _, err := f.ReadAt(buf, b.Off+b.Hdr); err != nil {
				return nil, err
			}
			track, timescale = videoTrack(buf)
		case "moof":
			if idx.InitLen == 0 {
				idx.InitLen = b.Off
			}
			if b.Size > 8<<20 {
				return nil, errors.New("moof too large")
			}
			buf := make([]byte, b.Size-b.Hdr)
			if _, err := f.ReadAt(buf, b.Off+b.Hdr); err != nil {
				return nil, err
			}
			fr := frag{off: b.Off}
			fr.t, fr.hasT = fragTime(buf, track)
			frags = append(frags, fr)
		case "mdat":
			if n := len(frags); n > 0 {
				frags[n-1].end = b.Off + b.Size
			}
		}
		off = b.Off + b.Size
	}
	for len(frags) > 0 && frags[len(frags)-1].end == 0 {
		frags = frags[:len(frags)-1] // moof whose data is not written yet
	}
	if idx.InitLen == 0 || len(frags) == 0 || timescale == 0 {
		return nil, errors.New("no complete fragments")
	}

	durs := make([]float64, len(frags))
	for i := range frags {
		if i+1 < len(frags) && frags[i].hasT && frags[i+1].hasT && frags[i+1].t > frags[i].t {
			durs[i] = float64(frags[i+1].t-frags[i].t) / float64(timescale)
		} else if i > 0 {
			durs[i] = durs[i-1] // last fragment: assume it is like the one before
		} else {
			durs[i] = 2
		}
	}
	for i := 0; i < len(frags); {
		s := vodSeg{Off: frags[i].off}
		for {
			s.Dur += durs[i]
			s.Len = frags[i].end - s.Off
			i++
			if i >= len(frags) || s.Dur >= vodTargetSec {
				break
			}
		}
		idx.Segs = append(idx.Segs, s)
	}
	return idx, nil
}

// m3u8 renders the index as a playlist next to the recording called file.
func (x *vodIndex) m3u8(file string) string {
	maxDur := 1.0
	for _, s := range x.Segs {
		maxDur = math.Max(maxDur, s.Dur)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-INDEPENDENT-SEGMENTS\n", int(math.Ceil(maxDur)))
	fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\",BYTERANGE=\"%d@0\"\n", file, x.InitLen)
	for _, s := range x.Segs {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n#EXT-X-BYTERANGE:%d@%d\n%s\n", s.Dur, s.Len, s.Off, file)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// vodCache keeps the index of files that are no longer changing.
type vodCache struct {
	mu sync.Mutex
	m  map[string]vodCached
}

type vodCached struct {
	size int64
	mod  time.Time
	idx  *vodIndex
}

func (c *vodCache) get(path string) (*vodIndex, error) {
	fi, err := statOpen(path)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	e, ok := c.m[path]
	c.mu.Unlock()
	if ok && e.size == fi.Size() && e.mod.Equal(fi.ModTime()) {
		return e.idx, nil
	}
	idx, err := indexFMP4(path)
	if err != nil {
		return nil, err
	}
	if time.Since(fi.ModTime()) > time.Minute { // finished file
		c.mu.Lock()
		if c.m == nil || len(c.m) > 500 {
			c.m = map[string]vodCached{}
		}
		c.m[path] = vodCached{fi.Size(), fi.ModTime(), idx}
		c.mu.Unlock()
	}
	return idx, nil
}
