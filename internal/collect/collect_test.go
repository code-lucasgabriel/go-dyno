package collect

import (
	"strings"
	"testing"
)

func TestProm(t *testing.T) {
	d := ParseProm(strings.NewReader(`# HELP x
# TYPE http_requests_total counter
http_requests_total{handler="/a",code="200"} 10
http_requests_total{handler="/a",code="500"} 2
http_requests_total{handler="/b",code="200"} 5 1700000000
process_cpu_seconds_total 3.5
odd{label="has space inside"} 7
`))
	if got := PromSum(d, "http_requests_total", nil); got != 17 {
		t.Errorf("sum = %v", got)
	}
	if got := PromSum(d, "http_requests_total", map[string]string{"handler": "/a"}); got != 12 {
		t.Errorf("filtered = %v", got)
	}
	if got := PromSum(d, "process_cpu_seconds_total", nil); got != 3.5 {
		t.Errorf("cpu = %v", got)
	}
	if got := PromSum(d, "odd", nil); got != 7 {
		t.Errorf("label with space = %v", got)
	}
}
