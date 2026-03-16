package playback

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediamtx/internal/recordstore"
)

const (
	sampleFlagIsNonSyncSample = 1 << 16
	concatenationTolerance    = 1 * time.Second
	// gopPreroll is the amount of video before the seek point that is always
	// fully processed to ensure the pre-seek GOP is available for clean decode.
	// Must be >= the maximum keyframe interval used by any source.
	gopPreroll = 10 * time.Second
)

var errTerminated = errors.New("terminated")

type readSeekerAt interface {
	io.Reader
	io.Seeker
	io.ReaderAt
}

func durationGoToMp4(v time.Duration, timeScale uint32) int64 {
	timeScale64 := int64(timeScale)
	secs := v / time.Second
	dec := v % time.Second
	return int64(secs)*timeScale64 + int64(dec)*timeScale64/int64(time.Second)
}

func durationMp4ToGo(v int64, timeScale uint32) time.Duration {
	timeScale64 := int64(timeScale)
	secs := v / timeScale64
	dec := v % timeScale64
	return time.Duration(secs)*time.Second + time.Duration(dec)*time.Second/time.Duration(timeScale64)
}

func findInitTrack(tracks []*fmp4.InitTrack, id int) *fmp4.InitTrack {
	for _, track := range tracks {
		if track.ID == id {
			return track
		}
	}
	return nil
}

func findMtxi(userData []amp4.IBox) *recordstore.Mtxi {
	for _, box := range userData {
		if i, ok := box.(*recordstore.Mtxi); ok {
			return i
		}
	}
	return nil
}

func segmentFMP4TracksAreEqual(tracks1 []*fmp4.InitTrack, tracks2 []*fmp4.InitTrack) bool {
	if len(tracks1) != len(tracks2) {
		return false
	}

	for i, track1 := range tracks1 {
		track2 := tracks2[i]

		if track1.ID != track2.ID ||
			track1.TimeScale != track2.TimeScale ||
			reflect.TypeOf(track1.Codec) != reflect.TypeOf(track2.Codec) {
			return false
		}
	}

	return true
}

func segmentFMP4CanBeConcatenated(
	prevInit *fmp4.Init,
	prevEnd time.Time,
	curInit *fmp4.Init,
	curStart time.Time,
) bool {
	mtxi1 := findMtxi(prevInit.UserData)
	mtxi2 := findMtxi(curInit.UserData)

	switch {
	case mtxi1 == nil && mtxi2 != nil:
		return false

	case mtxi1 != nil && mtxi2 == nil:
		return false

	case mtxi1 == nil && mtxi2 == nil: // legacy method
		return segmentFMP4TracksAreEqual(prevInit.Tracks, curInit.Tracks) &&
			!curStart.Before(prevEnd.Add(-concatenationTolerance)) &&
			!curStart.After(prevEnd.Add(concatenationTolerance))

	default:
		return bytes.Equal(mtxi1.StreamID[:], mtxi2.StreamID[:]) &&
			(mtxi1.SegmentNumber+1) == mtxi2.SegmentNumber
	}
}

func segmentFMP4ReadHeader(r io.ReadSeeker) (*fmp4.Init, time.Duration, int64, error) {
	// check and skip ftyp

	buf := make([]byte, 8)
	_, err := io.ReadFull(r, buf)
	if err != nil {
		return nil, 0, 0, err
	}

	if !bytes.Equal(buf[4:], []byte{'f', 't', 'y', 'p'}) {
		return nil, 0, 0, fmt.Errorf("ftyp box not found")
	}

	ftypSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

	_, err = r.Seek(int64(ftypSize), io.SeekStart)
	if err != nil {
		return nil, 0, 0, err
	}

	// check moov

	_, err = io.ReadFull(r, buf)
	if err != nil {
		return nil, 0, 0, err
	}

	if !bytes.Equal(buf[4:], []byte{'m', 'o', 'o', 'v'}) {
		return nil, 0, 0, fmt.Errorf("moov box not found")
	}

	moovSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

	// skip moov header

	_, err = r.Seek(8, io.SeekCurrent)
	if err != nil {
		return nil, 0, 0, err
	}

	// read mvhd

	var mvhd amp4.Mvhd
	_, err = amp4.Unmarshal(r, uint64(moovSize-8), &mvhd, amp4.Context{})
	if err != nil {
		return nil, 0, 0, err
	}

	d := time.Duration(mvhd.DurationV0) * time.Second / time.Duration(mvhd.Timescale)

	// read ftyp and moov

	_, err = r.Seek(0, io.SeekStart)
	if err != nil {
		return nil, 0, 0, err
	}

	buf = make([]byte, uint64(ftypSize+moovSize))

	_, err = io.ReadFull(r, buf)
	if err != nil {
		return nil, 0, 0, err
	}

	// pass ftyp and moov to fmp4.Init

	var init fmp4.Init
	err = init.Unmarshal(bytes.NewReader(buf))
	if err != nil {
		return nil, 0, 0, err
	}

	return &init, d, int64(ftypSize) + int64(moovSize), nil
}

