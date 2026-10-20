package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestIpFamilyDecodesListAndOldStringForm(t *testing.T) {
	tests := []struct {
		json string
		want IpFamilies
	}{
		{`{}`, nil},
		{`{"ipFamily":null}`, nil},
		{`{"ipFamily":[]}`, IpFamilies{}},
		{`{"ipFamily":["ipv4"]}`, IpFamilies{"ipv4"}},
		{`{"ipFamily":["ipv6","ipv4"]}`, IpFamilies{"ipv6", "ipv4"}},
		{`{"ipFamily":"ipv4"}`, IpFamilies{"ipv4"}},
		{`{"ipFamily":"ipv6"}`, IpFamilies{"ipv6"}},
		{`{"ipFamily":"all"}`, IpFamilies{"ipv4", "ipv6"}},
		{`{"ipFamily":""}`, nil},
	}
	for _, tc := range tests {
		var spec EndpointMonitorSpec
		if err := json.Unmarshal([]byte(tc.json), &spec); err != nil {
			t.Errorf("%s: %v", tc.json, err)
			continue
		}
		if !reflect.DeepEqual(spec.IpFamily, tc.want) {
			t.Errorf("%s: got %#v, want %#v", tc.json, spec.IpFamily, tc.want)
		}
	}

	for _, bad := range []string{`{"ipFamily":7}`, `{"ipFamily":["ipv4",7]}`, `{"ipFamily":{"ipv4":true}}`} {
		var spec EndpointMonitorSpec
		if err := json.Unmarshal([]byte(bad), &spec); err == nil {
			t.Errorf("%s: decoded to %#v, want an error", bad, spec.IpFamily)
		}
	}
}

func TestIpFamilyEncodesAsAList(t *testing.T) {
	data, err := json.Marshal(EndpointMonitorSpec{IpFamily: IpFamilies{"ipv4", "ipv6"}})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw["ipFamily"], []any{"ipv4", "ipv6"}) {
		t.Fatalf("ipFamily encoded as %#v", raw["ipFamily"])
	}
}
