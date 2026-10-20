package driver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/internal/version"
)

type OpenSearchDriver struct {
	endpoint string
	client   *http.Client
}

type OpenSearchClusterHealth struct {
	ClusterName                 string  `json:"cluster_name"`
	Status                      string  `json:"status"`
	TimedOut                    bool    `json:"timed_out"`
	NumberOfNodes               int     `json:"number_of_nodes"`
	NumberOfDataNodes           int     `json:"number_of_data_nodes"`
	ActivePrimaryShards         int     `json:"active_primary_shards"`
	ActiveShards                int     `json:"active_shards"`
	RelocatingShards            int     `json:"relocating_shards"`
	InitializingShards          int     `json:"initializing_shards"`
	UnassignedShards            int     `json:"unassigned_shards"`
	DelayedUnassignedShards     int     `json:"delayed_unassigned_shards"`
	NumberOfPendingTasks        int     `json:"number_of_pending_tasks"`
	NumberOfInFlightFetch       int     `json:"number_of_in_flight_fetch"`
	TaskMaxWaitingInQueueMillis int     `json:"task_max_waiting_in_queue_millis"`
	ActiveShardsPercentAsNumber float64 `json:"active_shards_percent_as_number"`
}

func NewOpenSearchDriver(endpoint, family string) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}

	endpoint = strings.TrimSuffix(endpoint, "/")

	return &OpenSearchDriver{
		endpoint: endpoint,
		client:   httpClient(family, 30*time.Second),
	}, nil
}

func (o *OpenSearchDriver) Check() (*CheckResult, error) {
	start := time.Now()

	healthURL := o.endpoint + "/_cluster/health"

	req, err := http.NewRequest(http.MethodGet, healthURL, nil)
	var resp *http.Response
	if err == nil {
		req.Header.Set("User-Agent", version.UserAgent)
		resp, err = o.client.Do(req)
	}

	duration := time.Since(start)

	result := &CheckResult{
		ResponseTime: duration,
	}

	if resp != nil {
		defer resp.Body.Close()
	}

	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("OpenSearch check failed: %v", err)
		return result, nil
	}

	if resp.StatusCode != 200 {
		result.Success = false
		result.Message = fmt.Sprintf("OpenSearch check failed (status: %d)", resp.StatusCode)
		return result, nil
	}

	var health OpenSearchClusterHealth
	body, err := readBody(resp.Body)
	if err == nil {
		err = json.NewDecoder(bytes.NewReader(body)).Decode(&health)
	}
	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("OpenSearch check failed to parse response: %v", err)
		return result, nil
	}

	switch strings.ToLower(health.Status) {
	case "green":
		result.Success = true
		result.Message = fmt.Sprintf("OpenSearch cluster is healthy (status: %s, nodes: %d, active_shards: %d, response time: %v)",
			health.Status, health.NumberOfNodes, health.ActiveShards, duration)
	case "yellow":
		result.Success = false
		result.Message = fmt.Sprintf("OpenSearch cluster has issues (status: %s)", health.Status)
	case "red":
		result.Success = false
		result.Message = fmt.Sprintf("OpenSearch cluster is unhealthy (status: %s)", health.Status)
	default:
		result.Success = false
		result.Message = fmt.Sprintf("OpenSearch cluster status unknown (status: %s)", health.Status)
	}

	return result, nil
}

func (o *OpenSearchDriver) GetEndpoint() string {
	return o.endpoint
}

func (o *OpenSearchDriver) GetType() string {
	return "opensearch"
}