func segmentFMP4ReadDurationFromParts(
	r io.ReadSeeker,
	init *fmp4.Init,
) (time.Duration, error) {
	_, err := r.Seek(0, io.SeekStart)
	if err != nil {
		return 0, err
	}

	// check and skip ftyp

	buf := make([]byte, 8)
	_, err = io.ReadFull(r, buf)
	if err != nil {
		return 0, err
	}

	if !bytes.Equal(buf[4:], []byte{'f', 't', 'y', 'p'}) {
		return 0, fmt.Errorf("ftyp box not found")
	}

	ftypSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

	_, err = r.Seek(int64(ftypSize), io.SeekStart)
	if err != nil {
		return 0, err
	}

	// check and skip moov

	_, err = io.ReadFull(r, buf)
	if err != nil {
		return 0, err
	}

	if !bytes.Equal(buf[4:], []byte{'m', 'o', 'o', 'v'}) {
		return 0, fmt.Errorf("moov box not found")
	}

	moovSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

	_, err = r.Seek(int64(moovSize)-8, io.SeekCurrent)
	if err != nil {
		return 0, err
	}

	// find last valid moof and mdat

	lastMoofPos := int64(-1)

	for {
		var moofPos int64
		moofPos, err = r.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, err
		}

		_, err = io.ReadFull(r, buf)
		if err != nil {
			break
		}

		if !bytes.Equal(buf[4:], []byte{'m', 'o', 'o', 'f'}) {
			break
		}

		moofSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

		_, err = r.Seek(int64(moofSize)-8, io.SeekCurrent)
		if err != nil {
			break
		}

		_, err = io.ReadFull(r, buf)
		if err != nil {
			break
		}

		if !bytes.Equal(buf[4:], []byte{'m', 'd', 'a', 't'}) {
			break
		}

		mdatSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

		_, err = r.Seek(int64(mdatSize)-8, io.SeekCurrent)
		if err != nil {
			break
		}

		lastMoofPos = moofPos
	}

	if lastMoofPos < 0 {
		return 0, fmt.Errorf("no moof boxes found")
	}

	// open last moof

	_, err = r.Seek(lastMoofPos+8, io.SeekStart)
	if err != nil {
		return 0, err
	}

	_, err = io.ReadFull(r, buf)
	if err != nil {
		return 0, err
	}

	// skip mfhd

	if !bytes.Equal(buf[4:], []byte{'m', 'f', 'h', 'd'}) {
		return 0, fmt.Errorf("mfhd box not found")
	}

	_, err = r.Seek(8, io.SeekCurrent)
	if err != nil {
		return 0, err
	}

	var maxElapsed time.Duration

	// foreach traf

