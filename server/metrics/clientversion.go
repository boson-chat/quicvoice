package metrics

import (
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	// Distinct client.version values reported before the rest become "other".
	maxClientVersions = 32
	// Longer versions are truncated.
	maxClientVersionLen = 48

	clientVersionUnknown = "unknown"
	clientVersionOther   = "other"
)

// clientVersions maps the free-form Hello.client_version to a bounded set of
// attribute values: the string is normalised, and only the first
// maxClientVersions distinct values get their own series.
type clientVersions struct {
	mu    sync.Mutex
	limit int
	seen  map[string]metric.MeasurementOption
	other metric.MeasurementOption
}

func newClientVersions(limit int) *clientVersions {
	c := &clientVersions{limit: limit, seen: map[string]metric.MeasurementOption{}}
	c.other = clientVersionAttr(clientVersionOther)
	c.seen[clientVersionOther] = c.other
	c.seen[clientVersionUnknown] = clientVersionAttr(clientVersionUnknown)
	return c
}

func clientVersionAttr(v string) metric.MeasurementOption {
	return attrOpt(attribute.String("client.version", v))
}

func (c *clientVersions) attr(raw string) metric.MeasurementOption {
	v := normaliseClientVersion(raw)
	c.mu.Lock()
	defer c.mu.Unlock()
	if opt, ok := c.seen[v]; ok {
		return opt
	}
	// +2: "other" and "unknown" are preloaded and don't use up slots.
	if len(c.seen) >= c.limit+2 {
		return c.other
	}
	opt := clientVersionAttr(v)
	c.seen[v] = opt
	return opt
}

// normaliseClientVersion lowercases v, keeps [a-z0-9._/+-] and truncates it.
func normaliseClientVersion(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(v)) {
		if b.Len() >= maxClientVersionLen {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', strings.ContainsRune("._/+-", r):
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return clientVersionUnknown
	}
	return b.String()
}
