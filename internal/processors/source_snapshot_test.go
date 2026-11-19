package processors

import (
	"strings"
	"testing"
)

// The opt-in ParadeDB generated columns must never enter the canonical
// source snapshot, or enabling BM25 would change SourceSnapshot digests and
// void existing approvals.
func TestSourceSnapshotExcludesBM25GeneratedColumns(t *testing.T) {
	t.Parallel()

	if !strings.Contains(sourceSQL, `-'search_text'-'ep_text'`) {
		t.Fatalf("source snapshot SQL must strip search_text and ep_text:\n%s", sourceSQL)
	}
}
