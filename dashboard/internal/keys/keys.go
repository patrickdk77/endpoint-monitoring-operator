package keys

import "fmt"

func Dashboards() string { return "emo:dashboards" }

func Services(dash string) string { return fmt.Sprintf("emo:%s:svcs", dash) }

func Meta(dash, svc string) string { return fmt.Sprintf("emo:%s:%s:meta", dash, svc) }

func StatusHour(dash, svc string, hourEpoch int64) string {
	return fmt.Sprintf("emo:%s:%s:s:%d", dash, svc, hourEpoch)
}

func DetailHour(dash, svc string, hourEpoch int64) string {
	return fmt.Sprintf("emo:%s:%s:d:%d", dash, svc, hourEpoch)
}

func RollupLocationHour(dash, svc, loc string) string {
	return fmt.Sprintf("emo:%s:%s:rollup:%s:hour", dash, svc, loc)
}

func RollupAggregateHour(dash, svc string) string {
	return fmt.Sprintf("emo:%s:%s:rollup:agg:hour", dash, svc)
}

func RollupDaily(dash, svc string) string {
	return fmt.Sprintf("emo:%s:%s:rollup:daily", dash, svc)
}
