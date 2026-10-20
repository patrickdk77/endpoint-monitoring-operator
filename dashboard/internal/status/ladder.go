package status

// LocationStatus is stage-1 status for one location in one hour bucket.
type LocationStatus string

const (
	LocationSuccess  LocationStatus = "success"
	LocationDegraded LocationStatus = "degraded"
	LocationOutage   LocationStatus = "outage"
)

// AggregateStatus is stage-2 cross-location status.
type AggregateStatus string

const (
	AggregateUnknown     AggregateStatus = "unknown"
	AggregateOperational AggregateStatus = "operational"
	AggregateDegraded    AggregateStatus = "degraded"
	AggregateMajorOutage AggregateStatus = "major_outage"
)

func LocationFromCounts(successes, failures int) LocationStatus {
	if failures == 0 {
		return LocationSuccess
	}
	if failures > successes {
		return LocationOutage
	}
	return LocationDegraded
}

func AggregateFromLocations(locStatuses []LocationStatus) AggregateStatus {
	if len(locStatuses) == 0 {
		return AggregateUnknown
	}

	nonSuccess, outages := 0, 0
	for _, s := range locStatuses {
		if s != LocationSuccess {
			nonSuccess++
		}
		if s == LocationOutage {
			outages++
		}
	}

	switch {
	case nonSuccess == len(locStatuses) && outages > 0:
		return AggregateMajorOutage
	case outages > 0, nonSuccess > 1, nonSuccess == len(locStatuses):
		return AggregateDegraded
	}
	return AggregateOperational
}

func WorstAggregate(a, b AggregateStatus) AggregateStatus {
	rank := map[AggregateStatus]int{
		AggregateUnknown:     0,
		AggregateOperational: 1,
		AggregateDegraded:    2,
		AggregateMajorOutage: 3,
	}
	if rank[a] >= rank[b] {
		return a
	}
	return b
}
