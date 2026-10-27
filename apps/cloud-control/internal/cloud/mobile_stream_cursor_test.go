package cloud

import (
	"net/http/httptest"
	"testing"
)

func TestMobileStreamResumeCursor(t *testing.T) {
	for _, tc := range []struct {
		name, query, header string
		sequence            int64
		resume              bool
	}{
		{"new subscription", "", "", 0, false},
		{"zero header resumes first events", "", "0", 0, true},
		{"zero query resumes first events", "?after=0", "", 0, true},
		{"header resumes", "", "12", 12, true},
		{"newest cursor wins", "?after=15", "12", 15, true},
		{"zero header does not rewind", "?after=15", "0", 15, true},
		{"invalid input starts live", "?after=-1", "invalid", 0, false},
		{"invalid header preserves query", "?after=8", "-1", 8, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/stream"+tc.query, nil)
			if tc.header != "" {
				r.Header.Set("Last-Event-ID", tc.header)
			}
			sequence, resume := mobileStreamResumeCursor(r)
			if sequence != tc.sequence || resume != tc.resume {
				t.Fatalf("cursor=(%d,%v), want (%d,%v)", sequence, resume, tc.sequence, tc.resume)
			}
		})
	}
}