outer:
	for {
		_, err = io.ReadFull(r, buf)
		if err != nil {
			return 0, err
		}

		switch {
		case bytes.Equal(buf[4:], []byte{'t', 'r', 'a', 'f'}):
		case bytes.Equal(buf[4:], []byte{'m', 'd', 'a', 't'}):
			break outer
		default:
			return 0, fmt.Errorf("unexpected box %x", buf[4:8])
		}

		// parse tfhd

		_, err = io.ReadFull(r, buf)
		if err != nil {
			return 0, err
		}

		if !bytes.Equal(buf[4:], []byte{'t', 'f', 'h', 'd'}) {
			return 0, fmt.Errorf("tfhd box not found")
		}

		tfhdSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

		buf2 := make([]byte, tfhdSize-8)

		_, err = io.ReadFull(r, buf2)
		if err != nil {
			return 0, err
		}

		var tfhd amp4.Tfhd
		_, err = amp4.Unmarshal(bytes.NewReader(buf2), uint64(len(buf2)), &tfhd, amp4.Context{})
		if err != nil {
			return 0, fmt.Errorf("invalid tfhd box: %w", err)
		}

		track := findInitTrack(init.Tracks, int(tfhd.TrackID))
		if track == nil {
			return 0, fmt.Errorf("invalid track ID: %v", tfhd.TrackID)
		}

		// parse tfdt

		_, err = io.ReadFull(r, buf)
		if err != nil {
			return 0, err
		}

		if !bytes.Equal(buf[4:], []byte{'t', 'f', 'd', 't'}) {
			return 0, fmt.Errorf("tfdt box not found")
		}

		tfdtSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

		buf2 = make([]byte, tfdtSize-8)

		_, err = io.ReadFull(r, buf2)
		if err != nil {
			return 0, err
		}

		var tfdt amp4.Tfdt
		_, err = amp4.Unmarshal(bytes.NewReader(buf2), uint64(len(buf2)), &tfdt, amp4.Context{})
		if err != nil {
			return 0, fmt.Errorf("invalid tfdt box: %w", err)
		}

		// parse trun

		_, err = io.ReadFull(r, buf)
		if err != nil {
			return 0, err
		}

		if !bytes.Equal(buf[4:], []byte{'t', 'r', 'u', 'n'}) {
			return 0, fmt.Errorf("trun box not found")
		}

		trunSize := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])

		buf2 = make([]byte, trunSize-8)

		_, err = io.ReadFull(r, buf2)
		if err != nil {
			return 0, err
		}

		var trun amp4.Trun
		_, err = amp4.Unmarshal(bytes.NewReader(buf2), uint64(len(buf2)), &trun, amp4.Context{})
		if err != nil {
			return 0, fmt.Errorf("invalid trun box: %w", err)
		}

		elapsed := int64(tfdt.BaseMediaDecodeTimeV1)

		for _, entry := range trun.Entries {
			elapsed += int64(entry.SampleDuration)
		}

		elapsedGo := durationMp4ToGo(elapsed, track.TimeScale)

		if elapsedGo > maxElapsed {
			maxElapsed = elapsedGo
		}
	}

	return maxElapsed, nil
}

// fmp4WalkBoxes iterates over MP4 child boxes within buf, calling fn for each.
// fn returns true to continue iteration, false to stop early.
func fmp4WalkBoxes(buf []byte, fn func(boxType string, payload []byte) bool) {
	for off := 0; off+8 <= len(buf); {
		sz := int(binary.BigEndian.Uint32(buf[off : off+4]))
		typ := string(buf[off+4 : off+8])
		hdrSz := 8

		if sz == 1 {
			if off+16 > len(buf) {
				return
			}
			sz64 := binary.BigEndian.Uint64(buf[off+8 : off+16])
			if sz64 > uint64(len(buf)) {
				return
			}
			sz = int(sz64)
			hdrSz = 16
		} else if sz == 0 {
			sz = len(buf) - off
		}

		if sz < hdrSz || off+sz > len(buf) {
			return
		}

		if !fn(typ, buf[off+hdrSz:off+sz]) {
			return
		}
		off += sz
	}
}

// fmp4ReadMoofTfdt extracts the base decode time and track timescale from the
// first traf/tfdt found within a moof box body read via r.ReadAt.
func fmp4ReadMoofTfdt(r io.ReaderAt, bodyStart, bodyEnd int64, tracks []*fmp4.InitTrack) (baseDTS int64, timeScale uint32, ok bool) {
	bodySz := bodyEnd - bodyStart
	if bodySz <= 0 || bodySz > 1<<20 { // moofs are small; >1MB is malformed
		return 0, 0, false
	}
	body := make([]byte, bodySz)
	if _, err := r.ReadAt(body, bodyStart); err != nil {
		return 0, 0, false
	}

	fmp4WalkBoxes(body, func(t string, p []byte) bool {
		if t != "traf" {
			return true
		}
		var trackID uint32
		fmp4WalkBoxes(p, func(t2 string, p2 []byte) bool {
			switch t2 {
			case "tfhd":
				// FullBox: version(1)+flags(3)+trackID(4)
				if len(p2) >= 8 {
					trackID = binary.BigEndian.Uint32(p2[4:8])
				}
			case "tfdt":
				if len(p2) < 4 {
					return false
				}
				track := findInitTrack(tracks, int(trackID))
				if track == nil {
					return false
				}
				version := p2[0]
				if version == 1 && len(p2) >= 12 {
					baseDTS = int64(binary.BigEndian.Uint64(p2[4:12]))
					timeScale = track.TimeScale
					ok = true
				} else if version == 0 && len(p2) >= 8 {
					baseDTS = int64(binary.BigEndian.Uint32(p2[4:8]))
					timeScale = track.TimeScale
					ok = true
				}
				return false // stop after first tfdt
			}
			return !ok
		})
		return !ok
	})
	return
}

