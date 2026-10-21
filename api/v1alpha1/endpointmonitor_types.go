package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HttpJsonCheck defines expected JSON field values from a HTTP response
type HttpJsonCheck struct {
	// +kubebuilder:validation:XValidation:rule="self == 0 || (self >= 100 && self <= 599)",message="must be an HTTP status code from 100 to 599"
	ExpectedStatusCode int               `json:"expectedStatusCode,omitempty"` // optional override for status code check
	JsonAssertions     map[string]string `json:"jsonAssertions"`               // key: JSONPath-like dot string, value: expected value
}

// HttpCheck defines expected values from a HTTP response
type HttpCheck struct {
	BodyContains string `json:"bodyContains,omitempty"` // optional string to match in the first 1 MiB of the body
}

type SmtpCheck struct {
	Helo            string    `json:"helo,omitempty"`
	Tls             bool      `json:"tls,omitempty"`
	StartTls        bool      `json:"startTls,omitempty"`
	EmailSecretRef  SecretRef `json:"emailSecretRef,omitempty"`
	VerifyAssertion string    `json:"verifyAssertion,omitempty"`
	FromAssertion   string    `json:"fromAssertion,omitempty"`
	ToAssertion     string    `json:"toAssertion,omitempty"`
}

type DnsCheck struct {
	Tls    bool   `json:"tls,omitempty"`    // use dns tls, 853
	Verify string `json:"verify,omitempty"` // result must match
	Server string `json:"server,omitempty"` // dns server to request from
	Type   string `json:"type,omitempty"`   // dns type to request
}

type RedisCheck struct {
	Tls       bool      `json:"tls,omitempty"`
	SecretRef SecretRef `json:"secretRef,omitempty"`
}

type TlsCheck struct {
	DaysToExpire int    `json:"daysToExpire,omitempty"` // Alert if < days left on the certificate
	Host         string `json:"host,omitempty"`         // if certificate host to check if different from endpoint name
}

type MysqlCheck struct {
	SecretRef              SecretRef `json:"secretRef,omitempty"`
	MaxSecondsBehindMaster *int      `json:"maxSecondsBehindMaster,omitempty"`
}

type PostgresCheck struct {
	SecretRef SecretRef `json:"secretRef,omitempty"`
	// Database to log in to. Defaults to postgres.
	Database string `json:"database,omitempty"`
	// MaxSecondsBehindMaster, when set, expects a streaming replica and
	// fails when it has fallen further behind than this.
	MaxSecondsBehindMaster *int `json:"maxSecondsBehindMaster,omitempty"`
}

type MongodbCheck struct {
	Tls       bool      `json:"tls,omitempty"`
	SecretRef SecretRef `json:"secretRef,omitempty"`
	// MaxSecondsBehindMaster, when set, expects a secondary and fails when
	// its oplog is further behind the primary's than this.
	MaxSecondsBehindMaster *int `json:"maxSecondsBehindMaster,omitempty"`
}

// MaxCheckInterval is the largest checkInterval, in seconds, the CRD accepts.
const MaxCheckInterval = 31536000

// EndpointMonitorSpec defines the desired state of EndpointMonitor
type EndpointMonitorSpec struct {
	// +kubebuilder:validation:Enum=http;http-json;smtp;redis;tls;tcp;dns;ping;trino;opensearch;mysql;mariadb;postgres;mongodb
	Driver   string `json:"driver"`   // ex: "opensearch", "trino", "http", "http-json"
	Endpoint string `json:"endpoint"` // target service URL
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=31536000
	CheckInterval int            `json:"checkInterval"` // in seconds
	Notify        NotifyConfig   `json:"notify"`
	HttpCheck     *HttpCheck     `json:"httpCheck,omitempty"`     // only relevant for driver = "http"
	HttpJsonCheck *HttpJsonCheck `json:"httpJsonCheck,omitempty"` // only relevant for driver = "http-json"
	SmtpCheck     *SmtpCheck     `json:"smtpCheck,omitempty"`     // only relevant for driver = "smtp"
	RedisCheck    *RedisCheck    `json:"redisCheck,omitempty"`    // only relevant for driver = "redis"
	TlsCheck      *TlsCheck      `json:"tlsCheck,omitempty"`      // only relevant for driver = "tls", "http"
	DnsCheck      *DnsCheck      `json:"dnsCheck,omitempty"`      // only relevant for driver = "dns"
	MysqlCheck    *MysqlCheck    `json:"mysqlCheck,omitempty"`    // only relevant for driver = "mysql", "mariadb"
	PostgresCheck *PostgresCheck `json:"postgresCheck,omitempty"` // only relevant for driver = "postgres"
	MongodbCheck  *MongodbCheck  `json:"mongodbCheck,omitempty"`  // only relevant for driver = "mongodb"
	// IpFamily lists the address families to connect to the endpoint over.
	// With ipv4 or ipv6 alone, every connection uses that family, connects
	// directly, ignoring HTTP proxy settings, and redirects stay on that
	// family. With both, each check runs over each family, fails if either
	// fails, and reports the slower response time. Omit the field or leave
	// it empty to leave the choice to the system. For the dns driver it
	// applies to the connection to the DNS server.
	// +optional
	IpFamily IpFamilies `json:"ipFamily,omitempty"`
}

// +kubebuilder:validation:Type=array
// +kubebuilder:validation:items:Type=string
// +kubebuilder:validation:items:Enum=ipv4;ipv6
// +kubebuilder:validation:MaxItems=2
// +kubebuilder:validation:XValidation:rule="self.all(f, self.exists_one(g, g == f))",message="must not list a family twice"
// +listType=atomic
type IpFamilies []string

