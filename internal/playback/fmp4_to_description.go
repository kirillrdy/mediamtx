package playback

import (
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

// FMP4InitToDescription converts an fmp4.Init to a description.Session
// by mapping each track's codec to the corresponding format type.
func FMP4InitToDescription(init *fmp4.Init) *description.Session {
	var medias []*description.Media

	for _, track := range init.Tracks {
		switch codec := track.Codec.(type) {
		case *mcodecs.AV1:
			medias = append(medias, &description.Media{
				Type: description.MediaTypeVideo,
				Formats: []format.Format{&format.AV1{
					PayloadTyp: 96,
				}},
			})

		case *mcodecs.VP9:
			medias = append(medias, &description.Media{
				Type: description.MediaTypeVideo,
				Formats: []format.Format{&format.VP9{
					PayloadTyp: 96,
				}},
			})

		case *mcodecs.H265:
			medias = append(medias, &description.Media{
				Type: description.MediaTypeVideo,
				Formats: []format.Format{&format.H265{
					PayloadTyp: 96,
					VPS:        codec.VPS,
					SPS:        codec.SPS,
					PPS:        codec.PPS,
				}},
			})

		case *mcodecs.H264:
			medias = append(medias, &description.Media{
				Type: description.MediaTypeVideo,
				Formats: []format.Format{&format.H264{
					PayloadTyp:        96,
					PacketizationMode: 1,
					SPS:               codec.SPS,
					PPS:               codec.PPS,
				}},
			})

		case *mcodecs.Opus:
			medias = append(medias, &description.Media{
				Type: description.MediaTypeAudio,
				Formats: []format.Format{&format.Opus{
					PayloadTyp:   96,
					ChannelCount: codec.ChannelCount,
				}},
			})

		case *mcodecs.MPEG4Audio:
			medias = append(medias, &description.Media{
				Type: description.MediaTypeAudio,
				Formats: []format.Format{&format.MPEG4Audio{
					PayloadTyp:       96,
					SizeLength:       13,
					IndexLength:      3,
					IndexDeltaLength: 3,
					Config: &mpeg4audio.AudioSpecificConfig{
						Type:          codec.Config.Type,
						SampleRate:    codec.Config.SampleRate,
						ChannelConfig: codec.Config.ChannelConfig,
						ChannelCount:  codec.Config.ChannelCount, //nolint:staticcheck
					},
				}},
			})

		case *mcodecs.LPCM:
			medias = append(medias, &description.Media{
				Type: description.MediaTypeAudio,
				Formats: []format.Format{&format.LPCM{
					PayloadTyp:   96,
					BitDepth:     codec.BitDepth,
					SampleRate:   codec.SampleRate,
					ChannelCount: codec.ChannelCount,
				}},
			})
		}
	}

	return &description.Session{Medias: medias}
}