// fmp4LinearScanFrom scans boxes starting at fromOffset (using ReadAt) and
// returns the file offset of the first moof whose fragment DTS satisfies the
// gopPreroll condition relative to startDTS. Returns 0 if no such moof is
// found or on any read error.
func fmp4LinearScanFrom(r io.ReaderAt, fromOffset uint64, startDTS time.Duration, tracks []*fmp4.InitTrack) uint64 {
	off := int64(fromOffset)
	hdr := [16]byte{}

	for {
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return 0
		}

		boxSz := int64(binary.BigEndian.Uint32(hdr[0:4]))
		boxType := string(hdr[4:8])
		hdrSz := int64(8)

		if boxSz == 1 {
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return 0
			}
			boxSz = int64(binary.BigEndian.Uint64(hdr[8:16]))
			hdrSz = 16
		} else if boxSz == 0 {
			return 0
		}

		if boxSz < hdrSz {
			return 0
		}

		if boxType == "moof" {
			baseDTS, timeScale, ok := fmp4ReadMoofTfdt(r, off+hdrSz, off+boxSz, tracks)
			if ok {
				fragDTS := durationMp4ToGo(baseDTS, timeScale) + startDTS
				if fragDTS >= -gopPreroll {
					return uint64(off)
				}
			}
		}

		off += boxSz
	}
}

// fmp4FindNearestMoofAfter reads a 2 MiB buffer starting at pos and returns
// the absolute file offset of the first plausible moof box found within it.
// Returns 0 if no moof is found.
func fmp4FindNearestMoofAfter(r io.ReaderAt, pos int64) int64 {
	const bufSize = 2 << 20 // 2 MiB
	buf := make([]byte, bufSize)
	n, _ := r.ReadAt(buf, pos)
	if n < 8 {
		return 0
	}
	buf = buf[:n]

	moofMark := []byte("moof")
	for off := 0; off+8 <= len(buf); {
		idx := bytes.Index(buf[off:], moofMark)
		if idx < 0 {
			return 0
		}
		absIdx := off + idx
		// The box size field is the 4 bytes immediately before "moof".
		if absIdx >= 4 {
			sz := binary.BigEndian.Uint32(buf[absIdx-4 : absIdx])
			if sz >= 100 && sz <= 1_000_000 {
				return pos + int64(absIdx-4)
			}
		}
		off = absIdx + 1
	}
	return 0
}

// segmentFMP4FindSkipOffset returns the file offset of the first moof box that
// falls within the GOP pre-roll window before startDTS. All moof boxes before
// this offset are outside the relevant time range and can be skipped in the
// ReadBoxStructure pass, reducing seek latency from O(segment_duration) to
// O(gopPreroll).
//
// When moovEnd, totalDuration, and fileSize are all non-zero the function uses
// linear interpolation to jump near the target position (O(1) I/O) and then
// scans only a small number of fragments. If those values are unknown (zero)
// it falls back to a linear scan from the beginning of the file.
//
// Returns 0 when no moofs can be skipped (full scan required).
func segmentFMP4FindSkipOffset(
	r io.ReaderAt,
	startDTS time.Duration,
	tracks []*fmp4.InitTrack,
	moovEnd int64,
	totalDuration time.Duration,
	fileSize int64,
) uint64 {
	targetRelTime := -startDTS - gopPreroll
	if targetRelTime <= 0 {
		return 0
	}

	// Fast path: use linear interpolation to jump near the target position
	// instead of scanning the whole file one fragment at a time.
	if totalDuration > 0 && moovEnd > 0 && fileSize > moovEnd {
		dataSize := float64(fileSize - moovEnd)
		frac := float64(targetRelTime) / float64(totalDuration)
		if frac < 1.0 {
			// Back off by 2× gopPreroll worth of estimated bytes so we are
			// guaranteed to start before the required pre-roll window even
			// when the bitrate estimate is slightly off.
			margin := 2.0 * float64(gopPreroll) / float64(totalDuration) * dataSize
			safeOffset := int64(float64(moovEnd) + frac*dataSize - margin)
			if safeOffset > moovEnd {
				if moofOff := fmp4FindNearestMoofAfter(r, safeOffset); moofOff > 0 {
					if skipOff := fmp4LinearScanFrom(r, uint64(moofOff), startDTS, tracks); skipOff > 0 {
						return skipOff
					}
				}
			}
		}
	}

	// Slow path: linear scan from the beginning of the file.
	return fmp4LinearScanFrom(r, 0, startDTS, tracks)
}

