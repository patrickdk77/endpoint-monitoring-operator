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

	nonSuccess := 0
	outages := 0
	degraded := 0
	for _, s := range locStatuses {
		if s != LocationSuccess {
			nonSuccess++
		}
		switch s {
		case LocationOutage:
			outages++
		case LocationDegraded:
			degraded++
		}
	}

	allNonSuccess := nonSuccess == len(locStatuses)
	if allNonSuccess {
		return AggregateMajorOutage
	}
	if nonSuccess > 1 {
		return AggregateDegraded
	}
	if outages == 1 {
		return AggregateDegraded
	}
	if degraded == 1 {
		return AggregateOperational
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
