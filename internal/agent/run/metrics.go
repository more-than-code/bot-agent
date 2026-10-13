package run

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// maxMetricsBytes is the serialized metrics-frame target. It sits under the
// contract's 1 MiB cap so the envelope cannot push a full batch over the limit.
const maxMetricsBytes = 960 * 1024

type metricsFrame struct {
	id  string
	raw []byte
}

func (a *agent) emitMetrics(ctx context.Context, send func(context.Context, []byte) error, samples []protocol.Sample, now time.Time) error {
	frames, err := buildMetricsFrames(samples)
	if err != nil {
		return err
	}
	for _, fr := range frames {
		if err := a.spool.Put(fr.id, fr.raw, now); err != nil {
			a.opt.Log.Printf("bot-agent: spool drop")
		}
		if err := send(ctx, fr.raw); err != nil {
			return err
		}
	}
	return nil
}

func buildMetricsFrames(samples []protocol.Sample) ([]metricsFrame, error) {
	chunks, err := splitSamples(samples)
	if err != nil {
		return nil, err
	}
	out := make([]metricsFrame, 0, len(chunks))
	for _, chunk := range chunks {
		id := uuid.NewString()
		raw, err := protocol.Marshal(protocol.Metrics{
			V: protocol.Version, Type: "metrics", BatchID: id, Samples: chunk,
		})
		if err != nil {
			return nil, err
		}
		if len(raw) > protocol.MaxFrame {
			return nil, errors.New("metrics frame exceeds cap")
		}
		out = append(out, metricsFrame{id: id, raw: raw})
	}
	return out, nil
}

// splitSamples packs samples so each metrics frame stays within maxMetricsBytes
// and protocol.MaxSamples. A single sample that fits under the 1 MiB cap is
// kept even when it is larger than the headroom.
func splitSamples(samples []protocol.Sample) ([][]protocol.Sample, error) {
	if len(samples) == 0 {
		return nil, nil
	}
	sizes := make([]int, len(samples))
	for i, s := range samples {
		b, err := protocol.Marshal(s)
		if err != nil {
			return nil, err
		}
		sizes[i] = len(b)
	}
	empty, err := protocol.Marshal(protocol.Metrics{
		V: protocol.Version, Type: "metrics",
		BatchID: "00000000-0000-4000-8000-000000000000",
		Samples: []protocol.Sample{},
	})
	if err != nil {
		return nil, err
	}
	var chunks [][]protocol.Sample
	var cur []protocol.Sample
	curBytes := len(empty)
	for i, s := range samples {
		extra := sizes[i]
		if len(cur) > 0 {
			extra++
		}
		if len(cur) > 0 && (len(cur)+1 > protocol.MaxSamples || curBytes+extra > maxMetricsBytes) {
			chunks = append(chunks, cur)
			cur = nil
			curBytes = len(empty)
			extra = sizes[i]
		}
		cur = append(cur, s)
		curBytes += extra
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return fitFrames(chunks)
}

func fitFrames(chunks [][]protocol.Sample) ([][]protocol.Sample, error) {
	id := "00000000-0000-4000-8000-000000000000"
	var out [][]protocol.Sample
	for _, ch := range chunks {
		for len(ch) > 0 {
			raw, err := protocol.Marshal(protocol.Metrics{
				V: protocol.Version, Type: "metrics", BatchID: id, Samples: ch,
			})
			if err != nil {
				return nil, err
			}
			if len(raw) <= maxMetricsBytes || len(ch) == 1 {
				if len(raw) > protocol.MaxFrame {
					return nil, errors.New("metrics sample exceeds frame cap")
				}
				out = append(out, ch)
				break
			}
			best := 1
			lo, hi := 1, len(ch)-1
			for lo <= hi {
				mid := (lo + hi) / 2
				raw, err = protocol.Marshal(protocol.Metrics{
					V: protocol.Version, Type: "metrics", BatchID: id, Samples: ch[:mid],
				})
				if err != nil {
					return nil, err
				}
				if len(raw) <= maxMetricsBytes {
					best = mid
					lo = mid + 1
				} else {
					hi = mid - 1
				}
			}
			out = append(out, append([]protocol.Sample(nil), ch[:best]...))
			ch = ch[best:]
		}
	}
	return out, nil
}
