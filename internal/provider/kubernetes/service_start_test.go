package kubernetes

import "testing"

func TestServiceAcknowledgementRequiresSingleSuccess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  string
		valid bool
	}{
		{"success", "{\"exit_code\":0}\n", true},
		{"agent error", "{\"error\":\"rejected\"}\n", false},
		{"nonzero", "{\"exit_code\":1}\n", false},
		{"extra frame", "{\"exit_code\":0}\n{\"exit_code\":0}\n", false},
		{"missing result", "{}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := serviceAcknowledged([]byte(tc.data))
			if (err == nil) != tc.valid {
				t.Fatalf("acknowledgement result: %v", err)
			}
		})
	}
}
