package status

import "testing"

func TestLocationFromCounts(t *testing.T) {
	tests := []struct {
		success, failure int
		want             LocationStatus
	}{
		{10, 0, LocationSuccess},
		{5, 5, LocationDegraded},
		{2, 5, LocationOutage},
	}
	for _, tc := range tests {
		if got := LocationFromCounts(tc.success, tc.failure); got != tc.want {
			t.Fatalf("LocationFromCounts(%d,%d)=%q want %q", tc.success, tc.failure, got, tc.want)
		}
	}
}

func TestAggregateFromLocations(t *testing.T) {
	tests := []struct {
		name string
		in   []LocationStatus
		want AggregateStatus
	}{
		{"unknown", nil, AggregateUnknown},
		{"all success", []LocationStatus{LocationSuccess, LocationSuccess}, AggregateOperational},
		{"one degraded", []LocationStatus{LocationDegraded, LocationSuccess}, AggregateOperational},
		{"one outage", []LocationStatus{LocationOutage, LocationSuccess}, AggregateDegraded},
		{"two non-success with one up", []LocationStatus{LocationOutage, LocationDegraded, LocationSuccess}, AggregateDegraded},
		{"two degraded with one up", []LocationStatus{LocationDegraded, LocationDegraded, LocationSuccess}, AggregateDegraded},
		{"all non-success", []LocationStatus{LocationOutage, LocationDegraded}, AggregateMajorOutage},
		{"every location degraded", []LocationStatus{LocationDegraded, LocationDegraded}, AggregateDegraded},
		{"every location in outage", []LocationStatus{LocationOutage, LocationOutage}, AggregateMajorOutage},
		{"single location up", []LocationStatus{LocationSuccess}, AggregateOperational},
		{"single location degraded", []LocationStatus{LocationDegraded}, AggregateDegraded},
		{"single location in outage", []LocationStatus{LocationOutage}, AggregateMajorOutage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AggregateFromLocations(tc.in); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
