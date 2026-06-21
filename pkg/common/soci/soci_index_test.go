package soci

import "testing"

func TestNormalizePrefetchPatterns(t *testing.T) {
	got := normalizePrefetchPatterns([]string{
		"/usr/share/opensearch/bin/",
		"usr/share/opensearch/bin",
		"",
		"./usr/share/opensearch/config/",
	})
	want := []string{
		"usr/share/opensearch/bin/",
		"usr/share/opensearch/bin",
		"usr/share/opensearch/config/",
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pattern[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMatchesPrefetchPattern(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		pattern string
		want    bool
	}{
		{
			name:    "exact",
			path:    "/usr/share/opensearch/jdk/lib/modules",
			pattern: "usr/share/opensearch/jdk/lib/modules",
			want:    true,
		},
		{
			name:    "prefix",
			path:    "usr/share/opensearch/bin/opensearch",
			pattern: "usr/share/opensearch/bin/",
			want:    true,
		},
		{
			name:    "suffix",
			path:    "usr/share/opensearch/lib/opensearch.jar",
			pattern: "*.jar",
			want:    true,
		},
		{
			name:    "single segment glob",
			path:    "usr/share/opensearch/lib/opensearch.jar",
			pattern: "usr/share/opensearch/lib/*.jar",
			want:    true,
		},
		{
			name:    "single segment glob does not cross slash",
			path:    "usr/share/opensearch/plugins/security/opensearch-security.jar",
			pattern: "usr/share/opensearch/plugins/*.jar",
			want:    false,
		},
		{
			name:    "module descriptor glob",
			path:    "usr/share/opensearch/modules/reindex/plugin-descriptor.properties",
			pattern: "usr/share/opensearch/modules/*/plugin-descriptor.properties",
			want:    true,
		},
		{
			name:    "prefix miss",
			path:    "usr/share/opensearch/config/opensearch.yml",
			pattern: "usr/share/opensearch/bin/",
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesPrefetchPattern(tt.path, tt.pattern); got != tt.want {
				t.Fatalf("matchesPrefetchPattern() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSelectPrefetchSpansForLargeArchiveUsesHeadAndTail(t *testing.T) {
	got := selectPrefetchSpans("usr/share/opensearch/jdk/lib/modules", 10, 109, 16, 8)
	if len(got) != 16 {
		t.Fatalf("len = %d, want 16", len(got))
	}
	wantIDs := []int{10, 11, 12, 13, 14, 15, 16, 17, 102, 103, 104, 105, 106, 107, 108, 109}
	for i, want := range wantIDs {
		if int(got[i].id) != want {
			t.Fatalf("span[%d] = %d, want %d", i, got[i].id, want)
		}
		if got[i].priority != prefetchPrioritySync {
			t.Fatalf("span[%d] priority = %d, want sync", i, got[i].priority)
		}
	}
}

func TestSelectPrefetchSpansForArchiveWarmsMiddleAsync(t *testing.T) {
	got := selectPrefetchSpans("usr/share/opensearch/jdk/lib/modules", 10, 44, 64, 8)
	if len(got) != 35 {
		t.Fatalf("len = %d, want 35", len(got))
	}
	for i := 0; i < 8; i++ {
		if got[i].priority != prefetchPrioritySync {
			t.Fatalf("head span[%d] priority = %d, want sync", i, got[i].priority)
		}
	}
	for i := 8; i < 16; i++ {
		if got[i].priority != prefetchPrioritySync {
			t.Fatalf("tail span[%d] priority = %d, want sync", i, got[i].priority)
		}
	}
	for i := 16; i < len(got); i++ {
		if got[i].priority != prefetchPriorityAsync {
			t.Fatalf("middle span[%d] priority = %d, want async", i, got[i].priority)
		}
	}
}

func TestSelectPrefetchSpansForNonArchiveKeepsFirstSyncOnly(t *testing.T) {
	got := selectPrefetchSpans("usr/share/opensearch/plugins/opensearch-ml/model.bin", 5, 20, 4, 8)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	for i, span := range got {
		if int(span.id) != 5+i {
			t.Fatalf("span[%d] = %d, want %d", i, span.id, 5+i)
		}
		wantPriority := prefetchPriorityAsync
		if i == 0 {
			wantPriority = prefetchPrioritySync
		}
		if span.priority != wantPriority {
			t.Fatalf("span[%d] priority = %d, want %d", i, span.priority, wantPriority)
		}
	}
}

func TestSelectPrefetchSpansUsesArchiveEdgeOption(t *testing.T) {
	got := selectPrefetchSpans("usr/share/opensearch/jdk/lib/modules", 10, 109, 12, 3)
	if len(got) != 12 {
		t.Fatalf("len = %d, want 12", len(got))
	}
	wantSync := []int{10, 11, 12, 107, 108, 109}
	for i, want := range wantSync {
		if int(got[i].id) != want {
			t.Fatalf("sync span[%d] = %d, want %d", i, got[i].id, want)
		}
		if got[i].priority != prefetchPrioritySync {
			t.Fatalf("span[%d] priority = %d, want sync", i, got[i].priority)
		}
	}
	for i := len(wantSync); i < len(got); i++ {
		if got[i].priority != prefetchPriorityAsync {
			t.Fatalf("span[%d] priority = %d, want async", i, got[i].priority)
		}
	}
}

func TestSelectPrefetchSpansKeepsJarSyncEdgeSmall(t *testing.T) {
	got := selectPrefetchSpans("usr/share/opensearch/lib/opensearch.jar", 10, 19, 10, 4)
	if len(got) != 10 {
		t.Fatalf("len = %d, want 10", len(got))
	}
	if int(got[0].id) != 10 || got[0].priority != prefetchPrioritySync {
		t.Fatalf("first span = (%d,%d), want first sync", got[0].id, got[0].priority)
	}
	if int(got[1].id) != 19 || got[1].priority != prefetchPrioritySync {
		t.Fatalf("tail span = (%d,%d), want tail sync", got[1].id, got[1].priority)
	}
	for i := 2; i < len(got); i++ {
		if got[i].priority != prefetchPriorityAsync {
			t.Fatalf("span[%d] priority = %d, want async", i, got[i].priority)
		}
	}
}

func TestNormalizePrefetchSpansKeepsPrioritySeparate(t *testing.T) {
	got := normalizePrefetchSpans([]PrefetchSpan{
		{StartSpan: 1, EndSpan: 1, Priority: prefetchPrioritySync},
		{StartSpan: 2, EndSpan: 2, Priority: prefetchPrioritySync},
		{StartSpan: 3, EndSpan: 3, Priority: prefetchPriorityAsync},
		{StartSpan: 4, EndSpan: 4, Priority: prefetchPriorityAsync},
	})
	want := []PrefetchSpan{
		{StartSpan: 1, EndSpan: 2, Priority: prefetchPrioritySync},
		{StartSpan: 3, EndSpan: 4, Priority: prefetchPriorityAsync},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("span[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}
