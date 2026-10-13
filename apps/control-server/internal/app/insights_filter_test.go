package app

import (
	"reflect"
	"testing"
)

func TestFilterInsightCandidatesByTypes(t *testing.T) {
	candidates := []pendingInsight{
		{Type: insightBug, Title: "bug"},
		{Type: insightFeature, Title: "feature"},
		{Type: insightBug, Title: "another bug"},
	}

	got := filterInsightCandidatesByTypes(candidates, []string{insightBug})
	want := []pendingInsight{candidates[0], candidates[2]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered candidates = %#v, want %#v", got, want)
	}
	if got := filterInsightCandidatesByTypes(candidates, nil); !reflect.DeepEqual(got, candidates) {
		t.Fatalf("empty type scope changed candidates: %#v", got)
	}
}
