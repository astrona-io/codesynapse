package gateway

import (
	"bytes"
	"encoding/json"
	"mime"
)

// Bounds of the usage reader. An SSE line holding usage (message_start,
// message_delta) is a few hundred bytes; longer lines are text deltas and
// are skipped. A non-streamed answer is read for its usage only while it
// fits maxUsageBody, so a huge answer costs no more than that.
const (
	maxUsageLine = 16 << 10
	maxUsageBody = 1 << 20
)

// cloudUsage reads the token usage out of an Anthropic answer as it passes
// through the gateway. It keeps at most one line (streamed) or
// maxUsageBody bytes (not streamed), reads only the usage numbers and never
// the text, and gives up quietly on anything it does not understand: the
// answer itself is never changed or held back.
type cloudUsage struct {
	sse  bool
	buf  []byte
	skip bool // the current SSE line ran past maxUsageLine
	over bool // the non-streamed answer ran past maxUsageBody

	in, cached, cacheWrite, out int
	seen                        bool
}

// anthropicUsage is the usage block of an answer, message_start or
// message_delta event.
type anthropicUsage struct {
	Input      int `json:"input_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
	Output     int `json:"output_tokens"`
}

// newCloudUsage returns a reader for an answer of contentType, or nil when
// the answer carries no usage it can read.
func newCloudUsage(contentType string) *cloudUsage {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil
	}
	switch mt {
	case "text/event-stream":
		return &cloudUsage{sse: true}
	case "application/json":
		return &cloudUsage{}
	}
	return nil
}

// Write takes the next piece of the answer.
func (c *cloudUsage) Write(b []byte) {
	if c == nil {
		return
	}
	if !c.sse {
		if c.over {
			return
		}
		if len(c.buf)+len(b) > maxUsageBody {
			c.over, c.buf = true, nil
			return
		}
		c.buf = append(c.buf, b...)
		return
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		part := b
		if i >= 0 {
			part = b[:i]
		}
		if !c.skip {
			if len(c.buf)+len(part) > maxUsageLine {
				c.skip, c.buf = true, c.buf[:0]
			} else {
				c.buf = append(c.buf, part...)
			}
		}
		if i < 0 {
			return
		}
		if !c.skip {
			c.line(c.buf)
		}
		c.buf, c.skip, b = c.buf[:0], false, b[i+1:]
	}
}

// line reads one SSE line; only data lines that mention usage are decoded.
func (c *cloudUsage) line(l []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimRight(l, "\r"), []byte("data:"))
	if !ok || !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	var ev struct {
		Message struct {
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(bytes.TrimSpace(data), &ev) != nil {
		return
	}
	c.add(ev.Message.Usage)
	c.add(ev.Usage)
}

// add keeps the largest value seen for each kind: message_delta repeats
// message_start's counts (or leaves them out) and carries the final output.
func (c *cloudUsage) add(u *anthropicUsage) {
	if u == nil {
		return
	}
	c.in, c.cached = max(c.in, u.Input), max(c.cached, u.CacheRead)
	c.cacheWrite, c.out = max(c.cacheWrite, u.CacheWrite), max(c.out, u.Output)
	c.seen = c.seen || u.Input+u.CacheRead+u.CacheWrite+u.Output > 0
}

// finish reads a non-streamed answer once it is complete, and the last SSE
// line when the stream ended without a newline.
func (c *cloudUsage) finish() {
	if c == nil {
		return
	}
	if c.sse {
		if !c.skip && len(c.buf) > 0 {
			c.line(c.buf)
		}
		c.buf = nil
		return
	}
	if !c.over && len(c.buf) > 0 {
		var v struct {
			Usage *anthropicUsage `json:"usage"`
		}
		if json.Unmarshal(c.buf, &v) == nil {
			c.add(v.Usage)
		}
	}
	c.buf = nil
}

// note copies what was read into m.
func (c *cloudUsage) note(m *Meta) {
	if c == nil || !c.seen {
		return
	}
	m.CloudIn, m.CloudCached, m.CloudCacheWrite, m.CloudOut, m.CloudMetered = c.in, c.cached, c.cacheWrite, c.out, true
}
