package spanmanager

import (
	"bytes"
	"testing"

	"github.com/cloudpilot-ai/hermes/pkg/common/soci/ztoc/compression"
)

func TestHotCacheStoresAndEvictsBoundedSpans(t *testing.T) {
	m := &SpanManager{}
	m.EnableHotCache(2)

	m.addHotSpan(1, []byte("one"))
	m.addHotSpan(2, []byte("two"))

	if got, ok := m.getHotSpan(1); !ok || !bytes.Equal(got, []byte("one")) {
		t.Fatalf("span 1 = %q, %t; want one, true", got, ok)
	}

	m.addHotSpan(3, []byte("three"))
	if _, ok := m.getHotSpan(1); ok {
		t.Fatalf("span 1 remained after bounded eviction")
	}
	if got, ok := m.getHotSpan(2); !ok || !bytes.Equal(got, []byte("two")) {
		t.Fatalf("span 2 = %q, %t; want two, true", got, ok)
	}
	if got, ok := m.getHotSpan(3); !ok || !bytes.Equal(got, []byte("three")) {
		t.Fatalf("span 3 = %q, %t; want three, true", got, ok)
	}
}

func TestHotCacheCanBeDisabled(t *testing.T) {
	m := &SpanManager{}
	m.EnableHotCache(1)
	m.addHotSpan(compression.SpanID(7), []byte("seven"))

	m.EnableHotCache(0)
	if _, ok := m.getHotSpan(compression.SpanID(7)); ok {
		t.Fatalf("span remained after disabling hot cache")
	}
}
