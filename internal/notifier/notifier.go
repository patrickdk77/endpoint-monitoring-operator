package notifier

type Notifier interface {
	SendAlert(status string, values *NoticeValues) error
}

type NoticeValues struct {
	Status           string
	Endpoint         string
	Driver           string
	Healthy          string
	Name             string
	Response         string
	LastStatus       string
	LastChecked      string
	LastStatusChange string
	CurrentTime      string
	StatusTime       string
	Message          string
	AlertMessage     string
}

// ShouldAlert returns true if the given status is in the allowed list, or if the list is empty and status is "failure".
func ShouldAlert(allowed []string, status string) bool {
	if len(allowed) == 0 {
		// Default to alert only on failure
		return status == "failure"
	}
	for _, a := range allowed {
		if a == status {
			return true
		}
	}
	return false
}