func (f *IpFamilies) UnmarshalJSON(data []byte) error {
	var list []string
	if err := json.Unmarshal(data, &list); err == nil {
		*f = list
		return nil
	}
	var single string
	if err := json.Unmarshal(data, &single); err != nil {
		return err
	}
	switch single {
	case "":
		*f = nil
	case "all":
		*f = IpFamilies{"ipv4", "ipv6"}
	default:
		*f = IpFamilies{single}
	}
	return nil
}

// NotifyConfig holds notifier configurations
type NotifyConfig struct {
	Slack   *SlackConfig   `json:"slack,omitempty"`
	Email   *EmailConfig   `json:"email,omitempty"`
	Discord *DiscordConfig `json:"discord,omitempty"`
	Webhook *WebhookConfig `json:"webhook,omitempty"`
	Valkey  *ValkeyConfig  `json:"valkey,omitempty"`
}

// ValkeyConfig defines Valkey status-page notifier config
type ValkeyConfig struct {
	Enabled   bool      `json:"enabled"`
	Endpoint  string    `json:"endpoint"`
	Tls       bool      `json:"tls,omitempty"`
	SecretRef SecretRef `json:"secretRef,omitempty"`
	// +kubebuilder:validation:items:Pattern=`^[^:]+$`
	Dashboards []string `json:"dashboards"`
	// +kubebuilder:validation:Pattern=`^[^:]*$`
	Name string `json:"name,omitempty"` // overrides the monitor name used as the service identity in Valkey; keep consistent across locations to aggregate the same service
	// +kubebuilder:validation:Minimum=1
	RetentionDays *int `json:"retentionDays,omitempty"`
	DB            int  `json:"db,omitempty"`
}

// WebhookConfig defines Webhook notifier config
type WebhookConfig struct {
	Enabled       bool      `json:"enabled"`
	WebhookURL    string    `json:"webhookUrl"`
	Authorization SecretRef `json:"authorization,omitempty"` // Bearer or other token needed
	ContentType   string    `json:"contentType,omitempty"`   // "application/json", "application/x-www-form-urlencoded", ...
	Contents      string    `json:"contents,omitempty"`      // template, if empty will use GET instead of POST
	// +kubebuilder:validation:items:Enum=success;failure;change
	AlertOn []string `json:"alertOn,omitempty"` // values: "success", "failure", "change"
}

// SlackConfig defines Slack notifier config
type SlackConfig struct {
	Enabled    bool   `json:"enabled"`
	WebhookURL string `json:"webhookUrl"`
	// +kubebuilder:validation:items:Enum=success;failure;change
	AlertOn []string `json:"alertOn,omitempty"` // values: "success", "failure", "change"
}

// EmailConfig sends alerts by SMTP or Amazon SES.
// +kubebuilder:validation:XValidation:rule="!has(self.emailProvider) || self.emailProvider.lowerAscii() != 'ses' || (has(self.from) && size(self.from) > 0)",message="from is required when emailProvider is ses"
type EmailConfig struct {
	Enabled bool     `json:"enabled"`
	From    string   `json:"from,omitempty"`
	To      []string `json:"to"`
	// +kubebuilder:validation:Pattern=`^([sS][mM][tT][pP]|[sS][eE][sS])?$`
	EmailProvider  string    `json:"emailProvider,omitempty"` // "smtp" (the default) or "ses", in any case
	EmailSecretRef SecretRef `json:"emailSecretRef,omitempty"`
	Host           string    `json:"host,omitempty"`    // Host:Port
	Subject        string    `json:"subject,omitempty"` // subject template
	Body           string    `json:"body,omitempty"`    // message body template
	// +kubebuilder:validation:items:Enum=success;failure;change
	AlertOn []string `json:"alertOn,omitempty"` // values: "success", "failure", "change"
}

type SecretRef struct {
	Name string `json:"name"`
}

// DiscordConfig defines Discord notifier config
type DiscordConfig struct {
	Enabled    bool   `json:"enabled"`
	WebhookURL string `json:"webhookUrl"`
	// +kubebuilder:validation:items:Enum=success;failure;change
	AlertOn []string `json:"alertOn,omitempty"` // values: "success", "failure", "change"
}

// EndpointMonitorStatus defines the observed state of EndpointMonitor
type EndpointMonitorStatus struct {
	LastCheckedTime  metav1.Time `json:"lastCheckedTime,omitempty"`
	LastStatus       string      `json:"lastStatus,omitempty"` // e.g., success/failure
	LastStatusChange metav1.Time `json:"lastStatusChange,omitempty"`
	// LastNotifiedStatus is the status whose change alert has been sent, or
	// that needed none because no notifier subscribes to change. Empty means
	// no change alert has gone out yet. Absent, as on objects written before
	// this field existed, means lastStatus counts as announced.
	// +optional
	LastNotifiedStatus *string `json:"lastNotifiedStatus,omitempty"`
	// LastNotifiedStatusChange is when the status in lastNotifiedStatus began.
	// +optional
	LastNotifiedStatusChange metav1.Time `json:"lastNotifiedStatusChange,omitempty"`
	// ObservedGeneration is the spec generation the last check ran against.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

type EndpointMonitor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EndpointMonitorSpec   `json:"spec"`
	Status EndpointMonitorStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

type EndpointMonitorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EndpointMonitor `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EndpointMonitor{}, &EndpointMonitorList{})
}
