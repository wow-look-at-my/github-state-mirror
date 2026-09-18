package webhook

import "encoding/json"

// WorkflowJobObject is the raw `workflow_job` key of a workflow_job delivery.
func WorkflowJobObject(raw json.RawMessage) (json.RawMessage, bool) {
	return namedObject(raw, func(b *deliveryObjects) json.RawMessage { return b.WorkflowJob })
}

// CheckRunObject is the raw `check_run` key of a check_run delivery, returned
func CheckRunObject(raw json.RawMessage) (json.RawMessage, bool) {
	return namedObject(raw, func(b *deliveryObjects) json.RawMessage { return b.CheckRun })
}

// WorkflowRunObject is the raw `workflow_run` key of a workflow_run delivery.
func WorkflowRunObject(raw json.RawMessage) (json.RawMessage, bool) {
	return namedObject(raw, func(b *deliveryObjects) json.RawMessage { return b.WorkflowRun })
}

type deliveryObjects struct {
	WorkflowJob json.RawMessage `json:"workflow_job"`
	CheckRun    json.RawMessage `json:"check_run"`
	WorkflowRun json.RawMessage `json:"workflow_run"`
}

func namedObject(raw json.RawMessage, pick func(*deliveryObjects) json.RawMessage) (json.RawMessage, bool) {
	var body deliveryObjects
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, false
	}
	obj := pick(&body)
	if len(obj) == 0 {
		return nil, false
	}
	return obj, true
}