// seekInterceptor wraps a readSeekerAt and redirects the very first
// Seek(0, io.SeekStart) call — which amp4.ReadBoxStructure always issues
// before it starts reading — to seekTarget instead. This lets ReadBoxStructure
// start scanning from an arbitrary moof position without traversing all the
// preceding boxes on disk.
type seekInterceptor struct {
	readSeekerAt
	seekTarget int64
	done       bool
}

func (si *seekInterceptor) Seek(offset int64, whence int) (int64, error) {
	if !si.done && whence == io.SeekStart && offset == 0 {
		si.done = true
		return si.readSeekerAt.Seek(si.seekTarget, io.SeekStart)
	}
	return si.readSeekerAt.Seek(offset, whence)
}

func segmentFMP4MuxParts(
	r readSeekerAt,
	startDTS time.Duration,
	duration time.Duration,
	tracks []*fmp4.InitTrack,
	m muxer,
	moovEnd int64,
	totalDuration time.Duration,
	fileSize int64,
) (time.Duration, error) {
	skipBeforeOffset := segmentFMP4FindSkipOffset(r, startDTS, tracks, moovEnd, totalDuration, fileSize)

	// amp4.ReadBoxStructure always rewinds to position 0 before scanning,
	// so without intervention it would seek through every moof/mdat pair up
	// to skipBeforeOffset — the same page faults we just avoided in
	// segmentFMP4FindSkipOffset. Use seekInterceptor to redirect that initial
	// rewind to skipBeforeOffset so the library starts reading there directly.
	var rs io.ReadSeeker = r
	if skipBeforeOffset > 0 {
		rs = &seekInterceptor{readSeekerAt: r, seekTarget: int64(skipBeforeOffset)}
	}

	var startDTSMP4 int64
	var durationMP4 int64
	moofOffset := uint64(0)
	var tfhd *amp4.Tfhd
	var tfdt *amp4.Tfdt
	var timeScale uint32
	var segmentDuration time.Duration
	breakAtNextMdat := false

	_, err := amp4.ReadBoxStructure(rs, func(h *amp4.ReadHandle) (any, error) {
		switch h.BoxInfo.Type.String() {
		case "moof":
			if h.BoxInfo.Offset < skipBeforeOffset {
				return nil, nil
			}
			moofOffset = h.BoxInfo.Offset
			return h.Expand()

		case "traf":
			return h.Expand()

		case "tfhd":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tfhd = box.(*amp4.Tfhd)

		case "tfdt":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tfdt = box.(*amp4.Tfdt)

			track := findInitTrack(tracks, int(tfhd.TrackID))
			if track == nil {
				return nil, fmt.Errorf("invalid track ID: %v", tfhd.TrackID)
			}

			m.setTrack(int(tfhd.TrackID))
			timeScale = track.TimeScale
			startDTSMP4 = durationGoToMp4(startDTS, track.TimeScale)
			durationMP4 = durationGoToMp4(duration, track.TimeScale)

		case "trun":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			trun := box.(*amp4.Trun)

			dataOffset := moofOffset + uint64(trun.DataOffset)
			dts := int64(tfdt.BaseMediaDecodeTimeV1) + startDTSMP4

			for _, e := range trun.Entries {
				if dts >= durationMP4 {
					breakAtNextMdat = true
					break
				}

				sampleOffset := dataOffset
				sampleSize := e.SampleSize

				err = m.writeSample(
					dts,
					e.SampleCompositionTimeOffsetV1,
					(e.SampleFlags&sampleFlagIsNonSyncSample) != 0,
					e.SampleSize,
					func() ([]byte, error) {
						payload := make([]byte, sampleSize)
						n, err2 := r.ReadAt(payload, int64(sampleOffset))
						if err2 != nil {
							return nil, err2
						}
						if n != int(sampleSize) {
							return nil, fmt.Errorf("partial read")
						}

						return payload, nil
					},
				)
				if err != nil {
					return nil, err
				}

				dataOffset += uint64(e.SampleSize)
				dts += int64(e.SampleDuration)
			}

			m.writeFinalDTS(dts)

			segmentElapsed := durationMp4ToGo(dts-startDTSMP4, timeScale)

			if segmentElapsed > segmentDuration {
				segmentDuration = segmentElapsed
			}

		case "mdat":
			if breakAtNextMdat {
				return nil, errTerminated
			}
		}
		return nil, nil
	})
	if err != nil && !errors.Is(err, errTerminated) {
		return 0, err
	}

	return segmentDuration, nil
}
