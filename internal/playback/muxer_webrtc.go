package playback

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/av1"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordstore"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/unit"
)

func scaleTimestamp(v, mul, div int64) int64 {
	secs := v / div
	dec := v % div
	return secs*mul + dec*mul/div
}

type muxerWebRTC struct {
	ctx       context.Context
	init      *fmp4.Init
	desc      *description.Session
	subStream *stream.SubStream
	startNTP  time.Time
	startWall time.Time // wall clock when playback started, for pacing

	trackMap   map[int]int // trackID -> index in desc.Medias / init.Tracks
	curTrackID int
}

func (m *muxerWebRTC) writeInit(_ *fmp4.Init) {
	// already stored at creation time
}

func (m *muxerWebRTC) setTrack(trackID int) {
	m.curTrackID = trackID
}

func (m *muxerWebRTC) writeSample(
	dts int64,
	ptsOffset int32,
	_ bool,
	_ uint32,
	getPayload func() ([]byte, error),
) error {
	select {
	case <-m.ctx.Done():
		return m.ctx.Err()
	default:
	}

	idx, ok := m.trackMap[m.curTrackID]
	if !ok {
		return nil // skip unknown tracks
	}

	track := m.init.Tracks[idx]
	media := m.desc.Medias[idx]
	forma := media.Formats[0]

	// Calculate timing
	dtsDur := durationMp4ToGo(dts, track.TimeScale)

	// Pace output at real-time speed for non-preroll frames (dts >= 0)
	if dts > 0 {
		targetTime := m.startWall.Add(dtsDur)
		sleepDur := time.Until(targetTime)
		if sleepDur > 0 {
			select {
			case <-time.After(sleepDur):
			case <-m.ctx.Done():
				return m.ctx.Err()
			}
		}
	}

	payload, err := getPayload()
	if err != nil {
		return err
	}

	unitPayload, err := fmp4SampleToUnitPayload(track.Codec, payload)
	if err != nil {
		return nil // skip malformed samples
	}

	// Convert DTS+PTS offset from track timescale to format clock rate
	pts := scaleTimestamp(dts+int64(ptsOffset), int64(forma.ClockRate()), int64(track.TimeScale))

	ntp := m.startNTP.Add(dtsDur)

	m.subStream.WriteUnit(media, forma, &unit.Unit{
		PTS:     pts,
		NTP:     ntp,
		Payload: unitPayload,
	})

	return nil
}

func (m *muxerWebRTC) writeFinalDTS(_ int64) {
	// no-op for WebRTC streaming
}

func (m *muxerWebRTC) flush() error {
	return nil
}

func fmp4SampleToUnitPayload(codec mcodecs.Codec, payload []byte) (unit.Payload, error) {
	switch codec.(type) {
	case *mcodecs.H264:
		var avcc h264.AVCC
		err := avcc.Unmarshal(payload)
		if err != nil {
			return nil, err
		}
		return unit.PayloadH264(avcc), nil

	case *mcodecs.H265:
		var avcc h264.AVCC
		err := avcc.Unmarshal(payload)
		if err != nil {
			return nil, err
		}
		return unit.PayloadH265(avcc), nil

	case *mcodecs.AV1:
		var bs av1.Bitstream
		err := bs.Unmarshal(payload)
		if err != nil {
			return nil, err
		}
		return unit.PayloadAV1(bs), nil

	case *mcodecs.VP9:
		return unit.PayloadVP9(payload), nil

	case *mcodecs.Opus:
		return unit.PayloadOpus([][]byte{payload}), nil

	case *mcodecs.MPEG4Audio:
		return unit.PayloadMPEG4Audio([][]byte{payload}), nil

	case *mcodecs.LPCM:
		return unit.PayloadLPCM(payload), nil

	default:
		return nil, fmt.Errorf("unsupported codec: %T", codec)
	}
}

// PreparePlayback reads the first segment's init data and creates a description.
func PreparePlayback(segments []*recordstore.Segment) (*fmp4.Init, *description.Session, error) {
	f, err := os.Open(segments[0].Fpath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	init, _, _, err := segmentFMP4ReadHeader(f)
	if err != nil {
		return nil, nil, err
	}

	desc := FMP4InitToDescription(init)
	if len(desc.Medias) == 0 {
		return nil, nil, fmt.Errorf("no supported tracks found in recording")
	}

	return init, desc, nil
}

// StreamPlaybackToSubStream reads fmp4 segments and pushes samples through
// a SubStream at real-time pace. It blocks until playback is complete,
// the context is cancelled, or an error occurs.
func StreamPlaybackToSubStream(
	ctx context.Context,
	recordFormat conf.RecordFormat,
	segments []*recordstore.Segment,
	start time.Time,
	duration time.Duration,
	desc *description.Session,
	subStream *stream.SubStream,
	init *fmp4.Init,
) error {
	if recordFormat != conf.RecordFormatFMP4 {
		return fmt.Errorf("only fmp4 recording format is supported for WebRTC playback")
	}

	// Build trackID -> index map
	trackMap := make(map[int]int, len(init.Tracks))
	for i, track := range init.Tracks {
		trackMap[track.ID] = i
	}

	m := &muxerWebRTC{
		ctx:       ctx,
		init:      init,
		desc:      desc,
		subStream: subStream,
		startNTP:  start,
		startWall: time.Now(),
		trackMap:  trackMap,
	}

	return seekAndStream(recordFormat, segments, start, duration, m, init)
}

// seekAndStream is similar to seekAndMux but designed for streaming playback.
// It passes a context-aware muxer and handles segment iteration.
func seekAndStream(
	recordFormat conf.RecordFormat,
	segments []*recordstore.Segment,
	start time.Time,
	duration time.Duration,
	m *muxerWebRTC,
	firstInit *fmp4.Init,
) error {
	f, err := os.Open(segments[0].Fpath)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}
	firstFileSize := fi.Size()

	_, firstTotalDuration, firstMoovEnd, err := segmentFMP4ReadHeader(f)
	if err != nil {
		return err
	}

	m.writeInit(&fmp4.Init{
		Tracks: firstInit.Tracks,
	})

	firstMtxi := findMtxi(firstInit.UserData)
	startOffset := segments[0].Start.Sub(start) // this is negative
	dts := startOffset

	segmentDuration, err := segmentFMP4MuxParts(f, dts, duration, firstInit.Tracks, m,
		firstMoovEnd, firstTotalDuration, firstFileSize)
	if err != nil {
		if m.ctx.Err() != nil {
			return m.ctx.Err()
		}
		return err
	}

	segmentEnd := segments[0].Start.Add(segmentDuration)
	prevInit := firstInit

	for _, seg := range segments[1:] {
		f, err = os.Open(seg.Fpath)
		if err != nil {
			return err
		}
		defer f.Close()

		var init *fmp4.Init
		init, _, _, err = segmentFMP4ReadHeader(f)
		if err != nil {
			return err
		}

		if !segmentFMP4CanBeConcatenated(prevInit, segmentEnd, init, seg.Start) {
			break
		}

		if firstMtxi != nil {
			mtxi := findMtxi(init.UserData)
			dts = time.Duration(mtxi.DTS-firstMtxi.DTS) + startOffset
		} else {
			dts = seg.Start.Sub(start)
		}

		segmentDuration, err = segmentFMP4MuxParts(f, dts, duration, firstInit.Tracks, m, 0, 0, 0)
		if err != nil {
			if m.ctx.Err() != nil {
				return m.ctx.Err()
			}
			return err
		}

		segmentEnd = seg.Start.Add(segmentDuration)
		prevInit = init
	}

	return m.flush()
}
